package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

const (
	nodeA = "node-a"
	nodeC = "node-c"
)

// drainEnv is a rolloutEnv whose store can be closed and opened again, as a
// restart of the server, and whose nodes take replicas at once.
type drainEnv struct {
	*rolloutEnv
	path  string
	opts  []Option
	nodes []string
}

// newDrainEnv opens a store with a fake clock and registers nodes. A nil
// place means the scheduler.
func newDrainEnv(t *testing.T, place PlaceFunc, nodes ...string) *drainEnv {
	t.Helper()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	path := filepath.Join(t.TempDir(), "data", "hivemind.db")
	opts := []Option{WithClock(clock.Now), WithMinReady(0)}
	if place != nil {
		opts = append(opts, WithPlacement(place))
	}
	e := &drainEnv{rolloutEnv: &rolloutEnv{t: t, s: openT(t, path, opts...), clock: clock, ctx: ctxT(t)}, path: path, opts: opts, nodes: nodes}
	for _, id := range nodes {
		e.renew(id)
	}
	return e
}

// reopen closes the store and opens it again, as a restart of the server.
func (e *drainEnv) reopen() {
	e.t.Helper()
	if err := e.s.Close(); err != nil {
		e.t.Fatal(err)
	}
	e.s = openT(e.t, e.path, e.opts...)
}

// tick moves the clock and renews every node's lease, so no node is lost.
func (e *drainEnv) tick(d time.Duration) {
	e.t.Helper()
	e.clock.Advance(d)
	for _, id := range e.nodes {
		e.renew(id)
	}
}

func (e *drainEnv) drain(node string, deadline time.Duration) Drain {
	e.t.Helper()
	d, err := e.s.DrainNode(e.ctx, node, deadline)
	if err != nil {
		e.t.Fatal(err)
	}
	checkInvariants(e.t, e.s)
	return d
}

// step advances every drain, with no floor on the start of moves.
func (e *drainEnv) step() []Drain { return e.stepSince(time.Time{}) }

func (e *drainEnv) stepSince(since time.Time) []Drain {
	e.t.Helper()
	done, err := e.s.AdvanceDrains(e.ctx, since)
	if err != nil {
		e.t.Fatal(err)
	}
	checkInvariants(e.t, e.s)
	return done
}

func (e *drainEnv) getDrain(node string) Drain {
	e.t.Helper()
	d, err := e.s.GetDrain(e.ctx, node)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *drainEnv) setEligible(node string, eligible bool) Eligibility {
	e.t.Helper()
	el, err := e.s.SetNodeEligibility(e.ctx, node, eligible)
	if err != nil {
		e.t.Fatal(err)
	}
	checkInvariants(e.t, e.s)
	return el
}

// surgesOn returns "app/idx@gen desired" for each surge on node.
func (e *drainEnv) surgesOn(node string) []string {
	e.t.Helper()
	ss, err := querySurges(e.ctx, e.s.db, `s.node_id = ?`, node)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, s := range ss {
		out = append(out, fmt.Sprintf("%s/%d@%d %s", s.App, s.Index, s.Generation, s.Desired))
	}
	return out
}

// moving returns "app/idx>node" for each move in flight.
func moving(d Drain) []string {
	var out []string
	for _, m := range d.Moving {
		out = append(out, fmt.Sprintf("%s/%d>%s", m.App, m.Index, m.To))
	}
	return out
}

// blocked returns "app/idx: reason" for each replica that stays.
func blocked(d Drain) []string {
	var out []string
	for _, b := range d.Blocked {
		out = append(out, fmt.Sprintf("%s/%d: %s", b.App, b.Index, b.Reason))
	}
	return out
}

func app(name string, replicas int) spec.App {
	return spec.App{Name: name, Image: "busybox", Replicas: replicas}
}

