//go:build integration

// These tests need a real Docker daemon. scripts/integration-docker.sh starts
// one (docker:dind) and sets DOCKER_HOST to its socket. Run them only through
// that script.
package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

const (
	alpineImage = "alpine:3"
	nginxImage  = "nginx:alpine"
)

func itClient(t *testing.T) *Client {
	t.Helper()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		t.Fatal("DOCKER_HOST is not set: run scripts/integration-docker.sh")
	}
	c, err := New(host)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func itCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// itName returns a name unique to this run, so tests do not collide.
func itName(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

var pulled sync.Map // image -> struct{}, so each image is pulled once per run

// itPull pulls an image once for the whole run.
func itPull(t *testing.T, c *Client, image string) {
	t.Helper()
	if _, loaded := pulled.LoadOrStore(image, struct{}{}); loaded {
		return
	}
	if err := c.Pull(itCtx(t), image); err != nil {
		pulled.Delete(image)
		t.Fatalf("pull %s: %v", image, err)
	}
}

// itCreate makes a container and removes it when the test ends.
func itCreate(t *testing.T, c *Client, cfg runtime.ContainerConfig) string {
	t.Helper()
	itPull(t, c, cfg.Image)
	id, err := c.Create(itCtx(t), cfg)
	if err != nil {
		t.Fatalf("create %s: %v", cfg.Name, err)
	}
	t.Cleanup(func() { _ = c.Remove(context.Background(), id, true) })
	return id
}

// itRawAPI calls the engine directly, bypassing the client. Tests use it to
// change state out of band.
func itRawAPI(t *testing.T, c *Client, method, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(itCtx(t), method, c.base+"/v"+APIVersion+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// itWaitState polls until the container reaches want.
func itWaitState(t *testing.T, c *Client, id, want string, timeout time.Duration) runtime.Container {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got, err := c.Inspect(itCtx(t), id)
		if err != nil {
			t.Fatalf("inspect %s: %v", id, err)
		}
		if got.State == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s: state %q after %s, want %q", id[:12], got.State, timeout, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestIntegrationInfo(t *testing.T) {
	info, err := itClient(t).Info(itCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if info.NCPU < 1 || info.MemTotal < 1<<20 || info.ServerVersion == "" {
		t.Fatalf("info = %+v", info)
	}
}

func TestIntegrationPullMissingImage(t *testing.T) {
	c := itClient(t)
	ref := itName("hivemind-no-such-image") + ":1"
	err := c.Pull(itCtx(t), ref)
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("pull %s: err = %v, want ErrNotFound", ref, err)
	}
}

func TestIntegrationCreateWithoutImage(t *testing.T) {
	c := itClient(t)
	_, err := c.Create(itCtx(t), runtime.ContainerConfig{
		Name: itName("noimg"), Image: itName("hivemind-no-such-image") + ":1", App: "noimg",
	})
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("create: err = %v, want ErrNotFound", err)
	}
}

func TestIntegrationLifecycle(t *testing.T) {
	c := itClient(t)
	ctx := itCtx(t)
	app := itName("life")
	vol := itName("vol")
	t.Cleanup(func() { itRawAPI(t, c, http.MethodDelete, "/volumes/"+vol) })

	itPull(t, c, alpineImage)
	id, err := c.Create(ctx, runtime.ContainerConfig{
		Name:    app + "-1-1",
		Image:   alpineImage,
		Command: []string{"sh", "-c", "echo $GREETING; echo kept > /data/marker; sleep 300"},
		Env:     map[string]string{"GREETING": "hello-from-env"},
		Volumes: []runtime.VolumeMount{{Name: vol, Path: "/data"}},
		App:     app,
		Replica: 1, Generation: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove(context.Background(), id, true) })

	created, err := c.Inspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "created" || created.RestartPolicy != "no" || created.Image != "docker.io/library/alpine:3" {
		t.Fatalf("after create: %+v", created)
	}
	if created.Labels[runtime.LabelApp] != app || created.Labels[runtime.LabelReplica] != "1" || created.Labels[runtime.LabelGeneration] != "1" {
		t.Fatalf("labels = %v", created.Labels)
	}

	if err := c.Start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.Start(ctx, id); err != nil {
		t.Fatalf("start of a running container: %v", err)
	}
	itWaitState(t, c, id, "running", 30*time.Second)

	listed, err := c.List(ctx, map[string]string{runtime.LabelApp: app})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != id || !listed[0].Running() {
		t.Fatalf("list by label = %+v", listed)
	}

	if err := c.Stop(ctx, id, time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopped := itWaitState(t, c, id, "exited", 30*time.Second)
	if err := c.Stop(ctx, id, time.Second); err != nil {
		t.Fatalf("stop of a stopped container: %v", err)
	}

	if err := c.Remove(ctx, id, true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := c.Inspect(ctx, id); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("inspect after remove: err = %v, want ErrNotFound", err)
	}
	if stopped.RestartPolicy != "no" {
		t.Errorf("restart policy = %q", stopped.RestartPolicy)
	}

	// Remove keeps named volumes: the engine still knows the volume.
	if code, body := itRawAPI(t, c, http.MethodGet, "/volumes/"+vol); code != http.StatusOK {
		t.Fatalf("volume %s after container removal: HTTP %d %s", vol, code, body)
	}
}

func TestIntegrationPublishedPortReachesNginx(t *testing.T) {
	c := itClient(t)
	const port = 18080
	itPull(t, c, nginxImage)
	id := itCreate(t, c, runtime.ContainerConfig{
		Name:  itName("web"),
		Image: nginxImage,
		Ports: []runtime.PortBinding{{HostPort: port, ContainerPort: 80}}, // no HostIP: loopback
		App:   "web",
	})
	if err := c.Start(itCtx(t), id); err != nil {
		t.Fatalf("start: %v", err)
	}
	itWaitState(t, c, id, "running", 30*time.Second)

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && bytes.Contains(body, []byte("Welcome to nginx")) {
				break
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: %v", url, lastErr)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The default bind must be loopback only: no listener on a wildcard address.
	listeners := itListeners(t, port)
	if !listeners["0100007F"] {
		t.Errorf("no listener on 127.0.0.1:%d, found %v", port, listeners)
	}
	if listeners["00000000"] || listeners["00000000000000000000000000000000"] {
		t.Errorf("port %d listens on a wildcard address: %v", port, listeners)
	}
}

// itListeners returns the listening addresses for a local TCP port, read from
// /proc/net/tcp and /proc/net/tcp6 in this network namespace. Each key is the
// hex address part of the entry, such as "0100007F" for 127.0.0.1.
func itListeners(t *testing.T, port int) map[string]bool {
	t.Helper()
	want := fmt.Sprintf("%04X", port)
	out := map[string]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" { // 0A is LISTEN
				continue
			}
			addr, p, ok := strings.Cut(fields[1], ":")
			if ok && p == want {
				out[addr] = true
			}
		}
	}
	return out
}

func TestIntegrationLogsReturnWhatContainerPrinted(t *testing.T) {
	c := itClient(t)
	itPull(t, c, alpineImage)

	id := itCreate(t, c, runtime.ContainerConfig{
		Name:    itName("logs"),
		Image:   alpineImage,
		Command: []string{"sh", "-c", "echo first-line; echo second-line >&2; echo third-line"},
		App:     "logs",
	})
	if err := c.Start(itCtx(t), id); err != nil {
		t.Fatal(err)
	}
	itWaitState(t, c, id, "exited", 30*time.Second)

	var out, errOut bytes.Buffer
	if err := c.Logs(itCtx(t), id, runtime.LogOptions{}, &out, &errOut); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if out.String() != "first-line\nthird-line\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if errOut.String() != "second-line\n" {
		t.Errorf("stderr = %q", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if err := c.Logs(itCtx(t), id, runtime.LogOptions{Tail: 1}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != "third-line\n" || errOut.Len() != 0 {
		t.Errorf("tail 1: stdout = %q, stderr = %q", out.String(), errOut.String())
	}

	err := c.Logs(itCtx(t), itName("no-such-container"), runtime.LogOptions{}, io.Discard, io.Discard)
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("logs of a missing container: err = %v, want ErrNotFound", err)
	}
}

func TestIntegrationFollowReturnsAtExit(t *testing.T) {
	c := itClient(t)
	itPull(t, c, alpineImage)

	id := itCreate(t, c, runtime.ContainerConfig{
		Name:    itName("follow"),
		Image:   alpineImage,
		Command: []string{"sh", "-c", "echo before; sleep 2; echo after"},
		App:     "follow",
	})
	if err := c.Start(itCtx(t), id); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := c.Logs(itCtx(t), id, runtime.LogOptions{Follow: true}, &out, io.Discard); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if out.String() != "before\nafter\n" {
		t.Errorf("followed stdout = %q", out.String())
	}
}

func TestIntegrationPortAlreadyInUse(t *testing.T) {
	c := itClient(t)
	itPull(t, c, alpineImage)
	const port = 18081

	first := itCreate(t, c, runtime.ContainerConfig{
		Name: itName("holder"), Image: alpineImage, Command: []string{"sleep", "300"},
		Ports: []runtime.PortBinding{{HostPort: port, ContainerPort: 80}}, App: "holder",
	})
	if err := c.Start(itCtx(t), first); err != nil {
		t.Fatalf("first start: %v", err)
	}
	second := itCreate(t, c, runtime.ContainerConfig{
		Name: itName("clash"), Image: alpineImage, Command: []string{"sleep", "300"},
		Ports: []runtime.PortBinding{{HostPort: port, ContainerPort: 80}}, App: "clash",
	})
	err := c.Start(itCtx(t), second)
	if !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("second start: err = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "port is already allocated") {
		t.Errorf("error does not name the cause: %v", err)
	}
	// The holder is unaffected.
	if got := itWaitState(t, c, first, "running", 10*time.Second); !got.Running() {
		t.Fatal("first container stopped")
	}
}

func TestIntegrationRemovedOutOfBand(t *testing.T) {
	c := itClient(t)
	itPull(t, c, alpineImage)
	app := itName("oob")
	id, err := c.Create(itCtx(t), runtime.ContainerConfig{
		Name: app + "-1-1", Image: alpineImage, Command: []string{"sleep", "300"}, App: app,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(itCtx(t), id); err != nil {
		t.Fatal(err)
	}

	// Remove it through the raw API, as if someone ran docker rm -f.
	if code, body := itRawAPI(t, c, http.MethodDelete, "/containers/"+id+"?force=true"); code != http.StatusNoContent {
		t.Fatalf("out-of-band remove: HTTP %d %s", code, body)
	}

	if _, err := c.Inspect(itCtx(t), id); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("inspect: err = %v, want ErrNotFound", err)
	}
	if err := c.Stop(itCtx(t), id, time.Second); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("stop: err = %v, want ErrNotFound", err)
	}
	if err := c.Start(itCtx(t), id); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("start: err = %v, want ErrNotFound", err)
	}
	if err := c.Remove(itCtx(t), id, true); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("remove: err = %v, want ErrNotFound", err)
	}
	if err := c.Logs(itCtx(t), id, runtime.LogOptions{}, io.Discard, io.Discard); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("logs: err = %v, want ErrNotFound", err)
	}
	listed, err := c.List(itCtx(t), map[string]string{runtime.LabelApp: app})
	if err != nil || len(listed) != 0 {
		t.Errorf("list after out-of-band removal = %+v, %v", listed, err)
	}
}

func TestIntegrationStopHonoursTimeout(t *testing.T) {
	c := itClient(t)
	itPull(t, c, alpineImage)
	// PID 1 ignores SIGTERM, so Stop must wait out its timeout and then kill it.
	id := itCreate(t, c, runtime.ContainerConfig{
		Name:    itName("stubborn"),
		Image:   alpineImage,
		Command: []string{"sh", "-c", "trap '' TERM; while true; do sleep 1; done"},
		App:     "stubborn",
	})
	if err := c.Start(itCtx(t), id); err != nil {
		t.Fatal(err)
	}
	itWaitState(t, c, id, "running", 30*time.Second)

	began := time.Now()
	if err := c.Stop(itCtx(t), id, 2*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	elapsed := time.Since(began)
	if elapsed < 1500*time.Millisecond || elapsed > 20*time.Second {
		t.Errorf("stop took %s, want about the 2s timeout", elapsed)
	}
	got := itWaitState(t, c, id, "exited", 10*time.Second)
	if got.ExitCode != 137 {
		t.Errorf("exit code = %d, want 137 (SIGKILL after the timeout)", got.ExitCode)
	}
}
