package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

const nodeID = "node-1"

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// failingSource wraps the store so tests can make the server unreachable.
type failingSource struct {
	*store.Store
	mu   sync.Mutex
	fail error
}

func (f *failingSource) setFail(err error) { f.mu.Lock(); f.fail = err; f.mu.Unlock() }

func (f *failingSource) AssignmentsForNode(ctx context.Context, id string) ([]store.Assignment, error) {
	f.mu.Lock()
	err := f.fail
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Store.AssignmentsForNode(ctx, id)
}

type env struct {
	t     *testing.T
	path  string
	st    *store.Store
	src   *failingSource
	rt    *fakeRuntime
	agent *Agent
	logs  *syncBuffer
	clock *fakeClock
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, path: filepath.Join(t.TempDir(), "hivemind.db"), rt: newFakeRuntime(), clock: newFakeClock()}
	e.rt.now = e.clock.Now
	e.openStore()
	if err := e.st.UpsertNode(ctxT(t), store.Node{ID: nodeID, Name: "one"}); err != nil {
		t.Fatal(err)
	}
	e.newAgent()
	return e
}

func (e *env) openStore() {
	st, err := store.Open(ctxT(e.t), e.path, store.WithMinReady(0))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { st.Close() })
	e.st = st
	e.src = &failingSource{Store: st}
}

func (e *env) newAgent() {
	e.logs = &syncBuffer{}
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Clock: e.clock.Now,
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		e.t.Fatal(err)
	}
	e.agent = a
}

func (e *env) put(a spec.App) store.App {
	e.t.Helper()
	got, err := e.st.PutApp(ctxT(e.t), a)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *env) reconcile() {
	e.t.Helper()
	if err := e.agent.Reconcile(ctxT(e.t)); err != nil {
		e.t.Fatalf("Reconcile: %v", err)
	}
}

// running returns the running containers by name.
func (e *env) running() map[string]fakeContainer {
	out := map[string]fakeContainer{}
	for n, c := range e.rt.snapshot() {
		if c.State == "running" {
			out[n] = c
		}
	}
	return out
}