func TestNodeEligibility(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB)
	e.put(web(2))
	if got := e.nodesOf("web"); !eqStrings(got, []string{nodeA, nodeA}) {
		t.Fatalf("placement: %v", got)
	}
	el := e.setEligible(nodeA, false)
	if el.ID != nodeA || el.Name != nodeA || el.Eligible || el.DrainCancelled {
		t.Fatalf("eligibility: %+v", el)
	}

	// The node keeps its replicas, and new ones go elsewhere.
	if _, err := e.s.Scale(e.ctx, "web", 3); err != nil {
		t.Fatal(err)
	}
	e.put(app("api", 1))
	if got := e.nodesOf("web"); !eqStrings(got, []string{nodeA, nodeA, nodeB}) {
		t.Fatalf("web after a scale up: %v", got)
	}
	if got := e.nodesOf("api"); !eqStrings(got, []string{nodeB}) {
		t.Fatalf("api: %v", got)
	}

	// A lapsed lease that comes back, and a restart of the server, keep it.
	e.clock.Advance(time.Minute)
	e.renew(nodeA)
	e.reopen()
	if n, err := e.s.GetNode(e.ctx, nodeA); err != nil || n.Eligible {
		t.Fatalf("node after its lease came back and a restart: %+v, %v", n, err)
	}
	if n, err := e.s.GetNode(e.ctx, nodeB); err != nil || !n.Eligible {
		t.Fatalf("the other node: %+v, %v", n, err)
	}

	// A rollout does not surge onto an ineligible node: api's replica on
	// node-b is replaced in place, though node-a is ready and has room.
	api := app("api", 1)
	api.Env = map[string]string{"A": "2"}
	e.put(api)
	e.advance()
	if g := e.gens("api"); !eqInts(g, []int{2}) {
		t.Fatalf("api did not move in place: %v", g)
	}
	if s := append(e.surgesOn(nodeA), e.surgesOn(nodeB)...); len(s) != 0 {
		t.Fatalf("surges: %v", s)
	}

	if _, err := e.s.SetNodeEligibility(e.ctx, "node-x", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
}

// With the scheduler, a replica that no node takes says the nodes are
// ineligible, and making one eligible places it.
func TestIneligibleNodesLeaveReplicasPending(t *testing.T) {
	e := newDrainEnv(t, nil, nodeA)
	e.setEligible(nodeA, false)
	e.put(web(1))
	r := replicasOf(t, e.s, "web")[0]
	if r.NodeID != "" || r.PendingReason != "every ready node is ineligible for new replicas" {
		t.Fatalf("replica on an ineligible cluster: %+v", r)
	}
	e.setEligible(nodeA, true)
	if r := replicasOf(t, e.s, "web")[0]; r.NodeID != nodeA || r.PendingReason != "" {
		t.Fatalf("replica after the node was made eligible: %+v", r)
	}
}

// A placement function that picks an ineligible node is not obeyed.
func TestPlacementOnAnIneligibleNodeIsRefused(t *testing.T) {
	e := newDrainEnv(t, func([]Node, []Assignment, spec.App, int, time.Time) (string, error) { return nodeA, nil }, nodeA)
	e.setEligible(nodeA, false)
	e.put(web(1))
	if r := replicasOf(t, e.s, "web")[0]; r.NodeID != "" || !strings.Contains(r.PendingReason, "ineligible") {
		t.Fatalf("replica: %+v", r)
	}
}

