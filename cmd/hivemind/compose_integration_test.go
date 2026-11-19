//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
)

// TestCLICompose runs compose deploy with the real binary on a two-service
// file against a real serve and Docker daemon, checks each container's
// environment, published port and volume, and the health check from
// x-hivemind, and then runs compose delete.
func TestCLICompose(t *testing.T) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		t.Fatal("DOCKER_HOST is not set: run scripts/integration-docker.sh")
	}
	rt, err := docker.New(host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs, _ := rt.List(context.Background(), map[string]string{runtime.LabelApp: ""})
		for _, c := range cs {
			rt.Remove(context.Background(), c.ID, true)
		}
	})

	tmp := t.TempDir()
	bin := buildBinary(t, tmp)
	home := filepath.Join(tmp, "home")
	port, webPort := freePort(t), freePort(t)
	c := &cli{t: t, bin: bin, port: port, env: []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"HIVEMIND_DATA_DIR=" + filepath.Join(tmp, "data"),
		"HIVEMIND_API_ADDR=127.0.0.1:" + strconv.Itoa(port),
		"HIVEMIND_DOCKER_HOST=" + host,
		"HIVEMIND_RECONCILE_INTERVAL=1s",
		"WEB_PORT=" + strconv.Itoa(webPort),
	}}
	c.ok("init")
	srv, srvOut := c.serve()

	// WEB_PORT comes from the environment, GREETING from .env, and
	// STORE_MODE from its default.
	dir := filepath.Join(tmp, "project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "compose.yaml")
	writeFile(t, path, []byte(`services:
  web:
    image: nginx:alpine
    ports:
      - "${WEB_PORT}:80"
    environment:
      GREETING: ${GREETING}
    x-hivemind:
      health: {tcp: {port: 80}, interval: 1s, timeout: 500ms}
  store:
    image: busybox:1.36
    command: sh -c 'echo ready > /data/ready; exec sleep 3600'
    environment:
      STORE_MODE: ${STORE_MODE:-durable}
    volumes:
      - data:/data
volumes:
  data:
`))
	writeFile(t, filepath.Join(dir, ".env"), []byte("GREETING='hello from compose'\n"))

	r := c.run("compose", "deploy", "--prefix", "it", path)
	if r.code != 0 {
		t.Fatalf("compose deploy: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if want := "it-store: created, generation 1, 1 replica\nit-web: created, generation 1, 1 replica\n"; r.stdout != want {
		t.Fatalf("compose deploy stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
	if want := fmt.Sprintf("note: services.web.ports[0]: published on 127.0.0.1:%d only", webPort); !strings.Contains(r.stderr, want) {
		t.Fatalf("compose deploy stderr %q, want %q", r.stderr, want)
	}
	c.waitRunning(rt, "it-web", 1)
	c.waitRunning(rt, "it-store", 1)
	web, store := containers(t, rt, "it-web")[0], containers(t, rt, "it-store")[0]

	// web's TCP check, from x-hivemind, passes: Compose has none.
	waitFor(t, 30*time.Second, func() bool {
		a, ok := find(c.status(), "it-web")
		return ok && len(a.Replicas) == 1 && a.Replicas[0].Health == "healthy" && a.Replicas[0].Ready
	}, "it-web healthy by its tcp check", srvOut)

	// The environment, the published port, and the volume, as the engine
	// has them.
	wi, si := inspectRaw(t, host, web.ID), inspectRaw(t, host, store.ID)
	if !contains(wi.Config.Env, "GREETING=hello from compose") {
		t.Errorf("web env: %q", wi.Config.Env)
	}
	if !contains(si.Config.Env, "STORE_MODE=durable") {
		t.Errorf("store env: %q", si.Config.Env)
	}
	if b := wi.HostConfig.PortBindings["80/tcp"]; len(b) != 1 || b[0].HostIP != "127.0.0.1" || b[0].HostPort != strconv.Itoa(webPort) {
		t.Errorf("web port bindings: %+v", wi.HostConfig.PortBindings)
	}
	if m := si.Mounts; len(m) != 1 || m[0].Type != "volume" || m[0].Name != "hivemind-it-store_data" || m[0].Destination != "/data" {
		t.Errorf("store mounts: %+v", si.Mounts)
	}

	// The port answers on 127.0.0.1, and the command wrote to the volume.
	waitFor(t, 30*time.Second, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", webPort))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == 200 && strings.Contains(string(body), "nginx")
	}, "nginx on the published port", srvOut)
	waitFor(t, 30*time.Second, func() bool {
		code, err := rt.Exec(context.Background(), store.ID, []string{"test", "-f", "/data/ready"})
		return err == nil && code == 0
	}, "/data/ready in the store volume", srvOut)

	r = c.run("compose", "delete", "--prefix", "it", path)
	if want := "it-store: deleting; its containers stop on the next reconcile pass\nit-web: deleting; its containers stop on the next reconcile pass\n"; r.code != 0 || r.stdout != want {
		t.Fatalf("compose delete: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	c.waitGone(rt, "it-web")
	c.waitGone(rt, "it-store")
	if r = c.run("compose", "delete", "--prefix", "it", path); r.code != 0 || r.stdout != "it-store: not found\nit-web: not found\n" {
		t.Fatalf("second compose delete: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	stop(t, srv, srvOut)
	if strings.Contains(srvOut.String(), "hello from compose") {
		t.Fatalf("serve output holds an environment value:\n%s", srvOut.String())
	}
}

// engineInspect is the part of the engine's container inspect reply that
// the runtime package does not return.
type engineInspect struct {
	Config struct {
		Env []string `json:"Env"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

// inspectRaw reads GET /containers/{id}/json from the engine at host, a
// unix:// address.
func inspectRaw(t *testing.T, host, id string) engineInspect {
	t.Helper()
	sock := strings.TrimPrefix(host, "unix://")
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := hc.Get("http://docker/containers/" + id + "/json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out engineInspect
	if resp.StatusCode != 200 {
		t.Fatalf("inspect %s: %s", id, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