func (e *env) status(app string) []store.Status {
	e.t.Helper()
	st, err := e.st.ListStatus(ctxT(e.t), app)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func web(replicas int) spec.App {
	return spec.App{Name: "web", Image: "nginx:alpine", Replicas: replicas, Env: map[string]string{"GREETING": "hello"},
		Ports: []spec.Port{{Host: 8080, Container: 80}}}
}

func names(m map[string]fakeContainer) []string {
	var out []string
	for n := range m {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with an empty config succeeded")
	}
	a, err := New(Config{NodeID: "n", Runtime: newFakeRuntime(), Source: &failingSource{}})
	if err != nil || a.cfg.Interval != DefaultInterval || a.cfg.StopTimeout != DefaultStopTimeout {
		t.Fatalf("defaults: %+v %v", a.cfg, err)
	}
}

func TestContainerName(t *testing.T) {
	if got := ContainerName("web", 3, 12); got != "hivemind-web-3-g12" {
		t.Fatalf("ContainerName: %q", got)
	}
}

func TestStartsMissingReplicas(t *testing.T) {
	e := newEnv(t)
	web := web(2)
	web.Ports = nil // two replicas on one node can't share a host port
	e.put(web)
	e.reconcile()

	got := e.running()
	if want := []string{"hivemind-web-0-g1", "hivemind-web-1-g1"}; strings.Join(names(got), ",") != strings.Join(want, ",") {
		t.Fatalf("running containers %v, want %v", names(got), want)
	}
	c := got["hivemind-web-1-g1"]
	if c.Labels[runtime.LabelApp] != "web" || c.Labels[runtime.LabelReplica] != "1" || c.Labels[runtime.LabelGeneration] != "1" {
		t.Fatalf("labels: %v", c.Labels)
	}
	if c.cfg.Env["GREETING"] != "hello" || c.cfg.Image != "docker.io/library/nginx:alpine" {
		t.Fatalf("container config: %+v", c.cfg)
	}
	if n := e.rt.counts(false)["pull"]; n != 1 {
		t.Fatalf("%d pulls, want 1: the second replica finds the image", n)
	}
	st := e.status("web")
	if len(st) != 2 || st[0].State != "running" || st[0].ContainerID == "" || st[0].Error != "" {
		t.Fatalf("status: %+v", st)
	}
	if strings.Contains(e.logs.String(), "hello") {
		t.Fatal("an env value appeared in the log")
	}
}

func TestPublishedPortKeepsLoopbackDefault(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	c := e.running()["hivemind-web-0-g1"]
	if len(c.cfg.Ports) != 1 || c.cfg.Ports[0].HostIP != "127.0.0.1" {
		t.Fatalf("ports: %+v", c.cfg.Ports)
	}
}

func TestRemovesExtras(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.rt.add("hivemind-ghost-0-g1", "running", map[string]string{runtime.LabelApp: "ghost", runtime.LabelReplica: "0", runtime.LabelGeneration: "1"})
	e.rt.add("hivemind-web-7-g1", "exited", map[string]string{runtime.LabelApp: "web", runtime.LabelReplica: "7", runtime.LabelGeneration: "1"})
	e.rt.add("odd", "running", map[string]string{runtime.LabelApp: "web", runtime.LabelReplica: "x"})
	e.rt.add("not-ours", "running", map[string]string{"com.example": "1"})

	e.reconcile()
	snap := e.rt.snapshot()
	for _, n := range []string{"hivemind-ghost-0-g1", "hivemind-web-7-g1", "odd"} {
		if _, ok := snap[n]; ok {
			t.Errorf("extra container %s was not removed", n)
		}
	}
	if _, ok := snap["not-ours"]; !ok {
		t.Error("a container without hivemind labels was removed")
	}
	if _, ok := snap["hivemind-web-0-g1"]; !ok {
		t.Error("the wanted replica was not created")
	}
	c := e.rt.counts(false)
	if c["stop"] != 2 || c["remove"] != 3 {
		t.Errorf("stop %d remove %d, want 2 and 3: running extras are stopped first", c["stop"], c["remove"])
	}
}

func TestGenerationChangeReplaces(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	old := e.running()["hivemind-web-0-g1"]

	changed := web(1)
	changed.Env = map[string]string{"GREETING": "bonjour"}
	if a := e.put(changed); a.Generation != 2 {
		t.Fatalf("generation %d", a.Generation)
	}
	// The change starts a rollout. Until the store moves the replica to
	// generation 2, the agent must keep the old container.
	e.reconcile()
	if _, ok := e.running()["hivemind-web-0-g1"]; !ok {
		t.Fatal("old container removed before the rollout moved its replica")
	}
	if err := e.st.AdvanceRollouts(ctxT(e.t)); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	got := e.running()
	if len(got) != 1 {
		t.Fatalf("running after the change: %v", names(got))
	}
	c, ok := got["hivemind-web-0-g2"]
	if !ok || c.ID == old.ID || c.cfg.Env["GREETING"] != "bonjour" {
		t.Fatalf("replacement: %+v", c)
	}
	st := e.status("web")
	if len(st) != 1 || st[0].Generation != 2 {
		t.Fatalf("status: %+v", st)
	}
}

func TestScaleDown(t *testing.T) {
	e := newEnv(t)
	app := web(3)
	app.Ports = nil
	e.put(app)
	e.reconcile()
	if _, err := e.st.Scale(ctxT(t), "web", 1); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := names(e.running()); len(got) != 1 || got[0] != "hivemind-web-0-g1" {
		t.Fatalf("after scale down: %v", got)
	}
	// The removed replicas' rows are gone once the node reported.
	reps, _ := e.st.Replicas(ctxT(t), "web")
	if len(reps) != 1 {
		t.Fatalf("replica rows: %+v", reps)
	}
}

func TestRestartedAgentDoesNotTouchRunningContainers(t *testing.T) {
	e := newEnv(t)
	app := web(2)
	app.Ports = nil
	e.put(app)
	e.reconcile()
	before := e.running()
	e.rt.counts(true)

	// A new agent and a reopened store, on the same engine.
	e.st.Close()
	e.openStore()
	e.newAgent()
	e.reconcile()
	e.reconcile()

	c := e.rt.counts(false)
	for _, op := range []string{"create", "start", "stop", "remove", "pull"} {
		if c[op] != 0 {
			t.Errorf("restarted agent called %s %d times", op, c[op])
		}
	}
	after := e.running()
	if len(after) != 2 {
		t.Fatalf("running after restart: %v", names(after))
	}
	for n, c := range before {
		if after[n].ID != c.ID {
			t.Errorf("%s: ID changed across the restart", n)
		}
	}
}

func TestCreatedButNotStartedIsStarted(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	// The last agent created the container and stopped before starting it.
	e.rt.setFail(func(op, _ string) error {
		if op == "start" {
			return rtErr("start", runtime.ErrUnavailable)
		}
		return nil
	})
	if err := e.agent.Reconcile(ctxT(t)); err == nil {
		t.Fatal("Reconcile with a failing start returned nil")
	}
	snap := e.rt.snapshot()
	if c := snap["hivemind-web-0-g1"]; c.State != "created" {
		t.Fatalf("state %q, want created", c.State)
	}
	e.rt.setFail(nil)
	e.rt.counts(true)
	e.newAgent()
	e.reconcile()
	c := e.rt.counts(false)
	if c["create"] != 0 || c["start"] != 1 {
		t.Fatalf("create %d start %d: want the existing container started, not a new one", c["create"], c["start"])
	}
	if len(e.rt.snapshot()) != 1 {
		t.Fatal("a duplicate container was made")
	}
}

func TestRuntimeErrorReportedAndRetried(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.rt.setFail(func(op, _ string) error {
		if op == "pull" {
			return &runtime.Error{Op: "pull", Err: runtime.ErrUnavailable, Message: "registry timeout"}
		}
		return nil
	})
	err := e.agent.Reconcile(ctxT(t))
	if !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("Reconcile: %v, want ErrUnavailable", err)
	}
	st := e.status("web")
	if len(st) != 1 || st[0].State != "pending" || !strings.Contains(st[0].Error, "registry timeout") || st[0].ContainerID != "" {
		t.Fatalf("status after the failure: %+v", st)
	}

	e.rt.setFail(nil)
	e.reconcile()
	st = e.status("web")
	if len(st) != 1 || st[0].State != "running" || st[0].Error != "" {
		t.Fatalf("status after the retry: %+v", st)
	}
}

func TestListErrorReportsUnknownAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	e.rt.setFail(func(op, _ string) error {
		if op == "list" {
			return rtErr("list", runtime.ErrUnavailable)
		}
		return nil
	})
	e.rt.counts(true)
	if err := e.agent.Reconcile(ctxT(t)); !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("Reconcile: %v", err)
	}
	if c := e.rt.counts(false); c["create"]+c["remove"]+c["stop"]+c["start"] != 0 {
		t.Fatalf("calls while the engine was unreachable: %v", c)
	}
	st := e.status("web")
	if len(st) != 1 || st[0].State != "unknown" || st[0].Error == "" {
		t.Fatalf("status: %+v", st)
	}
}

func TestSourceErrorKeepsContainers(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	e.src.setFail(errors.New("server unreachable"))
	e.rt.counts(true)
	if err := e.agent.Reconcile(ctxT(t)); err == nil || !strings.Contains(err.Error(), "server unreachable") {
		t.Fatalf("Reconcile: %v", err)
	}
	if c := e.rt.counts(false); len(c) != 0 {
		t.Fatalf("the agent touched the engine without assignments: %v", c)
	}
	if len(e.running()) != 1 {
		t.Fatal("container stopped while the server was down")
	}
}

func TestRemovedOutOfBandIsReplaced(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	old := e.running()["hivemind-web-0-g1"]
	if err := e.rt.Remove(ctxT(t), old.ID, true); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	c, ok := e.running()["hivemind-web-0-g1"]
	if !ok || c.ID == old.ID {
		t.Fatalf("replacement: %+v", c)
	}
}