func TestDrainSurgesBeforeItStopsAnything(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB)
	e.put(web(2))
	e.put(app("api", 1))
	e.report(nodeA, container("api", 0, 1), container("web", 0, 1), container("web", 1, 1))
	start := e.clock.t

	// One move per app, the two apps at once. Nothing has left node-a: each
	// copy runs beside its replica.
	d := e.drain(nodeA, 0)
	if d.State != DrainDraining || !d.StartedAt.Equal(start) || !d.Deadline.Equal(start.Add(DefaultDrainDeadline)) || !d.FinishedAt.IsZero() {
		t.Fatalf("drain: %+v", d)
	}
	if got := moving(d); !eqStrings(got, []string{"api/0>" + nodeB, "web/0>" + nodeB}) || d.Remaining != 3 || len(d.Blocked) != 0 {
		t.Fatalf("first step: moving %v, %+v", got, d)
	}
	if n, _ := e.s.GetNode(e.ctx, nodeA); n.Eligible {
		t.Fatal("a draining node is eligible")
	}
	if got := e.surgesOn(nodeB); !eqStrings(got, []string{"api/0@1 running", "web/0@1 running"}) {
		t.Fatalf("surges on node-b: %v", got)
	}
	if got := e.nodesOf("web"); !eqStrings(got, []string{nodeA, nodeA}) {
		t.Fatalf("web moved before its copy was ready: %v", got)
	}

	// Copies that are starting do not move anything.
	starting := container("web", 0, 1)
	starting.Ready, starting.Health = false, "starting"
	e.report(nodeB, starting)
	e.tick(time.Second)
	e.step()
	if got := e.nodesOf("web"); !eqStrings(got, []string{nodeA, nodeA}) {
		t.Fatalf("web moved while its copy was starting: %v", got)
	}

	// Ready copies take their replicas, and web's next move starts.
	e.report(nodeB, container("api", 0, 1), container("web", 0, 1))
	if done := e.step(); len(done) != 0 {
		t.Fatalf("finished early: %+v", done)
	}
	if w, a := e.nodesOf("web"), e.nodesOf("api"); !eqStrings(w, []string{nodeB, nodeA}) || !eqStrings(a, []string{nodeB}) {
		t.Fatalf("after the first moves: web %v, api %v", w, a)
	}
	d = e.getDrain(nodeA)
	if d.Moved != 2 || d.Remaining != 1 || d.OldContainers != 2 || !eqStrings(moving(d), []string{"web/1>" + nodeB}) {
		t.Fatalf("after the first moves: %+v", d)
	}
	// The old copies are no longer node-a's assignments, so its agent removes
	// them; the replica still there is.
	as, _ := e.s.AssignmentsForNode(e.ctx, nodeA)
	if len(as) != 1 || as[0].App != "web" || as[0].Index != 1 {
		t.Fatalf("node-a assignments: %+v", as)
	}

	e.report(nodeA, container("web", 1, 1))
	e.report(nodeB, container("api", 0, 1), container("web", 0, 1), container("web", 1, 1))
	e.step()
	if d := e.getDrain(nodeA); d.State != DrainDraining || d.Moved != 3 || d.Remaining != 0 || d.OldContainers != 1 {
		t.Fatalf("waiting for node-a to remove the last copy: %+v", d)
	}

	// Done once node-a reports nothing left.
	e.report(nodeA)
	done := e.step()
	if len(done) != 1 || done[0].NodeID != nodeA || done[0].State != DrainDone || done[0].Moved != 3 || done[0].Stopped != 0 ||
		!done[0].FinishedAt.Equal(e.clock.t) {
		t.Fatalf("finished: %+v", done)
	}
	if ds, err := e.s.ListDrains(e.ctx); err != nil || len(ds) != 1 || ds[0].State != DrainDone {
		t.Fatalf("ListDrains: %+v, %v", ds, err)
	}
	if done := e.step(); len(done) != 0 {
		t.Fatalf("a finished drain finished again: %+v", done)
	}

	// The node stays in the cluster, ineligible, and its removal needs no
	// force because nothing runs on it.
	if n, _ := e.s.GetNode(e.ctx, nodeA); n.Eligible {
		t.Fatal("a drained node is eligible")
	}
	r, err := e.s.RemoveNode(e.ctx, nodeA, false)
	if err != nil || r.Rescheduled != 0 {
		t.Fatalf("removing the drained node: %+v, %v", r, err)
	}
	checkInvariants(t, e.s)
}

func TestDrainLeavesAVolumeReplica(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB)
	e.put(dbApp())
	e.put(web(1))
	e.report(nodeA, container("db", 0, 1), container("web", 0, 1))
	d := e.drain(nodeA, time.Hour)
	want := "db/0: it has a volume, and its data is on this node"
	if !eqStrings(blocked(d), []string{want}) || !eqStrings(moving(d), []string{"web/0>" + nodeB}) {
		t.Fatalf("first step: blocked %v, moving %v", blocked(d), moving(d))
	}
	if got := e.surgesOn(nodeB); !eqStrings(got, []string{"web/0@1 running"}) {
		t.Fatalf("surges: %v", got)
	}
	e.report(nodeB, container("web", 0, 1))
	e.step()
	e.report(nodeA, container("db", 0, 1))
	done := e.step()
	if len(done) != 1 || done[0].Remaining != 1 || done[0].Moved != 1 || !eqStrings(blocked(done[0]), []string{want}) {
		t.Fatalf("finished before the deadline with the volume replica left: %+v", done)
	}
	if got := e.nodesOf("db"); !eqStrings(got, []string{nodeA}) {
		t.Fatalf("db: %v", got)
	}
	// Something still runs on the node, so its removal needs force.
	if _, err := e.s.RemoveNode(e.ctx, nodeA, false); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "drain it first") {
		t.Fatalf("removing a drained node that runs a volume replica: %v", err)
	}
}

