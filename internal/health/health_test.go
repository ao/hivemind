package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// httpServer serves paths from routes and returns its loopback address and port.
func httpServer(t *testing.T, routes map[string]http.HandlerFunc) (string, int) {
	t.Helper()
	mux := http.NewServeMux()
	for p, h := range routes {
		mux.HandleFunc(p, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	return host, n
}

func httpHealth(port int, path string, timeout time.Duration) spec.Health {
	return spec.Health{HTTP: &spec.HTTPCheck{Path: path, Port: port}, Timeout: spec.Duration(timeout)}
}

func TestHTTPProbeStatusCodes(t *testing.T) {
	ok := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
	}
	host, port := httpServer(t, map[string]http.HandlerFunc{
		"/200": ok(200),
		"/204": ok(204),
		"/302": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/200", http.StatusFound) },
		"/404": ok(404),
		"/500": ok(500),
		"/slow": func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(2 * time.Second)
		},
	})
	tgt := Target{IP: host}
	tests := []struct {
		path    string
		timeout time.Duration
		pass    bool
	}{
		{"/200", time.Second, true},
		{"/204", time.Second, true},
		{"/302", time.Second, true}, // redirects are not followed; 3xx passes
		{"/404", time.Second, false},
		{"/500", time.Second, false},
		{"/slow", 200 * time.Millisecond, false},
	}
	for _, tc := range tests {
		err := Check(context.Background(), httpHealth(port, tc.path, tc.timeout), tgt, nil)
		if (err == nil) != tc.pass {
			t.Errorf("%s: err = %v, want pass %v", tc.path, err, tc.pass)
		}
		if err != nil && tc.path == "/slow" && !strings.Contains(err.Error(), "timed out") {
			t.Errorf("slow path error %q does not say timed out", err)
		}
	}
}

func TestHTTPProbeNeedsAddress(t *testing.T) {
	err := Check(context.Background(), httpHealth(80, "/", time.Second), Target{}, nil)
	if err == nil {
		t.Fatal("a probe with no container address passed")
	}
}

func TestHTTPProbeRefusedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := Check(context.Background(), httpHealth(port, "/", time.Second), Target{IP: "127.0.0.1"}, nil); err == nil {
		t.Fatal("a refused connection passed")
	}
}

func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	h := spec.Health{TCP: &spec.TCPCheck{Port: port}, Timeout: spec.Duration(time.Second)}
	tgt := Target{IP: "127.0.0.1"}
	if err := Check(context.Background(), h, tgt, nil); err != nil {
		t.Fatalf("open port: %v", err)
	}
	ln.Close()
	if err := Check(context.Background(), h, tgt, nil); err == nil {
		t.Fatal("closed port passed")
	}
}

// fakeExec answers exec probes. block makes it wait for the context.
type fakeExec struct {
	code  int
	err   error
	block bool
	mu    sync.Mutex
	calls []string
}

func (f *fakeExec) Exec(ctx context.Context, id string, cmd []string) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, id+" "+strings.Join(cmd, " "))
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return f.code, f.err
}

func TestExecProbe(t *testing.T) {
	h := spec.Health{Exec: []string{"test", "-f", "/ready"}, Timeout: spec.Duration(time.Second)}
	tgt := Target{ContainerID: "c1"}

	f := &fakeExec{code: 0}
	if err := Check(context.Background(), h, tgt, f); err != nil {
		t.Fatalf("exit 0: %v", err)
	}
	if len(f.calls) != 1 || f.calls[0] != "c1 test -f /ready" {
		t.Fatalf("exec calls: %v", f.calls)
	}

	f = &fakeExec{code: 2}
	if err := Check(context.Background(), h, tgt, f); err == nil || !strings.Contains(err.Error(), "exited with 2") {
		t.Fatalf("exit 2: %v", err)
	}

	f = &fakeExec{block: true}
	start := time.Now()
	err := Check(context.Background(), spec.Health{Exec: []string{"sleep", "9"}, Timeout: spec.Duration(150 * time.Millisecond)}, tgt, f)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("blocked exec: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the probe did not give up at its timeout")
	}

	f = &fakeExec{err: errors.New("daemon gone")}
	if err := Check(context.Background(), h, tgt, f); err == nil || !strings.Contains(err.Error(), "daemon gone") {
		t.Fatalf("runtime error: %v", err)
	}
	if err := Check(context.Background(), h, tgt, nil); err == nil {
		t.Fatal("exec probe with no runtime passed")
	}
}

