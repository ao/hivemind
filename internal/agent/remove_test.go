package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// slowStops makes the stop of each container whose name has prefix block
// until release, or until the stop's context ends, as Docker's stop of a
// PID 1 that ignores SIGTERM blocks for the whole grace period. It counts
// the stops it has seen and how many were in flight at once.
type slowStops struct {
	prefix  string
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
	now     int
	most    int
}

func newSlowStops(prefix string) *slowStops {
	return &slowStops{prefix: prefix, release: make(chan struct{})}
}

func (s *slowStops) stop(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, s.prefix) {
		return nil
	}
	s.mu.Lock()
	s.calls++
	s.now++
	s.most = max(s.most, s.now)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.now--
		s.mu.Unlock()
	}()
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slowStops) open() { s.once.Do(func() { close(s.release) }) }

// counts returns the stops seen, those in flight, and the most in flight at once.
func (s *slowStops) counts() (calls, now, most int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.now, s.most
}

// addSleepers puts n running containers of an app that is not assigned, as
// busybox sleep containers of a deleted app are, on the engine.
func addSleepers(e *env, n int) {
	for i := range n {
		e.rt.add(fmt.Sprintf("hivemind-sleep-%d-g1", i), "running",
			map[string]string{runtime.LabelApp: "sleep", runtime.LabelReplica: strconv.Itoa(i), runtime.LabelGeneration: "1"})
	}
}

// removalsInFlight returns how many of a's removals have not ended.
func removalsInFlight(a *Agent) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, r := range a.removals {
		if !r.ended() {
			n++
		}
	}
	return n
}

func countNamed(sts []store.Status, prefix string) int {
	n := 0
	for _, s := range sts {
		if strings.HasPrefix(s.Name, prefix) {
			n++
		}
	}
	return n
}

