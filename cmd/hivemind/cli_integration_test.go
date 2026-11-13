//go:build integration

// This test builds the real binary and drives every command against a
// one-node cluster on a real Docker daemon. scripts/integration-docker.sh
// starts the daemon (docker:dind) and sets DOCKER_HOST. Run it only through
// that script.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/server"
)

type cli struct {
	t    *testing.T
	bin  string
	env  []string
	port int
}

type result struct {
	stdout, stderr string
	code           int
}

// syncBuf collects a background process's output.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (c *cli) run(args ...string) result {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Env = c.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			c.t.Fatalf("hivemind %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return result{out.String(), errb.String(), code}
}

// ok runs a command that must succeed.
func (c *cli) ok(args ...string) string {
	c.t.Helper()
	r := c.run(args...)
	if r.code != 0 {
		c.t.Fatalf("hivemind %v: exit %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// fails runs a command that must exit 1 with want on stderr.
func (c *cli) fails(want string, args ...string) {
	c.t.Helper()
	r := c.run(args...)
	if r.code != 1 || !strings.Contains(r.stderr, want) {
		c.t.Fatalf("hivemind %v: exit %d, stderr %q; want exit 1 and %q", args, r.code, r.stderr, want)
	}
}

func (c *cli) start(args ...string) (*exec.Cmd, *syncBuf) {
	c.t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Env = c.env
	buf := &syncBuf{}
	cmd.Stdout, cmd.Stderr = buf, buf
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd, buf
}

// serve starts hivemind serve and waits for /healthz.
func (c *cli) serve() (*exec.Cmd, *syncBuf) {
	c.t.Helper()
	cmd, buf := c.start("serve")
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", c.port)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return cmd, buf
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.t.Fatalf("serve did not answer /healthz within 30s:\n%s", buf.String())
	return nil, nil
}

// stop sends SIGTERM and requires exit 0 within 20 s.
func stop(t *testing.T, cmd *exec.Cmd, out *syncBuf) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve exited with %v:\n%s", err, out.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("serve did not stop within 20s of SIGTERM:\n%s", out.String())
	}
}

func (c *cli) status() []server.AppStatus {
	c.t.Helper()
	var apps []server.AppStatus
	out := c.ok("status", "--json")
	if err := json.Unmarshal([]byte(out), &apps); err != nil {
		c.t.Fatalf("status --json: %v\n%s", err, out)
	}
	return apps
}

func find(apps []server.AppStatus, name string) (server.AppStatus, bool) {
	for _, a := range apps {
		if a.Name == name {
			return a, true
		}
	}
	return server.AppStatus{}, false
}

// waitRunning waits until the app has exactly n replicas, all running, and
// the engine has exactly n containers for it.
func (c *cli) waitRunning(rt *docker.Client, app string, n int) {
	c.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		a, ok := find(c.status(), app)
		cs := containers(c.t, rt, app)
		running := 0
		for _, ct := range cs {
			if ct.Running() {
				running++
			}
		}
		if ok && a.Running == n && len(a.Replicas) == n && len(cs) == n && running == n {
			return
		}
		last = fmt.Sprintf("status %+v, %d containers (%d running)", a, len(cs), running)
		time.Sleep(time.Second)
	}
	c.t.Fatalf("%s did not reach %d running replicas: %s", app, n, last)
}

// waitGone waits until the app is not in status and has no containers.
func (c *cli) waitGone(rt *docker.Client, app string) {
	c.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		_, ok := find(c.status(), app)
		if !ok && len(containers(c.t, rt, app)) == 0 {
			return
		}
		time.Sleep(time.Second)
	}
	c.t.Fatalf("%s still exists a minute after delete", app)
}

func containers(t *testing.T, rt *docker.Client, app string) []runtime.Container {
	t.Helper()
	cs, err := rt.List(context.Background(), map[string]string{runtime.LabelApp: app})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// listeners returns the local addresses of listening TCP sockets on port,
// in /proc/net/tcp hex form, such as 0100007F:1183.
func listeners(t *testing.T, port int) []string {
	t.Helper()
	var out []string
	suffix := fmt.Sprintf(":%04X", port)
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fs := strings.Fields(line)
			if len(fs) > 3 && fs[3] == "0A" && strings.HasSuffix(fs[1], suffix) {
				out = append(out, fs[1])
			}
		}
	}
	return out
}

