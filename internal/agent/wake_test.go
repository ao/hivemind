package agent

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flakySource is a server that can be made unreachable for the passes and
// for the lease renewals alike.
type flakySource struct {
	*failingSource
	down atomic.Bool
}

func (f *flakySource) setDown(down bool) {
	f.down.Store(down)
	if down {
		f.setFail(unreachableErr{})
	} else {
		f.setFail(nil)
	}
}

func (f *flakySource) RenewLease(ctx context.Context, id string) error {
	if f.down.Load() {
		return unreachableErr{}
	}
	return f.Store.RenewLease(ctx, id)
}

// passWatch records the end of each pass Run makes, and whether it failed.
type passWatch struct {
	mu     sync.Mutex
	passes []time.Time
	failed []bool
	ended  chan struct{}
}

func newPassWatch() *passWatch { return &passWatch{ended: make(chan struct{}, 100)} }

func (w *passWatch) onPass(_ time.Duration, err error) {
	w.mu.Lock()
	w.passes = append(w.passes, time.Now())
	w.failed = append(w.failed, err != nil)
	w.mu.Unlock()
	select {
	case w.ended <- struct{}{}:
	default:
	}
}

// failures is the number of passes so far, all of which must have failed.
func (w *passWatch) failures(t *testing.T) int {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, f := range w.failed {
		if !f {
			t.Fatalf("pass %d did not fail", i)
		}
	}
	return len(w.failed)
}

// firstGoodAfter is when the first pass that worked after since ended, or
// zero.
func (w *passWatch) firstGoodAfter(since time.Time) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, p := range w.passes {
		if p.After(since) && !w.failed[i] {
			return p
		}
	}
	return time.Time{}
}

// runAgent runs a until the test ends.
func runAgent(t *testing.T, a *Agent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// downFor waits until w has seen n failed passes. The next wait is then
// interval doubled n-1 times.
func downFor(t *testing.T, w *passWatch, n int) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for w.failures(t) < n {
		select {
		case <-w.ended:
		case <-deadline:
			t.Fatalf("only %d failed passes in 30 s, want %d", w.failures(t), n)
		}
	}
}

// After the server has been unreachable for a while, the passes are backed
// off. Once a lease renewal gets through again, the next pass runs at once,
// and reports the node's containers, rather than when the backoff ends. In
// the third soak a worker's lease was back within a second of the server's
// return, but it reported its containers up to 30 s later, and until then
// its replicas showed as unknown.
func TestAPassRunsOnceTheLeaseGetsThroughAgain(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	src := &flakySource{failingSource: e.src}
	src.setDown(true)
	w := newPassWatch()
	const interval = 100 * time.Millisecond
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: src, Clock: e.clock.Now, Interval: interval,
		Heartbeat: 20 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(e.logs, nil)), OnPass: w.onPass})
	if err != nil {
		t.Fatal(err)
	}
	runAgent(t, a)
	// Five failed passes: the next would wait 1.6 s.
	downFor(t, w, 5)
	up := time.Now()
	src.setDown(false)
	waitFor(t, func() bool { return !w.firstGoodAfter(up).IsZero() })
	if d := w.firstGoodAfter(up).Sub(up); d > 800*time.Millisecond {
		t.Fatalf("the first pass after the server came back ended %s later, want well inside the 1.6 s backoff", d)
	}
	if len(e.running()) != 1 {
		t.Fatalf("running after the pass: %v", e.running())
	}
}

// ServerAnswered, which the worker calls on any reply from the server, ends
// the backoff in the same way, for a source without lease renewals. Outside
// a backoff it does nothing: it is called on every reply.
func TestServerAnsweredEndsTheBackoffAndNothingElse(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.src.setFail(unreachableErr{})
	w := newPassWatch()
	const interval = 100 * time.Millisecond
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Clock: e.clock.Now, Interval: interval,
		Logger: slog.New(slog.NewTextHandler(e.logs, nil)), OnPass: w.onPass})
	if err != nil {
		t.Fatal(err)
	}
	runAgent(t, a)
	downFor(t, w, 5)
	up := time.Now()
	e.src.setFail(nil)
	a.ServerAnswered()
	waitFor(t, func() bool { return !w.firstGoodAfter(up).IsZero() })
	if d := w.firstGoodAfter(up).Sub(up); d > 800*time.Millisecond {
		t.Fatalf("the first pass after ServerAnswered ended %s later, want well inside the 1.6 s backoff", d)
	}

	// Not backed off: an agent with an hour between passes makes none for
	// ServerAnswered.
	e2 := newEnv(t)
	w2 := newPassWatch()
	a2, err := New(Config{NodeID: nodeID, Runtime: e2.rt, Source: e2.src, Clock: e2.clock.Now, Interval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(e2.logs, nil)), OnPass: w2.onPass})
	if err != nil {
		t.Fatal(err)
	}
	runAgent(t, a2)
	<-w2.ended // the first pass, at once
	for range 10 {
		a2.ServerAnswered()
	}
	select {
	case <-w2.ended:
		t.Fatal("ServerAnswered made a pass while the server was reachable")
	case <-time.After(300 * time.Millisecond):
	}
}