func TestCheckWithoutProbe(t *testing.T) {
	if err := Check(context.Background(), spec.Health{}, Target{}, nil); err == nil {
		t.Fatal("empty health passed")
	}
}

func TestTrackerRetriesThenRecovers(t *testing.T) {
	start := time.Unix(1000, 0)
	tr := NewTracker(spec.Health{Retries: 3}, start)
	fail := errors.New("status 500")
	now := start.Add(time.Minute)

	if s := tr.Observe(now, fail); s != Starting {
		t.Fatalf("after 1 failure: %s", s)
	}
	if s := tr.Observe(now, fail); s != Starting {
		t.Fatalf("after 2 failures: %s", s)
	}
	if s := tr.Observe(now, fail); s != Unhealthy {
		t.Fatalf("after 3 failures: %s, want unhealthy", s)
	}
	if !tr.unhealthySince.Equal(now) {
		t.Fatalf("unhealthySince = %v", tr.unhealthySince)
	}
	if s := tr.Observe(now, nil); s != Healthy || tr.failures != 0 || !tr.unhealthySince.IsZero() {
		t.Fatalf("after a pass: %s failures %d", s, tr.failures)
	}
	// A single failure after recovery does not make it unhealthy again.
	if s := tr.Observe(now, fail); s != Healthy {
		t.Fatalf("one failure after recovery: %s", s)
	}
}

func TestTrackerStartPeriodSuppressesFailures(t *testing.T) {
	start := time.Unix(1000, 0)
	h := spec.Health{Retries: 2, StartPeriod: spec.Duration(10 * time.Second)}
	tr := NewTracker(h, start)
	fail := errors.New("refused")

	// Failures inside the start period never count, however many.
	for i := 1; i <= 20; i++ {
		if s := tr.Observe(start.Add(time.Duration(i/3)*time.Second), fail); s == Unhealthy {
			t.Fatalf("unhealthy inside the start period, after %d probes", i)
		}
	}
	if tr.failures != 0 {
		t.Fatalf("counted %d failures inside the start period", tr.failures)
	}
	// After it, the usual threshold applies.
	if s := tr.Observe(start.Add(11*time.Second), fail); s != Starting {
		t.Fatalf("first counted failure: %s", s)
	}
	if s := tr.Observe(start.Add(12*time.Second), fail); s != Unhealthy {
		t.Fatalf("second counted failure: %s, want unhealthy", s)
	}
}

func TestTrackerRecordsLastErrorTruncated(t *testing.T) {
	tr := NewTracker(spec.Health{Retries: 1}, time.Time{})
	tr.Observe(time.Now(), errors.New(strings.Repeat("x", 500)))
	if got := tr.lastErr; len(got) > maxErrLen+3 || !strings.HasSuffix(got, "...") {
		t.Fatalf("lastErr is %d bytes, not truncated", len(got))
	}
}

func TestMonitorReportsUnhealthyAndRecovers(t *testing.T) {
	var code atomic.Int32
	code.Store(500)
	host, port := httpServer(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(code.Load())) },
	})
	h := spec.Health{
		HTTP:     &spec.HTTPCheck{Path: "/", Port: port},
		Interval: spec.Duration(10 * time.Millisecond),
		Timeout:  spec.Duration(time.Second),
		Retries:  2,
	}
	m := NewMonitor(h, Target{IP: host}, nil, time.Now(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	waitState(t, m, Unhealthy)
	if r := m.Result(); r.UnhealthySince.IsZero() || r.LastError == "" || r.Failures < 2 {
		t.Fatalf("unhealthy result: %+v", r)
	}
	code.Store(200)
	waitState(t, m, Healthy)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func waitState(t *testing.T, m *Monitor, want State) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for m.Result().State != want {
		if time.Now().After(deadline) {
			t.Fatalf("state %s, want %s", m.Result().State, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