// A replica whose host port is taken on every other node stays, with the
// reason. At the deadline it loses the node and waits, pending, as after a
// node loss.
func TestDrainHostPortWithNoRoomAndTheDeadline(t *testing.T) {
	e := newDrainEnv(t, nil, nodeA, nodeB)
	ported := func(name string) spec.App {
		return spec.App{Name: name, Image: "nginx:alpine", Replicas: 1, Ports: []spec.Port{{Host: 8080, Container: 80}}}
	}
	e.put(ported("blocker")) // node-a: the scheduler breaks the tie by ID
	e.put(ported("web"))     // node-b: the port is taken on node-a
	e.put(app("spread", 2))  // one on each node
	if b, w, s := e.nodesOf("blocker"), e.nodesOf("web"), e.nodesOf("spread"); !eqStrings(b, []string{nodeA}) ||
		!eqStrings(w, []string{nodeB}) || !eqStrings(s, []string{nodeA, nodeB}) {
		t.Fatalf("placement: blocker %v, web %v, spread %v", b, w, s)
	}
	e.report(nodeA, container("blocker", 0, 1), container("spread", 0, 1))
	e.report(nodeB, container("spread", 1, 1), container("web", 0, 1))

	d := e.drain(nodeB, 5*time.Minute)
	if !eqStrings(blocked(d), []string{"web/0: host port 127.0.0.1:8080/tcp is in use on every other eligible node"}) ||
		!eqStrings(moving(d), []string{"spread/1>" + nodeA}) {
		t.Fatalf("first step: blocked %v, moving %v", blocked(d), moving(d))
	}
	e.report(nodeA, container("blocker", 0, 1), container("spread", 0, 1), container("spread", 1, 1))
	e.step()
	e.report(nodeB, container("web", 0, 1))
	e.tick(time.Minute)
	if done := e.step(); len(done) != 0 {
		t.Fatalf("finished with web still on the node: %+v", done)
	}
	if d := e.getDrain(nodeB); d.Moved != 1 || d.Remaining != 1 || len(d.Blocked) != 1 {
		t.Fatalf("before the deadline: %+v", d)
	}

	e.tick(4 * time.Minute)
	done := e.step()
	if len(done) != 1 || done[0].Stopped != 1 || done[0].Moved != 1 || done[0].Remaining != 0 || len(done[0].Blocked) != 0 {
		t.Fatalf("at the deadline: %+v", done)
	}
	r := replicasOf(t, e.s, "web")[0]
	if r.NodeID != "" || r.PendingReason != "host port 127.0.0.1:8080/tcp is in use on every eligible node" {
		t.Fatalf("web after the deadline: %+v", r)
	}
	if as, _ := e.s.AssignmentsForNode(e.ctx, nodeB); len(as) != 0 {
		t.Fatalf("node-b still has assignments, so its agent would not stop them: %+v", as)
	}
}

// At the deadline a copy that is still starting takes its replica at once. A
// replica whose copy failed is not tried again, and at the deadline it is
// placed again like a new replica.
func TestDrainDeadlineEndsMovesInFlight(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB)
	e.put(web(1))
	e.put(app("api", 1))
	e.drain(nodeA, 90*time.Second)
	bad := container("api", 0, 1)
	bad.Ready, bad.State, bad.CrashLooping = false, "exited", true
	e.report(nodeB, bad)
	e.step()
	d := e.getDrain(nodeA)
	if !eqStrings(blocked(d), []string{"api/0: its copy on node node-b was crash-looping; it stays until the deadline"}) ||
		!eqStrings(moving(d), []string{"web/0>" + nodeB}) {
		t.Fatalf("after the crash loop: blocked %v, moving %v", blocked(d), moving(d))
	}
	if got := e.surgesOn(nodeB); !eqStrings(got, []string{"api/0@1 removed", "web/0@1 running"}) {
		t.Fatalf("surges after the crash loop: %v", got)
	}
	e.report(nodeB) // node-b's agent removed the failed copy
	e.tick(30 * time.Second)
	e.step()
	if got := e.surgesOn(nodeB); !eqStrings(got, []string{"web/0@1 running"}) {
		t.Fatalf("api was tried again: %v", got)
	}

	e.tick(60 * time.Second)
	done := e.step()
	if len(done) != 1 || done[0].Moved != 1 || done[0].Stopped != 1 {
		t.Fatalf("at the deadline: %+v", done)
	}
	if w, a := e.nodesOf("web"), e.nodesOf("api"); !eqStrings(w, []string{nodeB}) || !eqStrings(a, []string{nodeB}) {
		t.Fatalf("after the deadline: web %v, api %v", w, a)
	}
	if got := e.surgesOn(nodeB); len(got) != 0 {
		t.Fatalf("surges after the deadline: %v", got)
	}
}

