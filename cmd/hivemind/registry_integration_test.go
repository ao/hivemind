//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/server"
)

// runIn runs a command with stdin.
func (c *cli) runIn(stdin string, args ...string) result {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Env = c.env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			c.t.Fatalf("hivemind %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return result{out.String(), errb.String(), code}
}

// engine sends one request to the Docker daemon at host, a unix socket.
func engine(t *testing.T, host, method, path string) (int, string) {
	t.Helper()
	sock := strings.TrimPrefix(host, "unix://")
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	req, _ := http.NewRequest(method, "http://docker/v"+docker.APIVersion+path, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// rawPull asks the daemon to pull image, with auth if it is not nil, and
// returns the status and the body as the daemon sent them.
func rawPull(t *testing.T, host, image string, auth *runtime.RegistryAuth) (int, string) {
	t.Helper()
	sock := strings.TrimPrefix(host, "unix://")
	hc := &http.Client{Timeout: time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	name, tag, _ := strings.Cut(image[strings.LastIndex(image, "/"):], ":")
	req, _ := http.NewRequest(http.MethodPost, "http://docker/v"+docker.APIVersion+"/images/create?fromImage="+
		image[:strings.LastIndex(image, "/")]+name+"&tag="+tag, nil)
	if auth != nil {
		h, err := docker.EncodeRegistryAuth(*auth)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Registry-Auth", h)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// removeImage deletes the image's tag from the daemon, as docker rmi -f
// does: a container that runs it keeps running, and the next replica must
// pull.
func removeImage(t *testing.T, host, image string) {
	t.Helper()
	code, body := engine(t, host, http.MethodDelete, "/images/"+image+"?force=1")
	if code != http.StatusOK && code != http.StatusNotFound {
		t.Fatalf("removing %s: %d %s", image, code, body)
	}
}

// TestCLIPrivateRegistry pulls from a registry with htpasswd auth and TLS
// from a CA of its own, which scripts/integration-docker.sh starts: no login
// leaves the replica pending with the login message, a login runs it, a
// logout makes the next replica's pull fail again, and a wrong password gives
// the message too. Neither password is in any log line, status or reply.
func TestCLIPrivateRegistry(t *testing.T) {
	registry := os.Getenv("HIVEMIND_IT_REGISTRY")
	image := os.Getenv("HIVEMIND_IT_REGISTRY_IMAGE")
	user := os.Getenv("HIVEMIND_IT_REGISTRY_USER")
	password := os.Getenv("HIVEMIND_IT_REGISTRY_PASSWORD")
	if registry == "" || image == "" || user == "" || password == "" {
		t.Fatal("HIVEMIND_IT_REGISTRY* are not set: run scripts/integration-docker.sh")
	}
	wrong := "wrong-" + password[:8]
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
	port := freePort(t)
	c := &cli{t: t, bin: bin, port: port, env: []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"HIVEMIND_DATA_DIR=" + filepath.Join(tmp, "data"),
		"HIVEMIND_API_ADDR=127.0.0.1:" + strconv.Itoa(port),
		"HIVEMIND_DOCKER_HOST=" + host,
		"HIVEMIND_RECONCILE_INTERVAL=1s",
	}}
	c.ok("init")
	srv, srvOut := c.serve()
	var outputs []string // every CLI output, for the password check
	ok := func(args ...string) string {
		t.Helper()
		out := c.ok(args...)
		outputs = append(outputs, out)
		return out
	}

	// replica returns the status of one replica of priv.
	replica := func(idx int) (server.ReplicaStatus, bool) {
		a, found := find(c.status(), "priv")
		if !found {
			return server.ReplicaStatus{}, false
		}
		for _, r := range a.Replicas {
			if r.Index == idx {
				return r, true
			}
		}
		return server.ReplicaStatus{}, false
	}
	waitError := func(idx int, want string) server.ReplicaStatus {
		t.Helper()
		var r server.ReplicaStatus
		waitFor(t, 2*time.Minute, func() bool {
			var found bool
			r, found = replica(idx)
			return found && r.State == "pending" && strings.Contains(r.Error, want)
		}, fmt.Sprintf("replica %d to fail its pull with %q", idx, want), srvOut)
		return r
	}

	removeImage(t, host, image)
	// What the daemon says, for the record: without a credential, and with
	// a wrong one.
	for _, auth := range []*runtime.RegistryAuth{nil, {Username: user, Password: wrong, ServerAddress: registry}} {
		code, body := rawPull(t, host, image, auth)
		t.Logf("the daemon's answer to a pull with credential %v: %d %s", auth != nil, code, strings.ReplaceAll(body, user, "<user>"))
	}
	spec := filepath.Join(tmp, "priv.json")
	writeFile(t, spec, []byte(`{"name":"priv","image":"`+image+`","command":["sleep","3600"]}`))
	ok("deploy", "-f", spec)

	// No login: the replica stays pending, and says what to run.
	r := waitError(0, "hivemind registry login "+registry)
	if !strings.Contains(r.Error, registry+" wants a credential") || strings.Contains(r.Error, user) {
		t.Fatalf("status without a login: %q", r.Error)
	}
	if n := len(containers(t, rt, "priv")); n != 0 {
		t.Fatalf("%d containers without a login", n)
	}

	// A login runs it, without waiting out the pull's backoff.
	login := func(pw string) {
		t.Helper()
		res := c.runIn(pw+"\n", "registry", "login", registry, "--username", user, "--password-stdin")
		if res.code != 0 {
			t.Fatalf("registry login: exit %d: %s", res.code, res.stderr)
		}
		outputs = append(outputs, res.stdout, res.stderr)
	}
	loggedIn := time.Now()
	login(password)
	c.waitRunning(rt, "priv", 1)
	t.Logf("replica 0 ran %s after the login", time.Since(loggedIn).Round(100*time.Millisecond))
	first := containers(t, rt, "priv")[0].ID
	if out := ok("registry", "list"); !strings.Contains(out, registry) || !strings.Contains(out, user) {
		t.Fatalf("registry list:\n%s", out)
	}

	// Logout: the running container stays, and a new replica's pull fails.
	ok("registry", "logout", registry)
	removeImage(t, host, image)
	ok("scale", "priv", "2")
	waitError(1, registry+" wants a credential")
	if r, _ := replica(0); r.State != "running" || r.ContainerID != first {
		t.Fatalf("replica 0 after the logout: %+v", r)
	}

	// A wrong password: the same kind of message, tried at once.
	login(wrong)
	r = waitError(1, registry+" refused the stored credential")
	if !strings.Contains(r.Error, "hivemind registry login "+registry) || strings.Contains(r.Error, user) {
		t.Fatalf("status with a wrong password: %q", r.Error)
	}

	// The right one again, for the second replica.
	login(password)
	c.waitRunning(rt, "priv", 2)

	// Neither password is in any reply, status or log line.
	outputs = append(outputs, c.ok("status", "--json"), c.ok("registry", "list", "--json"))
	token, err := os.ReadFile(filepath.Join(tmp, "data", "admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v1/apps", "/v1/apps/priv", "/v1/apps/priv/status", "/v1/status", "/v1/nodes", "/v1/registries", "/v1/routes"} {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, p), nil)
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", p, resp.StatusCode, b)
		}
		outputs = append(outputs, string(b))
	}
	var list server.RegistryList
	if err := json.Unmarshal([]byte(outputs[len(outputs)-2]), &list); err != nil || len(list.Registries) != 1 || list.Registries[0].Username != user {
		t.Fatalf("GET /v1/registries: %s, %v", outputs[len(outputs)-2], err)
	}
	ok("delete", "priv")
	c.waitGone(rt, "priv")
	ok("registry", "logout", registry)
	stop(t, srv, srvOut)
	outputs = append(outputs, srvOut.String())
	for _, pw := range []string{password, wrong} {
		for _, o := range outputs {
			if strings.Contains(o, pw) {
				t.Fatalf("a password is in an output, status, reply or log line:\n%s", o)
			}
		}
	}
	if !strings.Contains(srvOut.String(), "pulling image") {
		t.Fatalf("serve's log has no pull:\n%s", srvOut)
	}
}
