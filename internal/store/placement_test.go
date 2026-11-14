package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// schedEnv is a store with the given MinReady, a fake clock and the given
// nodes registered. It uses the default placement, the scheduler.
type schedEnv struct {
	t     *testing.T
	s     *Store
	clock *fakeClock
}

func newSchedEnv(t *testing.T, minReady time.Duration, nodes ...string) *schedEnv {
	t.Helper()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), WithClock(clock.Now), WithMinReady(minReady))
	e := &schedEnv{t: t, s: s, clock: clock}
	for _, id := range nodes {
		e.renew(id)
	}
	return e
}

// renew registers a node, or renews its lease at the fake clock's time.
func (e *schedEnv) renew(id string) {
	e.t.Helper()
	if err := e.s.UpsertNode(ctxT(e.t), Node{ID: id, Name: id, CPU: 4, Memory: 8 << 30}); err != nil {
		e.t.Fatal(err)
	}
}

// sweep runs the server's node sweep and returns the nodes it found lost.
func (e *schedEnv) sweep() []string {
	e.t.Helper()
	lost, err := e.s.ReconcileNodes(ctxT(e.t))
	if err != nil {
		e.t.Fatal(err)
	}
	return lost
}

// nodeOf returns the node of one replica, or "" when it is pending.
func (e *schedEnv) nodeOf(app string, idx int) string {
	e.t.Helper()
	for _, r := range replicasOf(e.t, e.s, app) {
		if r.Index == idx {
			return r.NodeID
		}
	}
	e.t.Fatalf("no replica %s/%d", app, idx)
	return ""
}