func TestCLIOneNode(t *testing.T) {
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
	bin := filepath.Join(tmp, "hivemind")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build with CGO_ENABLED=0: %v\n%s", err, out)
	}

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
	serverURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	if out := c.ok("version"); !strings.HasPrefix(out, "hivemind ") {
		t.Fatalf("version: %q", out)
	}
	c.fails("not implemented", "join")

	// init prints the token once and refuses a second run.
	out := c.ok("init")
	token := regexp.MustCompile(`hm_[A-Za-z0-9_-]{43}`).FindString(out)
	if token == "" || strings.Count(out, token) != 1 {
		t.Fatalf("init output does not show the token once:\n%s", out)
	}
	if fi, err := os.Stat(filepath.Join(tmp, "data", "admin.token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("admin.token: %v %v", fi.Mode(), err)
	}
	c.fails("already exists", "init")

	// No server yet.
	c.fails("cannot reach hivemind at "+serverURL+": connection refused", "status")

	srv, srvOut := c.serve()
	if ls := listeners(t, port); len(ls) != 1 || !strings.HasPrefix(ls[0], "0100007F:") {
		t.Fatalf("API listeners on port %d: %v, want only 127.0.0.1", port, ls)
	}
	if out := c.ok("status"); !strings.Contains(out, "No apps.") {
		t.Fatalf("empty status: %q", out)
	}

	// Wrong tokens, from the flag and from the environment.
	c.fails("unauthorized", "status", "--token", "hm_"+strings.Repeat("A", 43))
	c.fails("unauthorized", "status", "--token", "not-a-token")
	c.env = append(c.env, "HIVEMIND_TOKEN=hm_"+strings.Repeat("B", 43))
	c.fails("unauthorized", "deploy", "--name", "web", "--image", "nginx:alpine")
	c.env = c.env[:len(c.env)-1]
	// The right token from the environment works as well as the client config.
	if r := c.run("status", "--token", token); r.code != 0 {
		t.Fatalf("status with the token flag: %+v", r)
	}

	// Deploy nginx with 2 replicas from flags.
	if out := c.ok("deploy", "--name", "web", "--image", "nginx:alpine", "--replicas", "2", "--env", "MODE=test"); !strings.HasPrefix(out, "web: created, generation 1, 2 replicas") {
		t.Fatalf("deploy: %q", out)
	}
	c.fails("invalid app spec", "deploy", "--name", "bad", "--image", "Nginx:")
	c.waitRunning(rt, "web", 2)
	table := c.ok("status")
	if n := strings.Count(table, " running "); n < 2 || !strings.Contains(table, "nginx:alpine") {
		t.Fatalf("status table shows %d running replicas:\n%s", n, table)
	}

	// Logs from each replica.
	deadline := time.Now().Add(30 * time.Second)
	for {
		out := c.ok("logs", "web", "--tail", "50")
		if strings.Contains(out, "Configuration complete") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("logs web: %q", out)
		}
		time.Sleep(time.Second)
	}
	c.ok("logs", "web", "1", "--tail", "1")
	c.fails("no container", "logs", "web", "7")
	c.fails("not found", "logs", "nope")

	// An app from a file, with a command that prints a line a second.
	spec := filepath.Join(tmp, "ticker.yaml")
	os.WriteFile(spec, []byte("name: ticker\nimage: busybox:1.36\ncommand: [sh, -c, 'i=0; while true; do echo tick $i; i=$((i+1)); sleep 1; done']\n"), 0o600)
	c.ok("deploy", "-f", spec)
	c.waitRunning(rt, "ticker", 1)

	// logs -f keeps streaming new lines until it is interrupted.
	follow, fout := c.start("logs", "-f", "ticker")
	waitFor(t, 30*time.Second, func() bool { return strings.Count(fout.String(), "tick") >= 4 }, "logs -f output", fout)
	n1 := strings.Count(fout.String(), "tick")
	time.Sleep(3 * time.Second)
	if n2 := strings.Count(fout.String(), "tick"); n2 <= n1 {
		t.Fatalf("logs -f stopped streaming: %d then %d lines", n1, n2)
	}
	follow.Process.Signal(syscall.SIGINT)
	if err := follow.Wait(); err != nil {
		t.Fatalf("logs -f after SIGINT: %v\n%s", err, fout.String())
	}

	// Scale up, then down.
	if out := c.ok("scale", "web", "3"); out != "web: 3 replicas\n" {
		t.Fatalf("scale: %q", out)
	}
	c.waitRunning(rt, "web", 3)
	c.ok("scale", "web", "1")
	c.waitRunning(rt, "web", 1)
	c.fails("between 1", "scale", "web", "0")
	c.fails("not found", "scale", "nope", "2")

	// JSON output for a single app.
	var one server.AppStatus
	if err := json.Unmarshal([]byte(c.ok("status", "web", "--json")), &one); err != nil || one.Running != 1 || one.Desired != 1 {
		t.Fatalf("status web --json: %+v %v", one, err)
	}

	if out := c.ok("delete", "web"); !strings.HasPrefix(out, "web: deleting") {
		t.Fatalf("delete: %q", out)
	}
	c.waitGone(rt, "web")
	c.fails("not found", "delete", "web")

	// SIGTERM ends an open log stream and stops serve. The container keeps
	// running, and a restarted serve neither restarts nor duplicates it.
	before := containers(t, rt, "ticker")
	follow, fout = c.start("logs", "-f", "ticker")
	waitFor(t, 30*time.Second, func() bool { return strings.Contains(fout.String(), "tick") }, "logs -f output", fout)
	stop(t, srv, srvOut)
	fdone := make(chan error, 1)
	go func() { fdone <- follow.Wait() }()
	select {
	case <-fdone:
	case <-time.After(10 * time.Second):
		t.Fatal("logs -f did not end when serve stopped")
	}
	c.fails("connection refused", "status")
	after := containers(t, rt, "ticker")
	if len(after) != 1 || !after[0].Running() || after[0].ID != before[0].ID {
		t.Fatalf("ticker after serve stopped: %+v, before %+v", after, before)
	}

	srv, srvOut = c.serve()
	time.Sleep(3 * time.Second) // a few reconcile passes
	c.waitRunning(rt, "ticker", 1)
	if again := containers(t, rt, "ticker"); again[0].ID != before[0].ID {
		t.Fatalf("restarted serve replaced the container: %s, was %s", again[0].ID, before[0].ID)
	}
	c.ok("delete", "ticker")
	c.waitGone(rt, "ticker")
	if out := c.ok("status"); !strings.Contains(out, "No apps.") {
		t.Fatalf("final status: %q", out)
	}
	stop(t, srv, srvOut)

	// The server log never holds the token or an env value.
	if s := srvOut.String(); strings.Contains(s, token) || strings.Contains(s, "MODE=test") {
		t.Fatalf("serve output contains a secret:\n%s", s)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, what string, out *syncBuf) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %q", what, out.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}
