package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// fakeClock is the store's time source in tests. It moves only when a test
// moves it.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

const nodeB = "node-b"

// rolloutEnv is a store with a fake clock and the given nodes registered.
type rolloutEnv struct {
	t     *testing.T
	s     *Store
	clock *fakeClock
	ctx   context.Context
}

// placeBlockerOnB puts the app "blocker" on nodeB and every other app on the
// first node, so a test can choose where a conflicting app runs.
func placeBlockerOnB(nodes []Node, placed []Assignment, app spec.App, idx int, now time.Time) (string, error) {
	if app.Name == "blocker" {
		return nodeB, nil
	}
	return FirstNode(nodes, placed, app, idx, now)
}

func newRolloutEnv(t *testing.T, nodes ...string) *rolloutEnv {
	t.Helper()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), WithClock(clock.Now), WithPlacement(placeBlockerOnB))
	e := &rolloutEnv{t: t, s: s, clock: clock, ctx: ctxT(t)}
	for _, id := range nodes {
		e.renew(id)
	}
	return e
}

// renew registers a node, or renews its lease at the fake clock's time.
func (e *rolloutEnv) renew(id string) {
	e.t.Helper()
	if err := e.s.UpsertNode(e.ctx, Node{ID: id, Name: id, CPU: 4, Memory: 8 << 30}); err != nil {
		e.t.Fatal(err)
	}
}

// report replaces what node reports with sts.
func (e *rolloutEnv) report(node string, sts ...Status) {
	e.t.Helper()
	if err := e.s.ReportStatus(e.ctx, node, sts); err != nil {
		e.t.Fatal(err)
	}
}

// container is a status for one replica's container at gen, running and Ready.
func container(app string, idx, gen int) Status {
	return Status{Name: fmt.Sprintf("hivemind-%s-%d-g%d", app, idx, gen), App: app, Replica: idx,
		Generation: gen, State: "running", Ready: true}
}

