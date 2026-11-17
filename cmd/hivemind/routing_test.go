package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/nodeapi"
)

func TestServerNodeAddress(t *testing.T) {
	ctx := context.Background()
	for in, want := range map[string]string{"": "127.0.0.1", "10.0.0.5": "10.0.0.5", "::ffff:10.0.0.5": "10.0.0.5",
		"fd00::5": "fd00::5", "localhost": "127.0.0.1"} {
		got, err := serverNodeAddress(ctx, in)
		if err != nil || got != want {
			t.Errorf("serverNodeAddress(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := serverNodeAddress(ctx, "no-such-host.invalid"); err == nil {
		t.Error("a name that does not resolve was accepted")
	}
	if err := checkBoundAddress("10.0.0.5:4484", "10.0.0.5"); err != nil {
		t.Error(err)
	}
	if err := checkBoundAddress("[::1]:4484", "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "give --advertise as an IP address") {
		t.Errorf("a bound address unlike the node's: %v", err)
	}
}

// fakeRegistrar refuses a registration with an address, as a 2.1 server's
// decoder refuses a field it does not know, when old is set.
type fakeRegistrar struct {
	old   bool
	calls []nodeapi.Register
}

func (f *fakeRegistrar) Register(_ context.Context, _ string, g nodeapi.Register) error {
	f.calls = append(f.calls, g)
	if f.old && g.Address != "" {
		return &nodeapi.RefusedError{Code: http.StatusBadRequest, Status: "400 Bad Request", Message: "invalid request body"}
	}
	return nil
}

func TestRegisterFallsBackForAnOlderServer(t *testing.T) {
	ctx := context.Background()
	reg := nodeapi.Register{Name: "w", CPU: 2, Memory: 1 << 30, Address: "10.0.0.6"}

	newer := &fakeRegistrar{}
	if refused, err := registerWith(ctx, newer, "node-w", reg); err != nil || refused || len(newer.calls) != 1 || newer.calls[0].Address != "10.0.0.6" {
		t.Fatalf("a 2.2 server: %v %v, %+v", refused, err, newer.calls)
	}
	older := &fakeRegistrar{old: true}
	if refused, err := registerWith(ctx, older, "node-w", reg); err != nil || !refused || len(older.calls) != 2 || older.calls[1].Address != "" {
		t.Fatalf("an older server: %v %v, %+v", refused, err, older.calls)
	}
	// Without an address there is nothing to retry.
	noAddr := &fakeRegistrar{old: true}
	reg.Address = ""
	if refused, err := registerWith(ctx, noAddr, "node-w", reg); err != nil || refused || len(noAddr.calls) != 1 {
		t.Fatalf("no address: %v %v, %+v", refused, err, noAddr.calls)
	}
}

// upgradable is a server that refuses node addresses until it is upgraded,
// and then sends its version, which changes ServerChanged.
type upgradable struct {
	mu       sync.Mutex
	upgraded bool
	version  string
	changed  chan struct{}
	fakeRegistrar
}

func (u *upgradable) ServerVersion() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.version
}

func (u *upgradable) ServerChanged() <-chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.changed
}

func (u *upgradable) Register(ctx context.Context, id string, g nodeapi.Register) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.old = !u.upgraded
	return u.fakeRegistrar.Register(ctx, id, g)
}

func (u *upgradable) upgrade() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.upgraded, u.version = true, "v2.2.0"
	close(u.changed)
	u.changed = make(chan struct{})
}

func (u *upgradable) addresses() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for _, c := range u.calls {
		out = append(out, c.Address)
	}
	return out
}

