package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingSource counts lease renewals, and passes the rest to the store.
type countingSource struct {
	*failingSource
	renewals atomic.Int64
	// While watch is set, early counts the renewals that came within within
	// of the last report, reported. Both are set before the agent runs.
	reported func() time.Time
	within   time.Duration
	watch    atomic.Bool
	early    atomic.Int64
}

func (c *countingSource) RenewLease(ctx context.Context, id string) error {
	c.renewals.Add(1)
	if c.watch.Load() && time.Since(c.reported()) < c.within {
		c.early.Add(1)
	}
	return c.Store.RenewLease(ctx, id)
}

// A pass renews the lease only when it reports, at its end. A pass that is
// slow to stop a container, as Docker is for 10 s with a PID 1 that ignores
// SIGTERM, must not keep the lease from being renewed: Run renews it on its
// own timer meanwhile.
func TestRunRenewsTheLeaseDuringASlowPass(t *testing.T) {
	e := newEnv(t)
	src := &countingSource{failingSource: e.src}
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: src, Interval: time.Hour,
		Heartbeat: 10 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	e.put(web(1))
	a.Trigger()
	waitFor(t, func() bool { return len(e.running()) == 1 })

	// The next pass stops the container, and the stop hangs until released.
	release := make(chan struct{})
	var stopping sync.Once
	inStop := make(chan struct{})
	e.rt.setFail(func(op, _ string) error {
		if op == "stop" {
			stopping.Do(func() { close(inStop) })
			<-release
		}
		return nil
	})
	if err := e.st.DeleteApp(ctxT(t), "web"); err != nil {
		t.Fatal(err)
	}
	a.Trigger()
	<-inStop
	before := src.renewals.Load()
	time.Sleep(200 * time.Millisecond)
	during := src.renewals.Load() - before
	close(release)

	if during < 5 {
		t.Fatalf("%d lease renewals in 200 ms while a pass was stopping a container, want at least 5", during)
	}
	waitFor(t, func() bool { return len(e.running()) == 0 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	// The heartbeat stops with Run.
	after := src.renewals.Load()
	time.Sleep(50 * time.Millisecond)
	if n := src.renewals.Load(); n != after {
		t.Fatalf("%d lease renewals after Run returned", n-after)
	}
}

// A report the server took renews the lease, so the heartbeat skips a
// renewal that would follow one within Heartbeat: with passes that report
// more often than the heartbeat, it renews nothing on its own after its
// first renewal, which finds out whether the server has the route
// (docs/ha.md#fewer-raft-entries). Once the reports stop going through, it
// renews again.
func TestHeartbeatSkipsARenewalAfterAReport(t *testing.T) {
	e := newEnv(t)
	src := &countingSource{failingSource: e.src}
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: src, Interval: 20 * time.Millisecond,
		Heartbeat: 100 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	src.reported = func() time.Time { return time.Unix(0, a.reported.Load()) }
	src.within = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	// The first tick renews though the passes report.
	waitFor(t, func() bool { return a.reported.Load() != 0 && src.renewals.Load() >= 1 })
	before := src.renewals.Load()
	// A renewal is right when no report went through for a heartbeat, as
	// when a pass stalls on a loaded machine (GitHub's runners, under the
	// race detector); one that comes within half a heartbeat of a report
	// should have been skipped.
	src.watch.Store(true)
	time.Sleep(450 * time.Millisecond)
	src.watch.Store(false)
	if n := src.early.Load(); n != 0 {
		t.Fatalf("%d lease renewals in 450 ms within 50 ms of a report, with a pass that reports every 20 ms; want none (%d renewals in all)",
			n, src.renewals.Load()-before)
	}
	// The reports stop: the heartbeat renews on its own again.
	a.reported.Store(time.Now().Add(-time.Hour).UnixNano())
	// A pass that cannot read its assignments does not report.
	e.src.setFail(errors.New("the server is away"))
	waitFor(t, func() bool { return src.renewals.Load() > before })
}
