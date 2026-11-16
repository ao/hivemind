package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// blockedPulls makes every pull of one image block until its context ends
// or release is closed, and counts them.
type blockedPulls struct {
	started   chan struct{} // one value per pull that began
	cancelled atomic.Int64  // pulls that ended with their context
}

func blockPulls(f *fakeRuntime, image string, release <-chan struct{}) *blockedPulls {
	b := &blockedPulls{started: make(chan struct{}, 16)}
	f.mu.Lock()
	f.pullFn = func(ctx context.Context, img string) error {
		if !strings.Contains(img, image) {
			return nil
		}
		b.started <- struct{}{}
		select {
		case <-ctx.Done():
			b.cancelled.Add(1)
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	f.mu.Unlock()
	return b
}

// A registry that never answers must not stop the node: while one app's
// pull hangs, another app's container that exits is still restarted, and
// the pass still reports.
func TestHangingPullDoesNotHoldUpOtherApps(t *testing.T) {
	e := newEnv(t)
	e.put(spec.App{Name: "ok", Image: "busybox", Replicas: 1})
	e.reconcile()
	okID := e.running()["hivemind-ok-0-g1"].ID

	release := make(chan struct{})
	defer close(release)
	b := blockPulls(e.rt, "slow.example/app", release)
	e.put(spec.App{Name: "slow", Image: "slow.example/app:1", Replicas: 1})

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	passDone := make(chan error, 1)
	go func() { passDone <- e.agent.Reconcile(ctx) }()
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the pull of the slow image never started")
	}
	select {
	case err := <-passDone:
		if err != nil {
			t.Fatalf("the pass that started the pull: %v", err)
		}
	case <-time.After(pullWait + 3*time.Second):
		t.Fatalf("the pass is still waiting for a pull %s after it started: one hanging registry holds up every app on the node", pullWait+3*time.Second)
	}
	if st := e.status("slow"); len(st) != 1 || st[0].State != "pending" || !strings.Contains(st[0].Error, "pulling slow.example/app:1") {
		t.Fatalf("status of the app whose pull hangs: %+v", st)
	}

	// The other app is still supervised, and a pass takes no time.
	e.rt.exit(okID, 1)
	e.clock.Advance(time.Second)
	began := time.Now()
	if err := e.agent.Reconcile(ctx); err != nil {
		t.Fatalf("pass during the hanging pull: %v", err)
	}
	if d := time.Since(began); d > time.Second {
		t.Fatalf("a pass during the hanging pull took %s", d)
	}
	if c, ok := e.running()["hivemind-ok-0-g1"]; !ok || c.ID != okID {
		t.Fatal("the exited container of the other app was not restarted while a pull hung")
	}
	// No second pull of the same image starts while the first is in flight.
	if n := len(b.started); n != 0 {
		t.Fatalf("%d more pulls of the slow image started while one was in flight", n)
	}
}

// When the pull finishes, the agent passes again at once and starts the
// replica, without waiting for the next tick.
func TestFinishedPullTriggersAPass(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	b := blockPulls(e.rt, "slow.example/app", release)
	e.put(spec.App{Name: "slow", Image: "slow.example/app:1", Replicas: 1})
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Clock: e.clock.Now, Interval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	<-b.started
	time.Sleep(pullWait + 500*time.Millisecond) // the pull is in the background now
	close(release)
	waitFor(t, func() bool { return len(e.running()) == 1 })
}

// A pull that no replica needs any more, because the spec was fixed, is
// cancelled, and the fixed image is pulled at once.
func TestFixedSpecCancelsTheHangingPull(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	defer close(release)
	b := blockPulls(e.rt, "slow.example/app", release)
	e.put(spec.App{Name: "slow", Image: "slow.example/app:1", Replicas: 1})
	e.reconcile()
	<-b.started

	e.put(spec.App{Name: "slow", Image: "busybox", Replicas: 1}) // generation 2
	// The rollout moves the replica to generation 2, whose image is there,
	// and once that copy is ready, drops generation 1 and its pull.
	waitFor(t, func() bool {
		if err := e.st.AdvanceRollouts(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		e.reconcile()
		_, g1 := e.rt.snapshot()["hivemind-slow-0-g1"]
		return b.cancelled.Load() == 1 && !g1
	})
	if c, ok := e.running()["hivemind-slow-0-g2"]; !ok || !strings.HasSuffix(c.Image, "busybox:latest") {
		t.Fatalf("after the fix: running %v, want generation 2 with the fixed image", names(e.running()))
	}
	if st := e.status("slow"); len(st) != 1 || st[0].State != "running" || st[0].Error != "" {
		t.Fatalf("status after the fix: %+v", st)
	}
	if len(b.started) != 0 {
		t.Fatal("the slow image was pulled again after the fix")
	}
}

// Run's end cancels a pull in flight.
func TestRunEndCancelsPulls(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	defer close(release)
	b := blockPulls(e.rt, "slow.example/app", release)
	e.put(spec.App{Name: "slow", Image: "slow.example/app:1", Replicas: 1})
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Clock: e.clock.Now, Interval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Run(ctx)
	}()
	<-b.started
	time.Sleep(pullWait + 500*time.Millisecond)
	cancel()
	<-done
	waitFor(t, func() bool { return b.cancelled.Load() == 1 })
}