// The scale run on ten machines: a node removing containers that ignore
// SIGTERM, one after another for 10 s each, created a rollout's surge 42 s
// after it was placed, and health-checked rollouts paused at their deadline.
// Here ten containers stop slowly while a surge is assigned to the node. The
// surge must be created within one pass interval, the stops must run
// removeSlots at a time, a stop must not be started twice, and the
// containers must be reported until they are gone.
func TestSlowRemovalsDoNotHoldUpASurge(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	// The replica runs on node-2; its rollout surges onto this node.
	if err := e.st.UpsertNode(ctx, store.Node{ID: "node-2", Name: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.SetNodeEligibility(ctx, nodeID, false); err != nil {
		t.Fatal(err)
	}
	app := spec.App{Name: "web", Image: "nginx:alpine", Replicas: 1}
	e.put(app)
	if _, err := e.st.SetNodeEligibility(ctx, nodeID, true); err != nil {
		t.Fatal(err)
	}
	app.Env = map[string]string{"V": "2"}
	e.put(app)
	if err := e.st.AdvanceRollouts(ctx); err != nil {
		t.Fatal(err)
	}
	if as, err := e.st.AssignmentsForNode(ctx, nodeID); err != nil || len(as) != 1 || as[0].Generation != 2 {
		t.Fatalf("assignments of %s: %+v, %v; want the surge at generation 2", nodeID, as, err)
	}
	e.rt.mu.Lock()
	e.rt.images["docker.io/library/nginx:alpine"] = true
	e.rt.mu.Unlock()

	slow := newSlowStops("hivemind-sleep-")
	defer slow.open()
	e.rt.setStop(slow.stop)
	addSleepers(e, 10)
	created := make(chan time.Time, 1)
	e.rt.setFail(func(op, arg string) error {
		if op == "create" && arg == "hivemind-web-0-g2" {
			select {
			case created <- time.Now():
			default:
			}
		}
		return nil
	})
	var passes atomic.Int64
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Interval: DefaultInterval,
		OnPass: func(time.Duration, error) { passes.Add(1) },
		Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- a.Run(runCtx) }()
	defer func() {
		cancel()
		<-done
	}()

	select {
	case at := <-created:
		if d := at.Sub(start); d > DefaultInterval {
			t.Fatalf("the surge was created %s after the agent started, want within one pass interval (%s)", d.Round(time.Millisecond), DefaultInterval)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the surge was not created while ten containers were stopping")
	}
	waitFor(t, func() bool { _, now, _ := slow.counts(); return now == removeSlots })

	// Passes go on while the stops hang. Each reports the containers that
	// are being removed, beside the surge, and starts no stop again.
	p := passes.Load()
	waitFor(t, func() bool {
		a.Trigger()
		return passes.Load() >= p+3
	})
	sts, err := e.st.StatusForNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n := countNamed(sts, "hivemind-sleep-"); n != 10 || countNamed(sts, "hivemind-web-0-g2") != 1 {
		t.Fatalf("report while the stops hang: %d of 10 stopping containers, %+v", n, sts)
	}
	if calls, now, most := slow.counts(); calls != removeSlots || now != removeSlots || most != removeSlots {
		t.Fatalf("stops: %d started, %d in flight, at most %d at once; want %d each: the rest queued, none started twice",
			calls, now, most, removeSlots)
	}

	slow.open()
	waitFor(t, func() bool {
		for n := range e.rt.snapshot() {
			if strings.HasPrefix(n, "hivemind-sleep-") {
				return false
			}
		}
		sts, err := e.st.StatusForNode(ctx, nodeID)
		return err == nil && countNamed(sts, "hivemind-sleep-") == 0
	})
	if calls, _, most := slow.counts(); calls != 10 || most > removeSlots {
		t.Fatalf("stops: %d started, at most %d at once; want 10, and at most %d", calls, most, removeSlots)
	}
	if c := e.rt.counts(false); c["remove"] != 10 {
		t.Fatalf("%d removes, want 10", c["remove"])
	}
}

// A wanted container that needs a host port an extra one holds starts only
// once that one is gone. Its removal skips the queue of slow removals, so the
// in-place update of a replica on host port 8080 is not held up behind them.
// A port holder that takes longer than removeWait leaves the replica pending,
// with what it waits for, and a later pass starts it.
func TestRemovalOfAPortHolderGoesFirst(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	e.rt.mu.Lock()
	e.rt.ports = true
	e.rt.mu.Unlock()
	e.put(web(1))
	e.reconcile()
	if _, ok := e.running()["hivemind-web-0-g1"]; !ok {
		t.Fatalf("running: %v", names(e.running()))
	}

	// Nine slow removals fill the slots, and one waits for a slot.
	slow := newSlowStops("hivemind-sleep-")
	defer slow.open()
	held := make(chan struct{})
	var heldOnce sync.Once
	release := func() { heldOnce.Do(func() { close(held) }) }
	defer release()
	e.rt.setStop(func(ctx context.Context, name string) error {
		if name == "hivemind-web-0-g2" {
			select {
			case <-held:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return slow.stop(ctx, name)
	})
	addSleepers(e, removeSlots+1)
	e.reconcile()
	waitFor(t, func() bool { _, now, _ := slow.counts(); return now == removeSlots })

	// An update in place: the new generation needs port 8080.
	changed := web(1)
	changed.Env = map[string]string{"GREETING": "bonjour"}
	e.put(changed)
	if err := e.st.AdvanceRollouts(ctx); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	run := e.running()
	if _, ok := run["hivemind-web-0-g2"]; !ok {
		t.Fatalf("generation 2 does not run while the slots are full: %v", names(run))
	}
	if _, ok := e.rt.snapshot()["hivemind-web-0-g1"]; ok {
		t.Fatal("generation 1 is still there")
	}

	// The next generation's port holder, generation 2, takes longer to stop
	// than a pass waits.
	changed.Env = map[string]string{"GREETING": "hola"}
	e.put(changed)
	if err := e.st.AdvanceRollouts(ctx); err != nil {
		t.Fatal(err)
	}
	e.rt.counts(true)
	e.reconcile()
	if c := e.rt.counts(false); c["create"] != 0 {
		t.Fatalf("%d creates while the port holder was stopping", c["create"])
	}
	var g3 *store.Status
	for _, s := range e.status("web") {
		if s.Name == "hivemind-web-0-g3" {
			g3 = &s
		}
	}
	want := "waiting for container hivemind-web-0-g2, which holds host port 8080/tcp, to be removed"
	if g3 == nil || g3.State != "pending" || g3.Error != want {
		t.Fatalf("generation 3 while its port is held: %+v, want pending: %s", g3, want)
	}
	release()
	waitFor(t, func() bool {
		_, ok := e.rt.snapshot()["hivemind-web-0-g2"]
		return !ok
	})
	e.reconcile()
	if _, ok := e.running()["hivemind-web-0-g3"]; !ok {
		t.Fatalf("generation 3 does not run once the port is free: %v", names(e.running()))
	}
}

// A removal that fails in the background is reported by the next pass, as
// the container's error and the pass's, and the pass after tries again.
func TestBackgroundRemovalErrorIsReportedAndRetried(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	e.put(web(1))
	e.reconcile()
	if err := e.st.DeleteApp(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	slow := newSlowStops("hivemind-web-")
	defer slow.open()
	e.rt.setStop(slow.stop)
	var failed atomic.Bool
	e.rt.setFail(func(op, _ string) error {
		if op == "remove" && failed.CompareAndSwap(false, true) {
			return rtErr("remove", runtime.ErrUnavailable)
		}
		return nil
	})

	// The stop hangs: the container is reported, without an error.
	e.reconcile()
	if reps, _ := e.st.Replicas(ctx, "web"); len(reps) != 1 || reps[0].Desired != store.Removed {
		t.Fatalf("replicas while the stop hangs: %+v", reps)
	}
	if st := e.status("web"); len(st) != 1 || st[0].Error != "" {
		t.Fatalf("status while the stop hangs: %+v", st)
	}

	slow.open()
	waitFor(t, func() bool { return failed.Load() && removalsInFlight(e.agent) == 0 })
	err := e.agent.Reconcile(ctx)
	if !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("the pass after the failed removal: %v, want ErrUnavailable", err)
	}
	if st := e.status("web"); len(st) != 1 || !strings.Contains(st[0].Error, "removing hivemind-web-0-g1") {
		t.Fatalf("status after the failed removal: %+v", st)
	}
	if reps, _ := e.st.Replicas(ctx, "web"); len(reps) != 1 {
		t.Fatalf("replicas after the failed removal: %+v", reps)
	}

	e.reconcile()
	if reps, _ := e.st.Replicas(ctx, "web"); len(reps) != 0 {
		t.Fatalf("replicas after the retry: %+v", reps)
	}
	if c := e.rt.counts(false); c["remove"] != 2 {
		t.Fatalf("%d removes, want 2: the failure and the retry", c["remove"])
	}
}

// Close cancels the removals in flight and waits for them; the container is
// left for the next agent.
func TestCloseCancelsRemovals(t *testing.T) {
	e := newEnv(t)
	slow := newSlowStops("hivemind-sleep-")
	defer slow.open()
	e.rt.setStop(slow.stop)
	addSleepers(e, 1)
	e.reconcile()
	start := time.Now()
	e.agent.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %s", d)
	}
	if _, now, _ := slow.counts(); now != 0 {
		t.Fatalf("%d stops in flight after Close", now)
	}
	if _, ok := e.rt.snapshot()["hivemind-sleep-0-g1"]; !ok {
		t.Fatal("Close removed the container")
	}
}
