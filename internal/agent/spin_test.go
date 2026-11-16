package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
)

// runFor runs a for d of real time with Interval set to an hour, so every
// pass after the first comes from a trigger or a scheduled wake, and returns
// how many passes it made.
func runFor(t *testing.T, e *env, d time.Duration) int {
	t.Helper()
	var passes atomic.Int64
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Clock: e.clock.Now, Interval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(e.logs, nil)),
		OnPass: func(time.Duration, error) { passes.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	// The restart counts and the schedule come from the agent the test
	// reconciled with, as if it had been running all along.
	a.restarts, a.wakeAt = e.agent.restarts, e.agent.wakeAt
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Run(ctx)
	}()
	time.Sleep(d)
	cancel()
	<-done
	return int(passes.Load())
}

// A container that waits out its restart backoff sets a wake time. When the
// daemon goes away before the wake, every pass fails at the list and the
// wake time stays in the past; Run must then wait for the next tick, not
// pass again at once, round after round, against a daemon that is down.
func TestRunDoesNotSpinWhileTheDaemonIsDown(t *testing.T) {
	e := newEnv(t)
	e.put(spec.App{Name: "web", Image: "busybox", Replicas: 1})
	e.reconcile()
	id := e.running()["hivemind-web-0-g1"].ID
	e.rt.exit(id, 1)
	e.reconcile() // the first restart waits 1 s: a wake is scheduled
	if e.agent.wakeAt.IsZero() {
		t.Fatal("no wake scheduled for the restart")
	}
	e.rt.setFail(func(op, _ string) error {
		if op == "list" || op == "inspect" || op == "start" {
			return &runtime.Error{Op: op, Err: runtime.ErrUnavailable, Message: "lost the connection to the Docker daemon"}
		}
		return nil
	})
	e.clock.Advance(time.Minute) // the wake is now in the past
	if n := runFor(t, e, 300*time.Millisecond); n > 3 {
		t.Fatalf("%d passes in 300 ms with a one-hour interval while the daemon was down: the loop spins", n)
	}
}

// The same with the server down: the assignments cannot be fetched, the
// pass ends early, and Run must not pass again at once.
func TestRunDoesNotSpinWhileTheServerIsDown(t *testing.T) {
	e := newEnv(t)
	e.put(spec.App{Name: "web", Image: "busybox", Replicas: 1})
	e.reconcile()
	e.rt.exit(e.running()["hivemind-web-0-g1"].ID, 1)
	e.reconcile()
	e.src.setFail(errors.New("connection refused"))
	e.clock.Advance(time.Minute)
	if n := runFor(t, e, 300*time.Millisecond); n > 3 {
		t.Fatalf("%d passes in 300 ms with a one-hour interval while the server was down: the loop spins", n)
	}
}
