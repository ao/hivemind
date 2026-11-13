package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/metrics"
	"github.com/ao/hivemind/internal/runtime"
)

// memRuntime is an in-memory container engine, enough for the agent to
// create, start and list containers during serve.
type memRuntime struct {
	mu   sync.Mutex
	next int
	cs   map[string]*runtime.Container
	envs map[string]map[string]string // by container ID
}

func newMemRuntime() *memRuntime {
	return &memRuntime{cs: map[string]*runtime.Container{}, envs: map[string]map[string]string{}}
}

func (m *memRuntime) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{NCPU: 2, MemTotal: 1 << 30, ServerVersion: "mem"}, nil
}
func (m *memRuntime) Pull(context.Context, string) error { return nil }

func (m *memRuntime) Create(_ context.Context, cfg runtime.ContainerConfig) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.cs {
		if c.Name == cfg.Name {
			return "", &runtime.Error{Op: "create", Err: runtime.ErrConflict}
		}
	}
	m.next++
	id := fmt.Sprintf("%064x", m.next)
	m.cs[id] = &runtime.Container{ID: id, Name: cfg.Name, Image: cfg.Image, State: "created", Labels: map[string]string{
		runtime.LabelApp: cfg.App, runtime.LabelReplica: strconv.Itoa(cfg.Replica), runtime.LabelGeneration: strconv.Itoa(cfg.Generation)}}
	m.envs[id] = cfg.Env
	return id, nil
}

func (m *memRuntime) find(ref string) *runtime.Container {
	if c, ok := m.cs[ref]; ok {
		return c
	}
	for _, c := range m.cs {
		if c.Name == ref {
			return c
		}
	}
	return nil
}

func (m *memRuntime) Start(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.find(id)
	if c == nil {
		return &runtime.Error{Op: "start", Err: runtime.ErrNotFound}
	}
	c.State, c.StartedAt = "running", time.Now()
	return nil
}

func (m *memRuntime) Stop(_ context.Context, id string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.find(id); c != nil {
		c.State = "exited"
	}
	return nil
}

func (m *memRuntime) Remove(_ context.Context, id string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.find(id); c != nil {
		delete(m.cs, c.ID)
	}
	return nil
}

func (m *memRuntime) Inspect(_ context.Context, id string) (runtime.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.find(id)
	if c == nil {
		return runtime.Container{}, &runtime.Error{Op: "inspect", Err: runtime.ErrNotFound}
	}
	return *c, nil
}

func (m *memRuntime) List(_ context.Context, labels map[string]string) ([]runtime.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []runtime.Container
	for _, c := range m.cs {
		ok := true
		for k, v := range labels {
			if got, has := c.Labels[k]; !has || (v != "" && got != v) {
				ok = false
			}
		}
		if ok {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (m *memRuntime) Exec(context.Context, string, []string) (int, error) { return 0, nil }

func (m *memRuntime) Logs(_ context.Context, _ string, _ runtime.LogOptions, stdout, _ io.Writer) error {
	_, err := io.WriteString(stdout, "hello\n")
	return err
}

func (m *memRuntime) running(app string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.cs {
		if c.Labels[runtime.LabelApp] == app && c.State == "running" {
			n++
		}
	}
	return n
}

func unusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServeAndCLILogsHoldNoSecrets runs serve in-process at debug level, in
// text and in JSON, with the metrics listener on. It sends requests that
// carry the admin token in the header, in a cookie, in the query and in the
// path, a session cookie and CSRF tokens, deploys an app with secret env
// values through the CLI, lets the agent start it, and scrapes both metrics
// endpoints. None of the secrets may appear in the server log or on the
// CLI's stderr.
func TestServeAndCLILogsHoldNoSecrets(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) { testServeLogs(t, format) })
	}
}