// A worker whose address an older server refused registers it once the
// server is upgraded, without a restart: at once when the server's version
// changes, and on the retry timer when it does not.
func TestWorkerRegistersItsAddressAfterTheServerUpgrade(t *testing.T) {
	srv := &upgradable{changed: make(chan struct{})}
	var refused atomic.Bool
	reg := nodeapi.Register{Name: "w", Address: "10.0.0.6"}
	register := func(ctx context.Context) error {
		r, err := registerWith(ctx, srv, "node-w", reg)
		refused.Store(r)
		return err
	}
	if err := register(context.Background()); err != nil || !refused.Load() {
		t.Fatalf("first registration with an old server: %v, refused %v", err, refused.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		retryAddress(ctx, srv, refused.Load, register, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() {
		cancel()
		<-done
	}()
	time.Sleep(50 * time.Millisecond)
	if n := len(srv.addresses()); n != 2 {
		t.Fatalf("%d registrations before the server changed, want 2: %v", n, srv.addresses())
	}
	srv.upgrade()
	waitUntil(t, "the address registered", func() bool { return !refused.Load() })
	if got := srv.addresses(); got[len(got)-1] != "10.0.0.6" {
		t.Fatalf("registrations %q: the last has no address", got)
	}

	// With no change of version, the timer retries.
	srv2 := &upgradable{changed: make(chan struct{})}
	refused.Store(true)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go retryAddress(ctx2, srv2, refused.Load, func(ctx context.Context) error {
		r, err := registerWith(ctx, srv2, "node-w", reg)
		refused.Store(r)
		return err
	}, 20*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	waitUntil(t, "a retry on the timer", func() bool { return len(srv2.addresses()) >= 4 })
}

// serve with --route-addr runs the router on that address beside the API:
// it knows the routed hosts from the store, redirects them to https, and
// answers 404 for other hosts. No ACME CA is reachable here, so there is no
// certificate, and the test does not use the https port.
func TestServeRunsTheRouter(t *testing.T) {
	home := isolate(t)
	data := filepath.Join(home, "data")
	if _, _, err := run(t, "", "init", "--data-dir", data); err != nil {
		t.Fatal(err)
	}
	token, err := auth.ReadAdminToken(data)
	if err != nil {
		t.Fatal(err)
	}
	apiPort, httpPort, httpsPort := unusedPort(t), unusedPort(t), unusedPort(t)
	env := map[string]string{
		"HIVEMIND_DATA_DIR":           data,
		"HIVEMIND_API_ADDR":           "127.0.0.1:" + strconv.Itoa(apiPort),
		"HIVEMIND_RECONCILE_INTERVAL": "50ms",
		"HIVEMIND_MIN_READY":          "0s",
		"HIVEMIND_ROUTE_ADDR":         "127.0.0.1",
		"HIVEMIND_ROUTE_HTTP_PORT":    strconv.Itoa(httpPort),
		"HIVEMIND_ROUTE_HTTPS_PORT":   strconv.Itoa(httpsPort),
		"HIVEMIND_ACME_EMAIL":         "ops@example.com",
		// .invalid never resolves (RFC 6761), so no request leaves the host.
		"HIVEMIND_ACME_CA": "https://acme.invalid/directory",
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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, log, newMemRuntime()) }()
	t.Cleanup(func() {
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
	do := func(method, url, host, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if host != "" {
			req.Host = host
		}
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if resp, body := do(http.MethodPut, api+"/v1/apps/web", "",
		`{"image":"nginx","replicas":2,"routes":[{"host":"www.example.com","port":80}]}`); resp.StatusCode/100 != 2 {
		t.Fatalf("put: %d %s", resp.StatusCode, body)
	}
	var routes struct {
		Routes []struct {
			Host      string   `json:"host"`
			Endpoints []string `json:"endpoints"`
		} `json:"routes"`
	}
	_, body := do(http.MethodGet, api+"/v1/routes", "", "")
	if err := json.Unmarshal([]byte(body), &routes); err != nil || len(routes.Routes) != 1 || routes.Routes[0].Host != "www.example.com" {
		t.Fatalf("routes: %s", body)
	}

	router := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	want := fmt.Sprintf("https://www.example.com:%d/x?y=1", httpsPort)
	waitUntil(t, "redirect", func() bool {
		resp, _ := do(http.MethodGet, router+"/x?y=1", "www.example.com", "")
		return resp.StatusCode == http.StatusPermanentRedirect && resp.Header.Get("Location") == want
	})
	if resp, body := do(http.MethodGet, router+"/", "other.example.com", ""); resp.StatusCode != http.StatusNotFound || strings.Contains(body, "www") {
		t.Fatalf("unknown host: %d %q", resp.StatusCode, body)
	}
	_, metrics := do(http.MethodGet, api+"/metrics", "", "")
	// The server's own router, under its node ID.
	for _, want := range []string{`hivemind_router_requests_total{node="node-`, `,host="www.example.com",code="308"}`,
		`,host="unknown",code="404"}`} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics have no %s:\n%s", want, metrics)
		}
	}
	if s := logs.String(); !strings.Contains(s, "router listening") {
		t.Errorf("no router line in the log:\n%s", s)
	}
}

type fakeRouterReporter struct {
	mu    sync.Mutex
	posts []nodeapi.RouterReport
	errs  []error // returned in turn; nil once used up
}

func (f *fakeRouterReporter) ReportRouter(_ context.Context, id string, r nodeapi.RouterReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != "node-w" {
		return fmt.Errorf("posted for %q", id)
	}
	f.posts = append(f.posts, r)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return nil
}

func (f *fakeRouterReporter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

type fakeWatcher struct{ changed chan struct{} }

func (fakeWatcher) ServerVersion() string            { return "" }
func (w fakeWatcher) ServerChanged() <-chan struct{} { return w.changed }

// The worker posts its router's report every period, keeps going after a
// failure, and stops posting to a server without the route.
func TestReportRouterPostsEveryPeriod(t *testing.T) {
	var logs strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &logs}, nil))
	f := &fakeRouterReporter{errs: []error{fmt.Errorf("connection refused"), fmt.Errorf("connection refused")}}
	var n atomic.Int64
	report := func() nodeapi.RouterReport {
		return nodeapi.RouterReport{Requests: []nodeapi.RouterRequests{{Host: "www.example.com", Code: 200, Count: uint64(n.Add(1))}}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); reportRouter(ctx, f, "node-w", report, 10*time.Millisecond, nil, log) }()
	deadline := time.Now().Add(5 * time.Second)
	for f.count() < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if f.count() < 5 {
		t.Fatalf("%d posts", f.count())
	}
	mu.Lock()
	out := logs.String()
	mu.Unlock()
	if c := strings.Count(out, "post router metrics"); c != 1 {
		t.Errorf("%d failure lines, want 1 for two failures in a row:\n%s", c, out)
	}
	if !strings.Contains(out, "reach the server again") {
		t.Errorf("no recovery line:\n%s", out)
	}

	// An old server: one post, then nothing until the server changes.
	old := &fakeRouterReporter{errs: []error{nodeapi.ErrNoRoute}}
	missing := nodeapi.NewMissingRoute(fakeWatcher{changed: make(chan struct{})}, "router metrics", log)
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { defer close(done); reportRouter(ctx, old, "node-w", report, 10*time.Millisecond, missing, log) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	if old.count() != 1 {
		t.Errorf("%d posts to a server without the route, want 1", old.count())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