// The server restarts in the middle of a drain. The drain carries on, and a
// copy that was starting while the server was down is given a whole rollout
// deadline from the restart.
func TestDrainSurvivesARestart(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB)
	e.put(web(1))
	e.put(app("api", 1))
	before := e.drain(nodeA, 10*time.Minute)

	e.reopen()
	e.tick(5 * time.Minute)
	restarted := e.clock.t
	e.stepSince(restarted)
	d := e.getDrain(nodeA)
	if d.State != DrainDraining || !d.Deadline.Equal(before.Deadline) || !eqStrings(moving(d), []string{"api/0>" + nodeB, "web/0>" + nodeB}) || len(d.Blocked) != 0 {
		t.Fatalf("after the restart: %+v", d)
	}

	e.report(nodeB, container("web", 0, 1))
	e.stepSince(restarted)
	if got := e.nodesOf("web"); !eqStrings(got, []string{nodeB}) {
		t.Fatalf("web after the restart: %v", got)
	}
	// api's copy never reports. Its deadline counts from the restart.
	e.tick(DefaultRolloutDeadline - time.Second)
	e.stepSince(restarted)
	if got := moving(e.getDrain(nodeA)); !eqStrings(got, []string{"api/0>" + nodeB}) {
		t.Fatalf("api's move failed early: %v", got)
	}
	e.tick(time.Second)
	e.stepSince(restarted)
	if got := blocked(e.getDrain(nodeA)); !eqStrings(got, []string{"api/0: its copy on node node-b was not ready after 2m0s; it stays until the deadline"}) {
		t.Fatalf("api after its deadline: %v", got)
	}
}

// A deadline that passed while the server was down ends the drain at its
// first step after the restart.
func TestDrainDeadlinePassedWhileTheServerWasDown(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB)
	e.put(web(1))
	e.drain(nodeA, time.Minute)
	e.reopen()
	e.tick(5 * time.Minute)
	done := e.stepSince(e.clock.t)
	if len(done) != 1 || done[0].Moved != 1 || done[0].Stopped != 0 {
		t.Fatalf("first step after the restart: %+v", done)
	}
	if got := e.nodesOf("web"); !eqStrings(got, []string{nodeB}) {
		t.Fatalf("web: %v", got)
	}
}

func TestEligibilityCancelsADrain(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB)
	e.put(web(1))
	e.drain(nodeA, 0)
	el := e.setEligible(nodeA, true)
	if !el.Eligible || !el.DrainCancelled {
		t.Fatalf("eligibility: %+v", el)
	}
	if _, err := e.s.GetDrain(e.ctx, nodeA); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "has no drain") {
		t.Fatalf("drain after it was cancelled: %v", err)
	}
	if got := e.surgesOn(nodeB); !eqStrings(got, []string{"web/0@1 removed"}) {
		t.Fatalf("the cancelled move's surge: %v", got)
	}
	if got := e.nodesOf("web"); !eqStrings(got, []string{nodeA}) {
		t.Fatalf("web: %v", got)
	}
	if done := e.step(); len(done) != 0 {
		t.Fatalf("a cancelled drain finished: %+v", done)
	}

	// A finished drain's record goes too, and nothing is cancelled.
	e.report(nodeB)
	e.drain(nodeA, 0)
	e.report(nodeB, container("web", 0, 1))
	if done := e.step(); len(done) != 1 {
		t.Fatalf("second drain did not finish: %+v", done)
	}
	if el := e.setEligible(nodeA, true); el.DrainCancelled {
		t.Fatalf("enabling a drained node: %+v", el)
	}
	if _, err := e.s.GetDrain(e.ctx, nodeA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drain record after enabling: %v", err)
	}
}

