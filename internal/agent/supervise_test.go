package agent

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// fakeClock is a clock the test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const webName = "hivemind-web-0-g1"

// startsOf counts the start calls the engine has seen.
func startsOf(e *env) int { return e.rt.counts(false)["start"] }

func TestBackoffDoublesAndCaps(t *testing.T) {
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second,
		5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}
	for n, d := range want {
		if got := backoff(n); got != d {
			t.Errorf("backoff(%d) = %s, want %s", n, got, d)
		}
	}
	if got := backoff(1000); got != 5*time.Minute {
		t.Errorf("backoff(1000) = %s, want the cap", got)
	}
}

func TestExitedContainerRestartedAndCountGoesUp(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	id := e.running()[webName].ID

	e.rt.exit(id, 1)
	e.clock.Advance(time.Second)
	e.reconcile()
	if c := e.running()[webName]; c.ID != id {
		t.Fatalf("after the first exit: %+v", c)
	}
	if st := e.status("web")[0]; st.RestartCount != 1 {
		t.Fatalf("restart count %d, want 1", st.RestartCount)
	}

	// The second exit waits 2 s.
	e.rt.exit(id, 1)
	e.reconcile()
	st := e.status("web")[0]
	if !st.CrashLooping || st.NextRestart.Sub(e.clock.Now()) != 2*time.Second {
		t.Fatalf("second exit: crash looping %v, next restart in %s", st.CrashLooping, st.NextRestart.Sub(e.clock.Now()))
	}
	e.clock.Advance(2 * time.Second)
	e.reconcile()
	if st := e.status("web")[0]; st.RestartCount != 2 || st.CrashLooping {
		t.Fatalf("after the second restart: %+v", st)
	}
}

// An engine that exits every container at once is the crash loop from the
// plan. Each restart must wait the full backoff: the delays double, cap at
// 5 minutes, and no pass starts a container early.
func TestImmediateExitBacksOffAndCaps(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	waits := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second,
		5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}
	startsBefore := startsOf(e)
	for i, d := range waits {
		id := e.running()[webName].ID
		e.rt.exit(id, 1)
		e.reconcile()
		st := e.status("web")[0]
		if !st.CrashLooping || st.NextRestart.IsZero() {
			t.Fatalf("exit %d: not reported as crash-looping: %+v", i+1, st)
		}
		if got := st.NextRestart.Sub(e.clock.Now()); got != d {
			t.Fatalf("exit %d: next restart in %s, want %s", i+1, got, d)
		}
		// Passes before the wait is over leave the container stopped.
		e.clock.Advance(d - time.Millisecond)
		e.reconcile()
		if len(e.running()) != 0 {
			t.Fatalf("exit %d: started before its %s backoff ended", i+1, d)
		}
		e.clock.Advance(time.Millisecond)
		e.reconcile()
		if len(e.running()) != 1 {
			t.Fatalf("exit %d: not started when the backoff ended", i+1)
		}
		if rc := e.status("web")[0].RestartCount; rc != i+1 {
			t.Fatalf("exit %d: restart count %d", i+1, rc)
		}
	}
	if n := startsOf(e) - startsBefore; n != len(waits) {
		t.Fatalf("%d starts for %d exits: the container was started more often than the backoff allows", n, len(waits))
	}
}

// A restarted agent reads the restart count back from the store and keeps
// the schedule the engine's FinishedAt gives, so it does not restart early.
func TestRestartedAgentKeepsBackoff(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	id := e.running()[webName].ID
	for i := range 3 {
		e.rt.exit(id, 1)
		e.clock.Advance(backoff(i))
		e.reconcile()
	}
	if rc := e.status("web")[0].RestartCount; rc != 3 {
		t.Fatalf("restart count %d, want 3", rc)
	}
	e.rt.exit(id, 1)
	e.reconcile()

	// A new agent on the same store and engine: its memory is empty.
	e.newAgent()
	e.clock.Advance(7 * time.Second) // the wait is 8 s
	e.reconcile()
	if len(e.running()) != 0 {
		t.Fatal("a restarted agent restarted the container before the backoff ended")
	}
	e.clock.Advance(time.Second)
	e.reconcile()
	if len(e.running()) != 1 {
		t.Fatal("the restarted agent did not restart the container when the backoff ended")
	}
	if rc := e.status("web")[0].RestartCount; rc != 4 {
		t.Fatalf("restart count after the restart: %d, want 4", rc)
	}
}

func TestRestartCountResetsAfterHealthyRunning(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	id := e.running()[webName].ID
	e.rt.exit(id, 1)
	e.clock.Advance(time.Second)
	e.reconcile()
	if rc := e.status("web")[0].RestartCount; rc != 1 {
		t.Fatalf("restart count %d, want 1", rc)
	}

	e.clock.Advance(9 * time.Minute)
	e.reconcile()
	if rc := e.status("web")[0].RestartCount; rc != 1 {
		t.Fatalf("count reset after 9 minutes: %d", rc)
	}
	e.clock.Advance(time.Minute + time.Second)
	e.reconcile()
	if rc := e.status("web")[0].RestartCount; rc != 0 {
		t.Fatalf("count after 10 minutes of healthy running: %d, want 0", rc)
	}
}