func (e *rolloutEnv) advance() {
	e.t.Helper()
	if err := e.s.AdvanceRollouts(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// gens returns the generation of each replica of app, by index.
func (e *rolloutEnv) gens(app string) []int {
	e.t.Helper()
	var out []int
	for _, r := range replicasOf(e.t, e.s, app) {
		out = append(out, r.Generation)
	}
	return out
}

// envA returns the A variable of the spec each assignment on node runs with,
// in replica order.
func (e *rolloutEnv) envA(node string) []string {
	e.t.Helper()
	as, err := e.s.AssignmentsForNode(e.ctx, node)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, a := range as {
		out = append(out, a.Spec.Env["A"])
	}
	return out
}

// nodesOf returns the node of each replica of app, by index.
func (e *rolloutEnv) nodesOf(app string) []string {
	e.t.Helper()
	var out []string
	for _, r := range replicasOf(e.t, e.s, app) {
		out = append(out, r.NodeID)
	}
	return out
}

func (e *rolloutEnv) rollout(app string) Rollout {
	e.t.Helper()
	ro, err := e.s.GetRollout(e.ctx, app)
	if err != nil {
		e.t.Fatal(err)
	}
	return ro
}

func (e *rolloutEnv) put(a spec.App) {
	e.t.Helper()
	if _, err := e.s.PutApp(e.ctx, a); err != nil {
		e.t.Fatal(err)
	}
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// withEnv returns web(replicas) with one env value, so each value is a new
// template and so a new generation.
func withEnv(replicas int, v string) spec.App {
	a := web(replicas)
	a.Env = map[string]string{"A": v}
	return a
}

func TestRolloutMovesOneReplicaAtATimeOnOneNode(t *testing.T) {
	e := newRolloutEnv(t, node1)
	e.put(web(3))
	e.put(withEnv(3, "2"))
	if g := e.gens("web"); !eqInts(g, []int{1, 1, 1}) {
		t.Fatalf("before any step: %v", g)
	}
	if ro := e.rollout("web"); ro.State != RolloutInProgress || ro.Target != 2 || ro.Moving != -1 {
		t.Fatalf("rollout: %+v", ro)
	}

	// Replica 0 moves, and nothing else moves until it reports Ready.
	e.advance()
	if g := e.gens("web"); !eqInts(g, []int{2, 1, 1}) {
		t.Fatalf("first move: %v", g)
	}
	if env := e.envA(node1); !eqStrings(env, []string{"2", "1", "1"}) {
		t.Fatalf("moved replica runs the new spec and the others the old one: %v", env)
	}
	if ro := e.rollout("web"); ro.Moving != 0 || ro.Since.IsZero() {
		t.Fatalf("moving: %+v", ro)
	}
	e.advance()
	if g := e.gens("web"); !eqInts(g, []int{2, 1, 1}) {
		t.Fatalf("waiting for replica 0: %v", g)
	}

	e.report(node1, container("web", 0, 2))
	e.advance()
	if g := e.gens("web"); !eqInts(g, []int{2, 2, 1}) {
		t.Fatalf("after replica 0 is ready: %v", g)
	}
	e.report(node1, container("web", 1, 2))
	e.advance()
	if g := e.gens("web"); !eqInts(g, []int{2, 2, 2}) {
		t.Fatalf("after replica 1 is ready: %v", g)
	}
	e.report(node1, container("web", 2, 2))
	e.advance()
	if ro := e.rollout("web"); ro.State != RolloutDone || ro.Moving != -1 || ro.Reason != "" {
		t.Fatalf("done: %+v", ro)
	}
}

func TestRolloutNotReadyIsNotAFailureBeforeTheDeadline(t *testing.T) {
	e := newRolloutEnv(t, node1)
	e.put(web(2))
	e.put(withEnv(2, "2"))
	e.advance()
	starting := container("web", 0, 2)
	starting.Ready = false
	starting.Health = "starting"
	e.report(node1, starting)
	e.clock.Advance(time.Minute)
	e.advance()
	if ro := e.rollout("web"); ro.State != RolloutInProgress || ro.Moving != 0 {
		t.Fatalf("within the deadline: %+v", ro)
	}
	if g := e.gens("web"); !eqInts(g, []int{2, 1}) {
		t.Fatalf("gens: %v", g)
	}
}

func TestRolloutStopsOnCrashLoop(t *testing.T) {
	e := newRolloutEnv(t, node1)
	e.put(web(3))
	e.put(withEnv(3, "2"))
	e.advance()
	crashing := container("web", 0, 2)
	crashing.Ready, crashing.State, crashing.CrashLooping, crashing.RestartCount = false, "exited", true, 3
	e.report(node1, crashing)
	e.advance()

	ro := e.rollout("web")
	want := "rollout to generation 2 stopped: replica 0 is crash-looping"
	if ro.State != RolloutPaused || ro.Reason != want || ro.Moving != -1 {
		t.Fatalf("paused: %+v", ro)
	}
	// Replica 0 goes back to generation 1, and the others were never touched.
	if g := e.gens("web"); !eqInts(g, []int{1, 1, 1}) {
		t.Fatalf("after the stop: %v", g)
	}
	// Every replica runs its own generation's spec: the reverted replica
	// does not pick up the new env.
	if env := e.envA(node1); !eqStrings(env, []string{"1", "1", "1"}) {
		t.Fatalf("specs after the stop: %v", env)
	}
	// A paused rollout does nothing on later calls, even after the deadline.
	e.clock.Advance(time.Hour)
	e.advance()
	e.advance()
	if g := e.gens("web"); !eqInts(g, []int{1, 1, 1}) {
		t.Fatalf("paused rollout moved: %v", g)
	}
	if ro2 := e.rollout("web"); ro2.State != RolloutPaused || ro2.Reason != want {
		t.Fatalf("paused rollout changed: %+v", ro2)
	}
}

func TestRolloutStopsOnTimeoutWithoutHealthCheck(t *testing.T) {
	e := newRolloutEnv(t, node1)
	e.put(web(2))
	e.put(withEnv(2, "2"))
	e.advance()
	e.clock.Advance(DefaultRolloutDeadline - time.Second)
	e.advance()
	if ro := e.rollout("web"); ro.State != RolloutInProgress {
		t.Fatalf("one second before the deadline: %+v", ro)
	}
	e.clock.Advance(time.Second)
	e.advance()
	ro := e.rollout("web")
	want := "rollout to generation 2 stopped: replica 0 is not ready after 2m0s"
	if ro.State != RolloutPaused || ro.Reason != want {
		t.Fatalf("at the deadline: %+v", ro)
	}
	if g := e.gens("web"); !eqInts(g, []int{1, 1}) {
		t.Fatalf("after the timeout: %v", g)
	}
}

func TestRolloutDeadlineFromHealthCheck(t *testing.T) {
	a := web(1)
	a.Health = &spec.Health{HTTP: &spec.HTTPCheck{Port: 80}, Interval: spec.Duration(5 * time.Second),
		Retries: 3, StartPeriod: spec.Duration(10 * time.Second)}
	// 10 s start period, 3 retries at 5 s, and the 30 s margin.
	if d := RolloutDeadline(a); d != 55*time.Second {
		t.Fatalf("deadline %v, want 55s", d)
	}
	if d := RolloutDeadline(web(1)); d != DefaultRolloutDeadline {
		t.Fatalf("no health check: %v", d)
	}
}

func TestNewDeployDuringStoppedRolloutStartsFromCurrentState(t *testing.T) {
	e := newRolloutEnv(t, node1)
	e.put(web(3))
	e.put(withEnv(3, "2"))
	e.advance()
	crashing := container("web", 0, 2)
	crashing.Ready, crashing.CrashLooping = false, true
	e.report(node1, crashing)
	e.advance()
	if ro := e.rollout("web"); ro.State != RolloutPaused {
		t.Fatalf("setup: %+v", ro)
	}

	// Deploying a third template starts a new rollout to generation 3. Every
	// replica is still at 1, so the rollout starts from there.
	e.put(withEnv(3, "3"))
	ro := e.rollout("web")
	if ro.State != RolloutInProgress || ro.Target != 3 || ro.Reason != "" {
		t.Fatalf("new rollout: %+v", ro)
	}
	e.advance()
	e.report(node1, container("web", 0, 3))
	e.advance()
	e.report(node1, container("web", 1, 3))
	e.advance()
	e.report(node1, container("web", 2, 3))
	e.advance()
	if ro := e.rollout("web"); ro.State != RolloutDone || ro.Target != 3 {
		t.Fatalf("done: %+v", ro)
	}
	if g := e.gens("web"); !eqInts(g, []int{3, 3, 3}) {
		t.Fatalf("gens: %v", g)
	}
}

func TestRollbackIsADeployOfTheOldSpec(t *testing.T) {
	e := newRolloutEnv(t, node1)
	e.put(web(1))
	e.put(withEnv(1, "2"))
	e.advance()
	e.report(node1, container("web", 0, 2))
	e.advance()
	// Deploying the original spec again is generation 3, not a return to 1.
	e.put(web(1))
	if ro := e.rollout("web"); ro.Target != 3 || ro.State != RolloutInProgress {
		t.Fatalf("rollback: %+v", ro)
	}
}

func TestSurgeRunsNewCopyOnAnotherNodeAndOldStaysUntilMoved(t *testing.T) {
	e := newRolloutEnv(t, node1, nodeB)
	e.put(web(2)) // placed on node1 by FirstNode
	e.put(withEnv(2, "2"))
	if got := e.nodesOf("web"); !eqStrings(got, []string{node1, node1}) {
		t.Fatalf("placement: %v", got)
	}

	// Replica 0 gets a surge on nodeB. Its old copy keeps running on node1.
	e.advance()
	if g, n := e.gens("web"), e.nodesOf("web"); !eqInts(g, []int{1, 1}) || !eqStrings(n, []string{node1, node1}) {
		t.Fatalf("surge placed: gens %v nodes %v", g, n)
	}
	onB, err := e.s.AssignmentsForNode(e.ctx, nodeB)
	if err != nil || len(onB) != 1 || onB[0].Index != 0 || onB[0].Generation != 2 || onB[0].Desired != Running {
		t.Fatalf("surge on nodeB: %+v %v", onB, err)
	}
	onA, _ := e.s.AssignmentsForNode(e.ctx, node1)
	if len(onA) != 2 || onA[0].Generation != 1 {
		t.Fatalf("node1 still runs the old copy: %+v", onA)
	}

	// While the surge is not Ready, the old copy is still the replica.
	e.clock.Advance(time.Second)
	e.advance()
	if got := e.nodesOf("web"); !eqStrings(got, []string{node1, node1}) {
		t.Fatalf("moved before ready: %v", got)
	}

	// Ready on nodeB: the replica moves there and the old copy is no longer
	// assigned to node1.
	e.report(nodeB, container("web", 0, 2))
	e.advance()
	if got := e.nodesOf("web"); !eqStrings(got, []string{nodeB, node1}) {
		t.Fatalf("after ready: %v", got)
	}
	if g := e.gens("web"); !eqInts(g, []int{2, 1}) {
		t.Fatalf("gens after surge promotion: %v", g)
	}
	onA, _ = e.s.AssignmentsForNode(e.ctx, node1)
	for _, a := range onA {
		if a.Index == 0 {
			t.Fatalf("node1 still assigned replica 0: %+v", a)
		}
	}
	// Replica 1's move starts in the same call, so its surge is expected. Replica
	// 0's surge row must be gone.
	surges, _ := querySurges(e.ctx, e.s.db, `1`)
	for _, sg := range surges {
		if sg.Index == 0 {
			t.Fatalf("surge row for replica 0 left: %+v", sg)
		}
	}
}

func TestSurgeFailureRemovesSurgeAndKeepsOldCopy(t *testing.T) {
	e := newRolloutEnv(t, node1, nodeB)
	e.put(web(2))
	e.put(withEnv(2, "2"))
	e.advance()
	bad := container("web", 0, 2)
	bad.Ready, bad.CrashLooping = false, true
	e.report(nodeB, bad)
	e.advance()

	ro := e.rollout("web")
	if ro.State != RolloutPaused || ro.Reason != "rollout to generation 2 stopped: replica 0 is crash-looping" {
		t.Fatalf("paused: %+v", ro)
	}
	if got := e.nodesOf("web"); !eqStrings(got, []string{node1, node1}) {
		t.Fatalf("replica moved: %v", got)
	}
	// The surge is marked for removal: nodeB's agent removes it, and the old
	// copy keeps running on node1.
	onB, _ := e.s.AssignmentsForNode(e.ctx, nodeB)
	if len(onB) != 1 || onB[0].Desired != Removed {
		t.Fatalf("surge not marked for removal: %+v", onB)
	}
	if g := e.gens("web"); !eqInts(g, []int{1, 1}) {
		t.Fatalf("gens: %v", g)
	}
}

func TestSurgeSkippedWhenAnotherNodeHasTheHostPort(t *testing.T) {
	e := newRolloutEnv(t, node1, nodeB)
	// The blocker publishes 8080 and is placed on nodeB, so nodeB can't take
	// a second copy of web, which publishes 8080 too.
	e.put(spec.App{Name: "blocker", Image: "busybox", Replicas: 1, Ports: []spec.Port{{Host: 8080, Container: 80}}})
	ported := web(2)
	ported.Ports = []spec.Port{{Host: 8080, Container: 80}}
	e.put(ported)
	ported.Env = map[string]string{"A": "2"}
	e.put(ported)
	e.advance()
	if g := e.gens("web"); !eqInts(g, []int{2, 1}) {
		t.Fatalf("expected an in-place move, gens %v", g)
	}
	if surges, _ := querySurges(e.ctx, e.s.db, `1`); len(surges) != 0 {
		t.Fatalf("surge placed on a node with the host port in use: %+v", surges)
	}
}

func TestSingleNodeFallsBackToInPlaceForHostPorts(t *testing.T) {
	e := newRolloutEnv(t, node1)
	ported := web(2)
	ported.Ports = []spec.Port{{Host: 8080, Container: 80}}
	e.put(ported)
	ported.Env = map[string]string{"A": "2"}
	e.put(ported)
	e.advance()
	// No other node: the replica's generation changes, and nothing is surged.
	if g := e.gens("web"); !eqInts(g, []int{2, 1}) {
		t.Fatalf("in place: %v", g)
	}
	surges, _ := querySurges(e.ctx, e.s.db, `1`)
	if len(surges) != 0 {
		t.Fatalf("surge on a single node: %+v", surges)
	}
}