// A drain leaves an app with a rollout in progress to the rollout, which no
// longer surges to the drained node, and finishes when the rollout has moved
// the replicas off.
func TestDrainWaitsForARollout(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB)
	e.put(web(2))
	e.put(withEnv(2, "2"))
	d := e.drain(nodeA, 0)
	reason := "its app has a rollout in progress; the drain moves it after the rollout, unless the rollout moves it first"
	if !eqStrings(blocked(d), []string{"web/0: " + reason, "web/1: " + reason}) || len(d.Moving) != 0 {
		t.Fatalf("first step: %+v", d)
	}
	e.advance()
	if got := e.surgesOn(nodeB); !eqStrings(got, []string{"web/0@2 running"}) {
		t.Fatalf("rollout surge: %v", got)
	}
	e.report(nodeB, container("web", 0, 2))
	e.advance()
	e.report(nodeB, container("web", 0, 2), container("web", 1, 2))
	e.advance()
	if ro := e.rollout("web"); ro.State != RolloutDone {
		t.Fatalf("rollout: %+v", ro)
	}
	e.report(nodeA)
	done := e.step()
	if len(done) != 1 || done[0].Moved != 0 || done[0].Remaining != 0 {
		t.Fatalf("drain after the rollout: %+v", done)
	}
	if got := e.nodesOf("web"); !eqStrings(got, []string{nodeB, nodeB}) {
		t.Fatalf("web: %v", got)
	}
}

// A deploy during a drain's move abandons the move, as it abandons a
// rollout's move, and the rollout takes over.
func TestDeployDuringADrainMove(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB)
	e.put(web(1))
	e.drain(nodeA, 0)
	e.put(withEnv(1, "2"))
	e.step()
	if d := e.getDrain(nodeA); len(d.Moving) != 0 || len(d.Blocked) != 1 {
		t.Fatalf("drain after a deploy: %+v", d)
	}
	e.advance()
	if got := e.surgesOn(nodeB); !eqStrings(got, []string{"web/0@2 running"}) {
		t.Fatalf("surges after the deploy: %v", got)
	}
}

// Removing a node that is draining ends each move in flight on the node
// where its copy is starting.
func TestRemoveADrainingNode(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB, nodeC)
	e.put(web(2))
	e.drain(nodeA, 0)
	r, err := e.s.RemoveNode(e.ctx, nodeA, true)
	if err != nil || r.Rescheduled != 2 {
		t.Fatalf("remove: %+v, %v", r, err)
	}
	if got := e.nodesOf("web"); !eqStrings(got, []string{nodeB, nodeB}) {
		t.Fatalf("web after the removal: %v", got)
	}
	if got := e.surgesOn(nodeB); len(got) != 0 {
		t.Fatalf("surges after the removal: %v", got)
	}
	if _, err := e.s.GetDrain(e.ctx, nodeA); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "was removed") {
		t.Fatalf("drain of the removed node: %v", err)
	}
	checkInvariants(t, e.s)
}

func TestDrainNodeArguments(t *testing.T) {
	e := newDrainEnv(t, FirstNode, nodeA, nodeB)
	if _, err := e.s.DrainNode(e.ctx, "node-x", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
	for _, d := range []time.Duration{-time.Second, MaxDrainDeadline + time.Second} {
		if _, err := e.s.DrainNode(e.ctx, nodeA, d); !errors.Is(err, ErrInvalid) {
			t.Fatalf("deadline %s: %v", d, err)
		}
	}
	if n, _ := e.s.GetNode(e.ctx, nodeA); !n.Eligible {
		t.Fatal("a refused drain made the node ineligible")
	}

	// A drain of a draining node keeps its deadline with zero, and takes a
	// new one otherwise. It is the same drain either way.
	e.put(web(1))
	first := e.drain(nodeA, time.Minute)
	e.tick(10 * time.Second)
	if d := e.drain(nodeA, 0); !d.Deadline.Equal(first.Deadline) || !d.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("drain again without a deadline: %+v", d)
	}
	if d := e.drain(nodeA, 5*time.Minute); !d.Deadline.Equal(e.clock.t.Add(5*time.Minute)) || !d.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("drain again with a deadline: %+v", d)
	}
}