// Run does not wait for the next tick when a backoff ends: it wakes up then.
func TestRunRestartsWhenBackoffEnds(t *testing.T) {
	e := newEnv(t)
	e.rt.now = time.Now // the backoff is real time here
	e.put(web(1))
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Interval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, func() bool { return len(e.running()) == 1 })
	e.rt.exit(e.running()[webName].ID, 1)
	a.Trigger() // see the exit; the restart is due 1 s later, well before the hour
	waitFor(t, func() bool {
		st := e.status("web")
		return len(st) == 1 && st[0].RestartCount == 1 && len(e.running()) == 1
	})
}

// httpApp is an app whose HTTP check runs against port, with 1 s checks, so
// the tests can wait for real probe results.
func httpApp(port, retries int, startPeriod time.Duration) spec.App {
	app := web(1)
	app.Ports = nil
	app.Health = &spec.Health{
		HTTP:        &spec.HTTPCheck{Path: "/", Port: port},
		Interval:    spec.Duration(time.Second),
		Timeout:     spec.Duration(500 * time.Millisecond),
		Retries:     retries,
		StartPeriod: spec.Duration(startPeriod),
	}
	return app
}

// serveOn starts an HTTP server on a loopback port the test knows in advance.
func serveOn(t *testing.T, h http.Handler) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return ln.Addr().(*net.TCPAddr).Port
}

// reconcileFor runs passes for d of real time, moving the fake clock forward
// by step on each pass.
func reconcileFor(e *env, d, step time.Duration, until func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		e.reconcile()
		if until != nil && until() {
			return true
		}
		e.clock.Advance(step)
		time.Sleep(100 * time.Millisecond)
	}
	return until == nil
}

func TestFailingHTTPProbeRestartsContainer(t *testing.T) {
	port := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	e := newEnv(t)
	e.put(httpApp(port, 2, 0))
	ok := reconcileFor(e, 20*time.Second, time.Second, func() bool {
		st := e.status("web")
		return len(st) == 1 && st[0].RestartCount >= 1
	})
	if !ok {
		t.Fatalf("no restart after failing the HTTP probe: %+v", e.status("web"))
	}
	if n := e.rt.counts(false)["stop"]; n < 1 {
		t.Fatalf("unhealthy restart did not stop the container (stop calls %d)", n)
	}
	if st := e.status("web")[0]; st.Ready {
		t.Fatalf("a failing replica reported ready: %+v", st)
	}
}

func TestStartPeriodSuppressesFailures(t *testing.T) {
	port := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	e := newEnv(t)
	e.put(httpApp(port, 1, 10*time.Second))
	e.reconcile()

	// Two probes run in 2.5 s of real time. The clock does not move, so both
	// fall inside the 10 s start period, and neither may count.
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		e.reconcile()
		time.Sleep(100 * time.Millisecond)
	}
	if st := e.status("web")[0]; st.RestartCount != 0 || st.Health == "unhealthy" {
		t.Fatalf("failures inside the start period counted: %+v", st)
	}

	// Past the start period, one failure is enough, and the backoff applies.
	e.clock.Advance(11 * time.Second)
	ok := reconcileFor(e, 20*time.Second, time.Second, func() bool {
		return e.status("web")[0].RestartCount >= 1
	})
	if !ok {
		t.Fatalf("no restart once the start period was over: %+v", e.status("web"))
	}
}

func TestProbesDoNotBlockReconcile(t *testing.T) {
	e := newEnv(t)
	app := web(1)
	app.Ports = nil
	app.Health = &spec.Health{
		Exec:     []string{"sleep", "9"},
		Interval: spec.Duration(time.Second),
		Timeout:  spec.Duration(time.Second),
		Retries:  3,
	}
	e.put(app)
	e.reconcile()

	var calls atomic.Int32
	// The check blocks until its context ends, which is the worst case for a
	// probe. It runs in its own goroutine.
	e.rt.setExec(func(ctx context.Context, _ string, _ []string) (int, error) {
		calls.Add(1)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	e.reconcile()
	waitFor(t, func() bool { return calls.Load() >= 1 })

	for i := range 10 {
		start := time.Now()
		e.reconcile()
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("pass %d took %s while a probe was in flight", i, d)
		}
	}
	e.agent.Close()
	e.agent.mu.Lock()
	left := len(e.agent.probes)
	e.agent.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d probes still registered after Close", left)
	}
}

func TestExecProbePassRaisesReadiness(t *testing.T) {
	e := newEnv(t)
	app := web(1)
	app.Ports = nil
	app.Health = &spec.Health{Exec: []string{"true"}, Interval: spec.Duration(time.Second),
		Timeout: spec.Duration(time.Second), Retries: 2}
	e.put(app)
	e.rt.setExec(func(context.Context, string, []string) (int, error) { return 0, nil })
	e.reconcile()
	ok := reconcileFor(e, 10*time.Second, 0, func() bool {
		st := e.status("web")
		return len(st) == 1 && st[0].Health == "healthy" && st[0].Ready
	})
	if !ok {
		t.Fatalf("status never became healthy and ready: %+v", e.status("web"))
	}
}

func TestContainerWithoutHealthCheckIsReadyWhenRunning(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	st := e.status("web")[0]
	if !st.Ready || st.Health != "" || st.CrashLooping || st.RestartCount != 0 {
		t.Fatalf("status: %+v", st)
	}
}