func TestExitedContainerIsStartedAgain(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	id := e.running()["hivemind-web-0-g1"].ID
	if err := e.rt.Stop(ctxT(t), id, 0); err != nil {
		t.Fatal(err)
	}
	// The first restart waits its 1 s of backoff.
	e.reconcile()
	if len(e.running()) != 0 {
		t.Fatal("restarted before its backoff ended")
	}
	e.clock.Advance(time.Second)
	e.reconcile()
	if c := e.running()["hivemind-web-0-g1"]; c.ID != id {
		t.Fatalf("exited container not started again in place: %+v", c)
	}
	if st := e.status("web"); len(st) != 1 || st[0].RestartCount != 1 || st[0].CrashLooping || !st[0].Ready {
		t.Fatalf("status after the restart: %+v", st)
	}
}

func TestDeleteAppRemovesContainersAndPurges(t *testing.T) {
	e := newEnv(t)
	app := web(2)
	app.Ports = nil
	e.put(app)
	e.reconcile()
	if err := e.st.DeleteApp(ctxT(t), "web"); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if snap := e.rt.snapshot(); len(snap) != 0 {
		t.Fatalf("containers left after delete: %v", names(snap))
	}
	if st := e.status("web"); len(st) != 0 {
		t.Fatalf("status after delete: %+v", st)
	}
	if reps, _ := e.st.Replicas(ctxT(t), "web"); len(reps) != 0 {
		t.Fatalf("replica rows after delete: %+v", reps)
	}
}

func TestFailedRemovalIsReportedAndRetried(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	if err := e.st.DeleteApp(ctxT(t), "web"); err != nil {
		t.Fatal(err)
	}
	e.rt.setFail(func(op, _ string) error {
		if op == "remove" {
			return rtErr("remove", runtime.ErrUnavailable)
		}
		return nil
	})
	if err := e.agent.Reconcile(ctxT(t)); err == nil {
		t.Fatal("Reconcile with a failing remove returned nil")
	}
	// The container still exists, so the replica stays marked for removal.
	if reps, _ := e.st.Replicas(ctxT(t), "web"); len(reps) != 1 || reps[0].Desired != store.Removed {
		t.Fatalf("replicas: %+v", reps)
	}
	if st := e.status("web"); len(st) != 1 || st[0].Error == "" {
		t.Fatalf("status: %+v", st)
	}
	e.rt.setFail(nil)
	e.reconcile()
	if reps, _ := e.st.Replicas(ctxT(t), "web"); len(reps) != 0 {
		t.Fatalf("replicas after the retry: %+v", reps)
	}
}

func TestDuplicateContainersCollapse(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	labels := map[string]string{runtime.LabelApp: "web", runtime.LabelReplica: "0", runtime.LabelGeneration: "1"}
	keep := e.rt.add("copy-a", "running", labels)
	e.rt.add("copy-b", "exited", labels)
	e.reconcile()
	snap := e.rt.snapshot()
	if len(snap) != 1 || snap["copy-a"].ID != keep {
		t.Fatalf("after collapse: %v", names(snap))
	}
	if c := e.rt.counts(false); c["create"] != 0 {
		t.Fatal("a new container was made though one was running")
	}
}