// A node lost during its drain gives up its replicas as on any node loss.
// The drain counts them as lost, not as moved or stopped, records when the
// node was lost, and counts each replica once, however many sweeps find the
// node lost. At the deadline nothing is left to stop.
func TestDrainCountsTheReplicasANodeLossTook(t *testing.T) {
	e := newDrainEnv(t, nil, nodeA, nodeB)
	webApp := spec.App{Name: "web", Image: "nginx:alpine", Replicas: 2, Ports: []spec.Port{{Host: 8080, Container: 80}}}
	mustPut(t, e.s, webApp)
	mustPut(t, e.s, app("calm", 2))
	on := func(app string) map[string]int {
		out := map[string]int{}
		for _, r := range replicasOf(t, e.s, app) {
			out[r.NodeID] = r.Index
		}
		return out
	}
	web, calm := on("web"), on("calm")
	if len(web) != 2 || len(calm) != 2 {
		t.Fatalf("web on %v, calm on %v; want one replica of each on each node", web, calm)
	}
	e.report(nodeA, container("web", web[nodeA], 1), container("calm", calm[nodeA], 1))
	e.report(nodeB, container("web", web[nodeB], 1), container("calm", calm[nodeB], 1))

	// calm's replica on node-b starts a copy on node-a; web's cannot move,
	// as its port is taken there.
	d := e.drain(nodeB, 2*time.Minute)
	if len(d.Moving) != 1 || len(d.Blocked) != 1 {
		t.Fatalf("first step: moving %v, blocked %v", moving(d), blocked(d))
	}

	// node-b stops renewing its lease. The sweep finds it lost 16 s later.
	e.clock.Advance(16 * time.Second)
	e.renew(nodeA)
	lostAt := e.clock.Now()
	if lost, err := e.s.ReconcileNodes(e.ctx); err != nil || len(lost) != 1 || lost[0] != nodeB {
		t.Fatalf("sweep: lost %v, %v; want node-b", lost, err)
	}
	e.step()
	for range 3 {
		e.clock.Advance(5 * time.Second)
		e.renew(nodeA)
		if _, err := e.s.ReconcileNodes(e.ctx); err != nil {
			t.Fatal(err)
		}
		e.step()
	}
	d = e.getDrain(nodeB)
	if d.State != DrainDraining || d.Lost != 2 || !d.LostAt.Equal(lostAt) || d.Moved != 0 || d.Stopped != 0 || d.Remaining != 0 {
		t.Fatalf("drain after the loss: %+v; want 2 lost at %s, none moved or stopped, none left", d, lostAt)
	}

	// The node never comes back. At the deadline the drain ends with
	// nothing to stop, and its result still says what the loss took.
	e.clock.Advance(2 * time.Minute)
	e.renew(nodeA)
	if done := e.step(); len(done) != 1 {
		t.Fatalf("the drain did not finish at its deadline: %+v", e.getDrain(nodeB))
	}
	d = e.getDrain(nodeB)
	if d.State != DrainDone || d.Lost != 2 || !d.LostAt.Equal(lostAt) || d.Moved != 0 || d.Stopped != 0 {
		t.Fatalf("finished drain: %+v; want 2 lost, none moved or stopped", d)
	}

	// A drain whose node is not lost records no loss.
	e2 := newDrainEnv(t, nil, nodeA, nodeB)
	mustPut(t, e2.s, app("calm", 2))
	e2.drain(nodeB, time.Minute)
	e2.tick(2 * time.Minute)
	e2.step()
	if d := e2.getDrain(nodeB); d.Lost != 0 || !d.LostAt.IsZero() {
		t.Fatalf("drain of a node that stayed: %+v; want no loss", d)
	}
}

// ReplicasToMove counts the replicas without a volume on a node, which a
// removed server's node would run twice: not a replica with a volume, and
// none once the node is lost and its replicas are placed again.
func TestReplicasToMove(t *testing.T) {
	e := newDrainEnv(t, nil, nodeA, nodeB)
	mustPut(t, e.s, app("calm", 3))
	mustPut(t, e.s, dbApp())
	count := func(node string) int {
		t.Helper()
		n, err := e.s.ReplicasToMove(e.ctx, node)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	calm := map[string]int{}
	for _, r := range replicasOf(t, e.s, "calm") {
		calm[r.NodeID]++
	}
	if a, b := count(nodeA), count(nodeB); a != calm[nodeA] || b != calm[nodeB] || a+b != 3 {
		t.Fatalf("to move: %d on node-a, %d on node-b; want calm's, %v, and not db's", a, b, calm)
	}
	e.clock.Advance(16 * time.Second)
	e.renew(nodeB)
	if lost, err := e.s.ReconcileNodes(e.ctx); err != nil || len(lost) != 1 || lost[0] != nodeA {
		t.Fatalf("sweep: lost %v, %v; want node-a", lost, err)
	}
	if a, b := count(nodeA), count(nodeB); a != 0 || b != 3 {
		t.Fatalf("to move once node-a is lost: %d on node-a, %d on node-b; want 0 and 3", a, b)
	}
}