// Replicas of one app go to different nodes when the nodes allow it.
func TestPlacementSpreadsReplicas(t *testing.T) {
	e := newSchedEnv(t, 0, "node-1", "node-2", "node-3")
	mustPut(t, e.s, web(3))
	seen := map[string]bool{}
	for _, r := range replicasOf(t, e.s, "web") {
		if r.NodeID == "" {
			t.Fatalf("replica %d is pending: %s", r.Index, r.PendingReason)
		}
		seen[r.NodeID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("3 replicas on %d nodes, want 3", len(seen))
	}
}

// A node takes new work only after MinReady of readiness. A node that keeps
// renewing its lease stays ready; the replica is placed when the wait is over.
func TestMinReadyWaitsBeforeNewWork(t *testing.T) {
	e := newSchedEnv(t, 30*time.Second, "node-1")
	mustPut(t, e.s, web(1))
	if r := replicasOf(t, e.s, "web")[0]; r.NodeID != "" || !strings.Contains(r.PendingReason, "ready for 30s") {
		t.Fatalf("placed on a node that just registered: %+v", r)
	}
	for _, want := range []bool{false, false, true} {
		e.clock.Advance(10 * time.Second)
		e.renew("node-1")
		e.sweep()
		got := replicasOf(t, e.s, "web")[0].NodeID != ""
		if got != want {
			t.Fatalf("at %s after registering: placed=%v, want %v", e.clock.Now().Sub(time.Unix(1_700_000_000, 0)), got, want)
		}
	}
}

// A node whose lease lapsed starts its readiness over. A node that goes
// quiet for longer than the lease timeout can't take new work straight away.
func TestLapsedLeaseRestartsReadiness(t *testing.T) {
	e := newSchedEnv(t, 30*time.Second, "node-1")
	e.clock.Advance(31 * time.Second)
	e.renew("node-1") // the lease lapsed at 15 s, so readiness starts now
	n, err := e.s.GetNode(ctxT(t), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !n.ReadySince.Equal(e.clock.Now()) {
		t.Fatalf("ready since %s after a lapse, want %s", n.ReadySince, e.clock.Now())
	}
}

// The cluster's own node is ready from the start, so a single-node cluster
// does not wait for MinReady.
func TestLocalNodeNeedsNoWait(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), WithClock(clock.Now))
	if err := s.InitCluster(ctxT(t), Cluster{AdminTokenHash: [32]byte{1}, NodeID: "node-local"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertNode(ctxT(t), Node{ID: "node-local", Name: "local", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, web(1))
	if r := replicasOf(t, s, "web")[0]; r.NodeID != "node-local" {
		t.Fatalf("local node did not take a replica at once: %+v", r)
	}
}

// Two apps that publish the same host port can't share a node. With one node
// the second app's replica stays pending with the scheduler's reason; when a
// second node joins, it goes there.
func TestHostPortConflictAcrossApps(t *testing.T) {
	e := newSchedEnv(t, 0, "node-1")
	ported := func(name string) spec.App {
		return spec.App{Name: name, Image: "nginx:alpine", Replicas: 1, Ports: []spec.Port{{Host: 8080, Container: 80}}}
	}
	mustPut(t, e.s, ported("a"))
	if got := e.nodeOf("a", 0); got != "node-1" {
		t.Fatalf("app a on %q, want node-1", got)
	}
	mustPut(t, e.s, ported("b"))
	if r := replicasOf(t, e.s, "b")[0]; r.NodeID != "" || !strings.Contains(r.PendingReason, "in use") {
		t.Fatalf("app b shares host port 8080 with a on node-1: %+v", r)
	}

	e.renew("node-2")
	e.sweep()
	if got := e.nodeOf("b", 0); got != "node-2" {
		t.Fatalf("app b on %q after node-2 joined, want node-2", got)
	}
	checkInvariants(t, e.s)
}

// A replica marked for removal still holds its host port until its node
// reports the container gone, so another app can't take the port meanwhile.
func TestRemovedReplicaStillHoldsItsPort(t *testing.T) {
	e := newSchedEnv(t, 0, "node-1")
	mustPut(t, e.s, spec.App{Name: "a", Image: "nginx:alpine", Replicas: 1, Ports: []spec.Port{{Host: 9090, Container: 80}}})
	if err := e.s.DeleteApp(ctxT(t), "a"); err != nil {
		t.Fatal(err)
	}
	mustPut(t, e.s, spec.App{Name: "b", Image: "nginx:alpine", Replicas: 1, Ports: []spec.Port{{Host: 9090, Container: 80}}})
	if r := replicasOf(t, e.s, "b")[0]; r.NodeID != "" {
		t.Fatalf("app b took port 9090 while a's container could still run: %+v", r)
	}

	// The node reports a's container gone: the port is free, and b is placed
	// in the same report, without waiting for a sweep.
	if err := e.s.ReportStatus(ctxT(t), "node-1", nil); err != nil {
		t.Fatal(err)
	}
	if got := e.nodeOf("b", 0); got != "node-1" {
		t.Fatalf("app b on %q after a's container was reported gone, want node-1", got)
	}
	checkInvariants(t, e.s)
}

// When a node's lease lapses, its replicas are placed on other nodes, and the
// lost node is given no assignments, so it can't restart them when it returns.
func TestNodeLossMovesReplicas(t *testing.T) {
	e := newSchedEnv(t, 0, "node-1", "node-2")
	mustPut(t, e.s, web(2))
	if e.nodeOf("web", 0) == e.nodeOf("web", 1) {
		t.Fatal("replicas not spread across the two nodes")
	}

	e.clock.Advance(20 * time.Second)
	e.renew("node-1") // node-2 stops renewing
	lost := e.sweep()
	if len(lost) != 1 || lost[0] != "node-2" {
		t.Fatalf("lost nodes %v, want [node-2]", lost)
	}
	for _, r := range replicasOf(t, e.s, "web") {
		if r.NodeID != "node-1" {
			t.Fatalf("replica %d on %q after node-2 was lost, want node-1", r.Index, r.NodeID)
		}
	}
	checkInvariants(t, e.s)
	if as, err := e.s.AssignmentsForNode(ctxT(t), "node-2"); err != nil || len(as) != 0 {
		t.Fatalf("lost node still has %d assignments (err %v)", len(as), err)
	}
}

// A lost node that comes back is given only the replicas it still holds, and
// its readiness starts again, so it takes no new work for MinReady.
func TestReturningNodeGetsNoMovedReplicas(t *testing.T) {
	e := newSchedEnv(t, 30*time.Second, "node-1", "node-2")
	for range 3 { // renew every 10 s, so both nodes are ready at 30 s
		e.clock.Advance(10 * time.Second)
		e.renew("node-1")
		e.renew("node-2")
	}
	mustPut(t, e.s, web(2))
	// The node that holds replica 1 is the one that goes quiet.
	lostNode := e.nodeOf("web", 1)
	other := "node-1"
	if lostNode == "node-1" {
		other = "node-2"
	}

	e.clock.Advance(10 * time.Second)
	e.renew(other)
	e.clock.Advance(10 * time.Second)
	e.renew(other)
	if lost := e.sweep(); len(lost) != 1 || lost[0] != lostNode {
		t.Fatalf("lost %v, want [%s]", lost, lostNode)
	}
	if got := e.nodeOf("web", 1); got != other {
		t.Fatalf("replica 1 on %q after its node was lost, want %s", got, other)
	}

	e.clock.Advance(5 * time.Second)
	e.renew(lostNode)
	e.sweep()
	as, err := e.s.AssignmentsForNode(ctxT(t), lostNode)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range as {
		if a.App == "web" && a.Index == 1 {
			t.Fatalf("returning node %s is still assigned replica 1, which moved to %s", lostNode, other)
		}
	}
	n, err := e.s.GetNode(ctxT(t), lostNode)
	if err != nil {
		t.Fatal(err)
	}
	if !n.ReadySince.Equal(e.clock.Now()) {
		t.Fatalf("returned node ready since %s, want the return time %s", n.ReadySince, e.clock.Now())
	}
}

// A volume replica is pinned to its node. When that node is lost it is not
// moved, even to a healthy node, and it is reported unavailable. When the node
// returns, the replica is still its, and the report is cleared.
func TestVolumeReplicaIsPinnedAndReportedUnavailable(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	pinTo := func(nodes []Node, placed []Assignment, app spec.App, idx int, now time.Time) (string, error) {
		if len(app.Volumes) > 0 {
			return "node-2", nil
		}
		return FirstNode(nodes, placed, app, idx, now)
	}
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), WithClock(clock.Now), WithPlacement(pinTo), WithMinReady(0))
	e := &schedEnv{t: t, s: s, clock: clock}
	e.renew("node-1")
	e.renew("node-2")
	db := spec.App{Name: "db", Image: "postgres:16", Replicas: 1, Volumes: []spec.Volume{{Name: "data", Path: "/var/lib/data"}}}
	mustPut(t, s, db)
	if got := e.nodeOf("db", 0); got != "node-2" {
		t.Fatalf("db on %q, want node-2", got)
	}

	clock.Advance(20 * time.Second)
	e.renew("node-1")
	if lost := e.sweep(); len(lost) != 1 || lost[0] != "node-2" {
		t.Fatalf("lost %v, want [node-2]", lost)
	}
	if r := replicasOf(t, s, "db")[0]; r.NodeID != "node-2" || !strings.Contains(r.PendingReason, "unavailable") {
		t.Fatalf("pinned replica after node loss: %+v", r)
	}
	checkInvariants(t, s)

	clock.Advance(5 * time.Second)
	e.renew("node-2")
	e.sweep()
	if r := replicasOf(t, s, "db")[0]; r.NodeID != "node-2" || r.PendingReason != "" {
		t.Fatalf("pinned replica after the node returned: %+v", r)
	}
	as, err := s.AssignmentsForNode(ctxT(t), "node-2")
	if err != nil || len(as) != 1 || as[0].App != "db" {
		t.Fatalf("returned node's assignments %v (err %v), want db", as, err)
	}
}

// A lost node's surges are dropped, so a rollout that was moving a replica to
// it can't wait on a copy that will never run there.
func TestNodeLossDropsSurges(t *testing.T) {
	e := newSchedEnv(t, 0, "node-1", "node-2")
	mustPut(t, e.s, web(1))
	if _, err := e.s.db.Exec(`INSERT INTO surges (app, idx, node_id, generation, desired, updated_at)
		VALUES ('web', 0, 'node-2', 2, 'running', 0)`); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(20 * time.Second)
	e.renew("node-1")
	e.sweep()
	var n int
	if err := e.s.db.QueryRow(`SELECT count(*) FROM surges WHERE node_id = 'node-2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d surges left on the lost node", n)
	}
}

// The default placement reads each node's lease: a node whose lease is stale
// is not picked, however free it is.
func TestSchedulerPlaceSeesLeaseAges(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), WithClock(clock.Now), WithMinReady(0))
	nodes := []Node{
		{ID: "node-old", CPU: 4, Memory: 1 << 30, LastLease: clock.Now().Add(-time.Minute)},
		{ID: "node-new", CPU: 4, Memory: 1 << 30, LastLease: clock.Now(), ReadySince: clock.Now()},
	}
	id, err := s.schedulerPlace(nodes, nil, spec.App{Name: "web", Image: "x", Replicas: 1}, 0, clock.Now())
	if err != nil || id != "node-new" {
		t.Fatalf("placed on %q (err %v), want node-new", id, err)
	}
}

// A rollout never surges a replica with a volume: a copy on another node would
// start with an empty volume. The replica moves in place on its own node.
func TestVolumeReplicaIsNeverSurged(t *testing.T) {
	e := newSchedEnv(t, 0, "node-1", "node-2")
	db := spec.App{Name: "db", Image: "postgres:15", Replicas: 1, Volumes: []spec.Volume{{Name: "data", Path: "/var/lib/data"}}}
	mustPut(t, e.s, db)
	before := e.nodeOf("db", 0)

	db.Image = "postgres:16"
	mustPut(t, e.s, db)
	if err := e.s.AdvanceRollouts(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	var surges int
	if err := e.s.db.QueryRow(`SELECT count(*) FROM surges`).Scan(&surges); err != nil {
		t.Fatal(err)
	}
	if surges != 0 {
		t.Fatalf("%d surges for a replica with a volume", surges)
	}
	if got := e.nodeOf("db", 0); got != before {
		t.Fatalf("volume replica moved from %s to %s during a rollout", before, got)
	}
}