func testServeLogs(t *testing.T, format string) {
	home := isolate(t)
	data := filepath.Join(home, "data")
	initOut, initErr, err := run(t, "", "init", "--data-dir", data)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.ReadAdminToken(data)
	if err != nil || !strings.Contains(initOut, token) {
		t.Fatalf("init: %v\n%s", err, initOut)
	}

	sessionID := "sessionid-" + strings.Repeat("S", 33)
	csrf := "csrftoken-" + strings.Repeat("C", 33)
	envSecret := "env-secret-" + strings.Repeat("E", 20)
	envSecret2 := "second-env-secret-" + strings.Repeat("F", 12)
	secrets := []string{token, sessionID, csrf, envSecret, envSecret2}

	apiPort, metricsPort := unusedPort(t), unusedPort(t)
	env := map[string]string{
		"HIVEMIND_DATA_DIR":           data,
		"HIVEMIND_API_ADDR":           "127.0.0.1:" + strconv.Itoa(apiPort),
		"HIVEMIND_METRICS_ADDR":       "127.0.0.1:" + strconv.Itoa(metricsPort),
		"HIVEMIND_LOG_LEVEL":          "debug",
		"HIVEMIND_LOG_FORMAT":         format,
		"HIVEMIND_RECONCILE_INTERVAL": "50ms",
		"HIVEMIND_TOKEN":              token,
	}
	cfg, err := config.Load(nil, config.Options{Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncLog{}
	log, err := newLogger(cfg, logs)
	if err != nil {
		t.Fatal(err)
	}
	rt := newMemRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, log, rt) }()
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("serve did not stop")
		}
	})

	api := "http://127.0.0.1:" + strconv.Itoa(apiPort)
	waitUntil(t, "healthz", func() bool {
		resp, err := http.Get(api + "/healthz")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil && resp.StatusCode == 200
	})

	send := func(method, url, body string, hdr map[string]string) int {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	bearer := map[string]string{"Authorization": "Bearer " + token}
	cases := []struct {
		method, path, body string
		hdr                map[string]string
		want               int
	}{
		{"GET", "/v1/apps", "", bearer, 200},
		{"GET", "/v1/apps/" + token, "", bearer, 400},
		{"GET", "/v1/apps?token=" + token + "&csrf_token=" + csrf, "", bearer, 200},
		{"GET", "/v1/apps", "", map[string]string{"Cookie": auth.SessionCookieName + "=" + sessionID}, 401},
		{"GET", "/v1/apps", "", map[string]string{"Cookie": auth.SessionCookieName + "=" + token}, 401},
		{"POST", "/v1/apps", `{"name":"x","image":"nginx"}`, map[string]string{
			"Cookie": auth.SessionCookieName + "=" + sessionID, auth.CSRFHeader: csrf}, 401},
		{"POST", "/v1/apps", "csrf_token=" + csrf, map[string]string{
			"Cookie": auth.SessionCookieName + "=" + sessionID, "Content-Type": "application/x-www-form-urlencoded"}, 401},
		{"GET", "/v1/status", "", map[string]string{"Authorization": "Bearer " + token + "x"}, 401},
		{"GET", "/v1/status", "", map[string]string{"Authorization": "Basic " + token}, 401},
		{"PUT", "/v1/apps/web2", `{"image":"nginx:alpine","env":{"API_KEY":"` + envSecret2 + `"}}`, bearer, 201},
		{"GET", "/metrics", "", nil, 401},
		{"GET", "/metrics", "", map[string]string{auth.CSRFHeader: csrf, "Cookie": "a=" + sessionID}, 401},
	}
	for _, c := range cases {
		if got := send(c.method, api+c.path, c.body, c.hdr); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}

	// The CLI paths, with the token from the environment.
	t.Setenv(envServer, api)
	t.Setenv(envToken, token)
	var cliErr strings.Builder
	cliErr.WriteString(initErr)
	for _, args := range [][]string{
		{"deploy", "--name", "web", "--image", "nginx:alpine", "--replicas", "2", "--env", "DB_PASSWORD=" + envSecret},
		{"status"},
		{"status", "web", "--json"},
		{"logs", "web"},
		{"scale", "web", "1"},
		{"status", "--token", token + "x"},
		{"deploy", "--name", "bad", "--image", "Nginx:", "--env", "K=" + envSecret},
	} {
		// main prints the returned error on stderr, so it counts as output.
		_, stderr, err := run(t, "", args...)
		cliErr.WriteString(stderr)
		if err != nil {
			cliErr.WriteString("error: " + err.Error() + "\n")
		}
	}
	waitUntil(t, "the agent to start web", func() bool { return rt.running("web") == 1 })
	rt.mu.Lock()
	sawEnv := false
	for _, e := range rt.envs {
		if e["DB_PASSWORD"] == envSecret {
			sawEnv = true
		}
	}
	rt.mu.Unlock()
	if !sawEnv {
		t.Fatal("the agent did not create a container with the env value")
	}

	// Both metrics endpoints, with the token.
	mURL := "http://127.0.0.1:" + strconv.Itoa(metricsPort) + "/metrics"
	for _, u := range []string{api + "/metrics", mURL} {
		waitUntil(t, "running replicas in "+u, func() bool {
			req, _ := http.NewRequest("GET", u, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := metrics.Parse(resp.Body)
			if err != nil {
				t.Fatalf("%s: %v", u, err)
			}
			return got[metrics.Key("hivemind_replicas_running", "app", "web")] == 1 &&
				got[metrics.Key("hivemind_reconcile_duration_seconds_count")] > 0
		})
	}
	if got := send("GET", mURL, "", nil); got != 401 {
		t.Errorf("metrics listener without a token: %d", got)
	}
	if got := send("GET", strings.TrimSuffix(mURL, "/metrics")+"/v1/apps", "", bearer); got != 404 {
		t.Errorf("metrics listener serves /v1/apps: %d", got)
	}

	cancel()
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop")
	}

	out := logs.String()
	if testing.Verbose() {
		t.Logf("server log:\n%s", out)
	}
	for _, want := range []string{"http request", "created container", "serving metrics", "resolved config", "level=DEBUG"} {
		if format == "json" {
			want = strings.Replace(want, "level=DEBUG", `"level":"DEBUG"`, 1)
		}
		if !strings.Contains(out, want) {
			t.Errorf("server log lacks %q:\n%s", want, out)
		}
	}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("server log holds a secret (%.12s...):\n%s", s, out)
		}
		if strings.Contains(cliErr.String(), s) {
			t.Errorf("CLI stderr holds a secret (%.12s...):\n%s", s, cliErr.String())
		}
	}
	// The token in the path is logged only as the mask.
	if !strings.Contains(out, "/v1/apps/[redacted]") {
		t.Errorf("the request path with the token was not masked:\n%s", out)
	}
}

type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestMetricsListenerLoopbackOnly checks that the metrics listener binds
// loopback and nothing else, both in config validation and at bind time.
func TestMetricsListenerLoopbackOnly(t *testing.T) {
	ln, err := listenMetrics("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if ip := ln.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Errorf("bound %s", ip)
	}
	ln.Close()
	if ln, err := listenMetrics("localhost:0"); err != nil {
		t.Errorf("localhost: %v", err)
	} else {
		if ip := ln.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
			t.Errorf("localhost bound %s", ip)
		}
		ln.Close()
	}
	for _, addr := range []string{"0.0.0.0:0", "[::]:0", ":0"} {
		if ln, err := listenMetrics(addr); err == nil {
			ln.Close()
			t.Errorf("%s: listener opened on %s", addr, ln.Addr())
		}
	}

	home := isolate(t)
	for _, addr := range []string{"0.0.0.0:9100", ":9100", "[::]:9100", "10.0.0.1:9100"} {
		_, _, err := run(t, "", "serve", "--data-dir", filepath.Join(home, "d"), "--metrics-addr", addr)
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("serve --metrics-addr %s: %v", addr, err)
		}
	}
}