func TestNameConflictAdoptsMatchingContainer(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	// The container appears after the list was read: List misses it, Create
	// then hits the name.
	labels := map[string]string{runtime.LabelApp: "web", runtime.LabelReplica: "0", runtime.LabelGeneration: "1"}
	e.rt.mu.Lock()
	e.rt.images["docker.io/library/nginx:alpine"] = true
	e.rt.mu.Unlock()
	created := false
	e.rt.setFail(func(op, _ string) error {
		if op == "create" && !created {
			created = true
			// The hook runs with the fake's lock held, so insert directly.
			e.rt.containers["adopted"] = &fakeContainer{Container: runtime.Container{ID: "adopted", Name: "hivemind-web-0-g1", State: "created", Labels: labels}}
		}
		return nil
	})
	e.reconcile()
	snap := e.rt.snapshot()
	if len(snap) != 1 || snap["hivemind-web-0-g1"].ID != "adopted" || snap["hivemind-web-0-g1"].State != "running" {
		t.Fatalf("after conflict: %+v", snap)
	}
}

func TestRunAndTrigger(t *testing.T) {
	e := newEnv(t)
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Interval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// The first pass runs at once; with nothing assigned it lists and reports.
	waitFor(t, func() bool { return e.rt.counts(false)["list"] >= 1 })
	e.put(web(1))
	a.Trigger()
	waitFor(t, func() bool { return len(e.running()) == 1 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunRetriesAfterError(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	var mu sync.Mutex
	failures := 2
	e.rt.setFail(func(op, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		if op == "create" && failures > 0 {
			failures--
			return rtErr("create", runtime.ErrUnavailable)
		}
		return nil
	})
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Interval: 20 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	waitFor(t, func() bool { return len(e.running()) == 1 })
	if !strings.Contains(e.logs.String(), "reconcile pass failed") {
		t.Fatal("the failed passes were not logged")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A start refused with a conflict (a host port already allocated) must not
// leave a container that a later pass could start without its ports: the
// container is removed, the replica is reported pending with the error, and
// the next pass creates a fresh one.
func TestStartConflictDiscardsContainer(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.rt.setFail(func(op, _ string) error {
		if op == "start" {
			return &runtime.Error{Op: "start", Err: runtime.ErrConflict, Message: "port is already allocated"}
		}
		return nil
	})
	if err := e.agent.Reconcile(ctxT(t)); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("Reconcile: %v, want ErrConflict", err)
	}
	if snap := e.rt.snapshot(); len(snap) != 0 {
		t.Fatalf("containers left after a refused start: %v", names(snap))
	}
	st := e.status("web")
	if len(st) != 1 || st[0].State != "pending" || st[0].ContainerID != "" || !strings.Contains(st[0].Error, "port is already allocated") {
		t.Fatalf("status after the refused start: %+v", st)
	}

	// Still refused on the next pass: still nothing left behind.
	_ = e.agent.Reconcile(ctxT(t))
	if snap := e.rt.snapshot(); len(snap) != 0 {
		t.Fatalf("containers left after a second refused start: %v", names(snap))
	}

	e.rt.setFail(nil)
	e.rt.counts(true)
	e.reconcile()
	c := e.rt.counts(false)
	snap := e.rt.snapshot()
	if c["create"] != 1 || len(snap) != 1 || snap["hivemind-web-0-g1"].State != "running" {
		t.Fatalf("after the port is free: create %d, containers %v", c["create"], names(snap))
	}
}

// An exited container whose restart is refused with a conflict is removed,
// not kept for a later start without its ports.
func TestRestartConflictDiscardsContainer(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	e.rt.exit("hivemind-web-0-g1", 1)
	e.clock.Advance(time.Minute)
	e.rt.setFail(func(op, _ string) error {
		if op == "start" {
			return &runtime.Error{Op: "start", Err: runtime.ErrConflict, Message: "port is already allocated"}
		}
		return nil
	})
	_ = e.agent.Reconcile(ctxT(t))
	if snap := e.rt.snapshot(); len(snap) != 0 {
		t.Fatalf("containers left after a refused restart: %v", names(snap))
	}
	if st := e.status("web"); len(st) != 1 || st[0].State != "pending" || st[0].ContainerID != "" {
		t.Fatalf("status after the refused restart: %+v", st)
	}
}
