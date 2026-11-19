package store

import (
	"path/filepath"
	"slices"
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

// RenewLease keeps a node from being lost without a status report, and leaves
// what it reported as it was.
func TestRenewLeaseKeepsANodeWithoutAReport(t *testing.T) {
	e := newSchedEnv(t, 0, "node-1")
	mustPut(t, e.s, web(1))
	st := []Status{{Name: "c1", App: "web", Replica: 0, Generation: 1, ContainerID: "abc", State: "running", Ready: true}}
	if err := e.s.ReportStatus(ctxT(t), "node-1", st); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		e.clock.Advance(10 * time.Second)
		if err := e.s.RenewLease(ctxT(t), "node-1"); err != nil {
			t.Fatal(err)
		}
		if lost := e.sweep(); len(lost) != 0 {
			t.Fatalf("lost %v at %s with a lease renewed every 10 s", lost, e.clock.Now())
		}
	}
	got, err := e.s.StatusForNode(ctxT(t), "node-1")
	if err != nil || len(got) != 1 || got[0].ContainerID != "abc" {
		t.Fatalf("status after the renewals: %+v, %v; want the report as it was", got, err)
	}
	e.clock.Advance(16 * time.Second)
	if lost := e.sweep(); len(lost) != 1 {
		t.Fatalf("lost %v 16 s after the last renewal, want node-1", lost)
	}
}

// A server that has just started, after an outage or a restore, finds every
// lease as old as the outage. A node that was up when the server went down
// waits for its first report for the start grace: it is not lost, and its
// replicas stay on it, though its lease is far older than the lease
// timeout. A node that reports in the grace is live at once, and from then
// on is lost as any node is, 15 s after its last renewal. A node that stays
// quiet is lost when the grace ends, and only then are its replicas placed
// again. A node whose lease had lapsed before the server went down is lost
// at once. In the third soak, a server restored at a new address, whose
// workers reached it 31 s after it started, lost all three at 15 s and ran
// their replicas a second time.
func TestStartGraceKeepsTheNodesThatWereUp(t *testing.T) {
	e := newSchedEnv(t, 0, "node-1", "node-2", "node-3")
	mustPut(t, e.s, web(3))
	if got := e.nodesOfWeb(); len(got) != 3 || got[0] == got[1] || got[1] == got[2] || got[0] == got[2] {
		t.Fatalf("web on %v, want one replica on each node", got)
	}
	// node-3 stops renewing, and the server finds it lost before it goes
	// down; its replica moves to node-1 or node-2.
	e.clock.Advance(10 * time.Second)
	e.renew("node-1")
	e.renew("node-2")
	e.clock.Advance(10 * time.Second)
	e.renew("node-1")
	e.renew("node-2")
	if lost := e.sweep(); !slices.Equal(lost, []string{"node-3"}) {
		t.Fatalf("before the outage: lost %v, want [node-3]", lost)
	}
	before := e.nodesOfWeb()
	if slices.Contains(before, "node-3") || slices.Contains(before, "") {
		t.Fatalf("web on %v after node-3 was lost", before)
	}

	// The server is down for ten minutes, then starts with a 2-minute grace.
	e.clock.Advance(10 * time.Minute)
	started := e.clock.Now()
	if err := e.s.SetStarted(ctxT(t), started, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration) { e.clock.t = started.Add(d) }
	waiting := func() []string {
		t.Helper()
		nodes, err := e.s.ListNodes(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range nodes {
			if e.s.Waiting(n, e.clock.Now()) {
				out = append(out, n.ID)
			}
		}
		return out
	}
	for _, d := range []time.Duration{0, 5 * time.Second, 16 * time.Second, 30 * time.Second} {
		at(d)
		if lost := e.sweep(); !slices.Equal(lost, []string{"node-3"}) {
			t.Fatalf("%s after the start: lost %v, want only node-3, lost before the start", d, lost)
		}
		if w := waiting(); !slices.Equal(w, []string{"node-1", "node-2"}) {
			t.Fatalf("%s after the start: waiting %v, want node-1 and node-2", d, w)
		}
		if got := e.nodesOfWeb(); !slices.Equal(got, before) {
			t.Fatalf("%s after the start: web on %v, want it where it was, %v", d, got, before)
		}
	}

	// node-1 reports 31 s after the start: it is live, not waiting.
	at(31 * time.Second)
	e.renew("node-1")
	if w := waiting(); !slices.Equal(w, []string{"node-2"}) {
		t.Fatalf("after node-1's report: waiting %v, want node-2", w)
	}
	at(45 * time.Second)
	if lost := e.sweep(); !slices.Equal(lost, []string{"node-3"}) {
		t.Fatalf("14 s after node-1's report: lost %v, want only node-3", lost)
	}
	// It then stops: 15 s later it is lost, in the grace, as any node is.
	at(47 * time.Second)
	if lost := e.sweep(); !slices.Equal(lost, []string{"node-1", "node-3"}) {
		t.Fatalf("16 s after node-1's report: lost %v, want node-1 and node-3", lost)
	}

	// node-2 never reports. It waits to the end of the grace, then is lost.
	at(2*time.Minute - time.Second)
	if lost := e.sweep(); !slices.Equal(lost, []string{"node-1", "node-3"}) {
		t.Fatalf("at the end of the grace: lost %v, want node-1 and node-3", lost)
	}
	for i, n := range before {
		if n == "node-2" && e.nodeOf("web", i) != "node-2" {
			t.Fatalf("web %d left node-2 in the grace", i)
		}
	}
	at(2 * time.Minute)
	if lost := e.sweep(); !slices.Equal(lost, []string{"node-1", "node-2", "node-3"}) {
		t.Fatalf("when the grace ended: lost %v, want all three", lost)
	}
	if w := waiting(); len(w) != 0 {
		t.Fatalf("after the grace: waiting %v", w)
	}
	for i, n := range e.nodesOfWeb() {
		if n == "node-2" {
			t.Fatalf("web %d is still on node-2 after the grace", i)
		}
	}
}

// The start grace takes when the server went down from the workers' leases:
// the server's own node renews its lease as serve starts, before or after
// SetStarted, and must not move that time. serve renewed it first in the
// third soak's build, and the routing table then dropped every worker.
func TestStartGraceLeavesOutTheServersOwnLease(t *testing.T) {
	for _, upsertFirst := range []bool{true, false} {
		e := newSchedEnv(t, 0, "node-s", "node-1")
		if err := e.s.InitCluster(ctxT(t), Cluster{AdminTokenHash: [32]byte{1}, NodeID: "node-s"}); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(2 * time.Minute)
		if upsertFirst {
			// As serve did: a moment before it took the start time.
			e.renew("node-s")
			e.clock.Advance(time.Millisecond)
		}
		started := e.clock.Now()
		if err := e.s.SetStarted(ctxT(t), started, 2*time.Minute); err != nil {
			t.Fatal(err)
		}
		if !upsertFirst {
			e.renew("node-s")
		}
		for range 2 {
			e.clock.Advance(10 * time.Second)
			e.renew("node-s")
		}
		if lost := e.sweep(); len(lost) != 0 {
			t.Fatalf("server's lease renewed first %v: lost %v 20 s after the start, want none", upsertFirst, lost)
		}
	}
}

// An old server back on its own, after its cluster was restored elsewhere,
// hears from none of its workers: they refuse it. In the fourth soak it
// marked all three lost when its grace ended, and ran every replica on its
// own machine. When the grace ends with no report from any worker that was
// waiting, they go on waiting: none is lost, and their replicas stay where
// they were, for as long as that lasts. A node removed meanwhile has its
// replicas placed again, as a removal always does. Once one of them
// reports, the others wait for the start grace again, and are lost after it.
func TestStartGraceHoldsWhenNoWorkerReports(t *testing.T) {
	e := newSchedEnv(t, 0, "node-s", "node-1", "node-2", "node-3")
	if err := e.s.InitCluster(ctxT(t), Cluster{AdminTokenHash: [32]byte{1}, NodeID: "node-s"}); err != nil {
		t.Fatal(err)
	}
	mustPut(t, e.s, web(4))
	before := e.nodesOfWeb()
	if slices.Contains(before, "") || len(before) != 4 {
		t.Fatalf("web on %v, want every replica placed", before)
	}
	e.clock.Advance(10 * time.Minute)
	started := e.clock.Now()
	if err := e.s.SetStarted(ctxT(t), started, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.s.StartWait(started); !ok {
		t.Fatal("StartWait: no workers waiting at the start")
	}
	waiting := func() []string {
		t.Helper()
		nodes, err := e.s.ListNodes(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range nodes {
			if e.s.Waiting(n, e.clock.Now()) {
				out = append(out, n.ID)
			}
		}
		return out
	}
	held := func() bool {
		w, ok := e.s.StartWait(e.clock.Now())
		if !ok || !w.GraceEnds.Equal(started.Add(2*time.Minute)) {
			t.Fatalf("StartWait %+v, %v", w, ok)
		}
		return w.Held
	}
	// The server's own node renews, as serve does; the workers never do.
	at := func(d time.Duration) {
		e.clock.t = started.Add(d)
		e.renew("node-s")
	}
	for _, d := range []time.Duration{30 * time.Second, 2*time.Minute - time.Second, 2 * time.Minute, 3 * time.Minute, 2 * time.Hour} {
		at(d)
		if lost := e.sweep(); len(lost) != 0 {
			t.Fatalf("%s after the start, no worker having reported: lost %v, want none", d, lost)
		}
		if w := waiting(); !slices.Equal(w, []string{"node-1", "node-2", "node-3"}) {
			t.Fatalf("%s after the start: waiting %v, want the three workers", d, w)
		}
		if got := e.nodesOfWeb(); !slices.Equal(got, before) {
			t.Fatalf("%s after the start: web on %v, want it where it was, %v", d, got, before)
		}
		if want := d >= 2*time.Minute; held() != want {
			t.Fatalf("%s after the start: held %v, want %v", d, !want, want)
		}
	}

	// The operator removes node-3, which is really gone: its replica is
	// placed again, on a node that is up.
	at(2*time.Hour + time.Second)
	if _, err := e.s.RemoveNode(ctxT(t), "node-3", false); err != nil {
		t.Fatal(err)
	}
	e.sweep()
	for i, n := range before {
		if n == "node-3" && e.nodeOf("web", i) != "node-s" {
			t.Fatalf("web %d is on %q after node-3 was removed, want node-s", i, e.nodeOf("web", i))
		}
	}
	if w := waiting(); !slices.Equal(w, []string{"node-1", "node-2"}) || !held() {
		t.Fatalf("after the removal: waiting %v, held %v", w, held())
	}

	// node-1 reports: the hold is over, and node-2, which has not, waits
	// for the start grace again (TestStartHoldEndGivesTheOthersTheGrace),
	// then is lost.
	at(2*time.Hour + time.Minute)
	e.renew("node-1")
	if lost := e.sweep(); len(lost) != 0 {
		t.Fatalf("after node-1's report: lost %v, want none", lost)
	}
	if w := waiting(); !slices.Equal(w, []string{"node-2"}) || held() {
		t.Fatalf("after node-1's report: waiting %v, held %v", w, held())
	}
	at(2*time.Hour + 3*time.Minute)
	e.renew("node-1")
	if lost := e.sweep(); !slices.Equal(lost, []string{"node-2"}) {
		t.Fatalf("a grace after node-1's report: lost %v, want node-2", lost)
	}
	if w := waiting(); len(w) != 0 || held() {
		t.Fatalf("a grace after node-1's report: waiting %v, held %v", w, held())
	}
	for i, n := range e.nodesOfWeb() {
		if n == "node-2" || n == "node-3" {
			t.Fatalf("web %d is still on %s", i, n)
		}
	}
}

// When the hold ends, at the first report of a worker that was waiting,
// the others wait for the start grace again, from that report: a partition
// that heals heals for each worker at its own moment. In the fifth soak the
// hold ended for all of them at once: a worker 0.52 s behind the first was
// lost, and one 12 s behind had its six replicas placed again on the
// server's machine, where they ran twice for 20 s and stayed. Here workers
// report at the hold's end, 0.5 s and 12 s after it, with a sweep before
// and after each report: none is lost, and no replica moves. One that never
// reports is lost a grace after the hold's end, and only then is its
// replica placed again.
func TestStartHoldEndGivesTheOthersTheGrace(t *testing.T) {
	e := newSchedEnv(t, 0, "node-s", "node-1", "node-2", "node-3", "node-4")
	if err := e.s.InitCluster(ctxT(t), Cluster{AdminTokenHash: [32]byte{1}, NodeID: "node-s"}); err != nil {
		t.Fatal(err)
	}
	mustPut(t, e.s, web(5))
	before := e.nodesOfWeb()
	if slices.Contains(before, "") || !slices.Contains(before, "node-4") {
		t.Fatalf("web on %v, want every replica placed, one on node-4", before)
	}
	e.clock.Advance(10 * time.Minute)
	started := e.clock.Now()
	if err := e.s.SetStarted(ctxT(t), started, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	waiting := func() []string {
		t.Helper()
		nodes, err := e.s.ListNodes(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range nodes {
			if e.s.Waiting(n, e.clock.Now()) {
				out = append(out, n.ID)
			}
		}
		return out
	}
	var back []string // the workers that have reported, which renew on
	at := func(t0 time.Time) {
		e.clock.t = t0
		e.renew("node-s")
		for _, id := range back {
			e.renew(id)
		}
	}
	at(started.Add(5 * time.Minute))
	if lost := e.sweep(); len(lost) != 0 {
		t.Fatalf("in the hold: lost %v", lost)
	}
	if w, _ := e.s.StartWait(e.clock.Now()); !w.Held || !w.WaitEnds.IsZero() || !w.HoldEnded.IsZero() {
		t.Fatalf("in the hold: %+v", w)
	}

	lift := started.Add(5 * time.Minute)
	for _, step := range []struct {
		after   time.Duration
		node    string
		waiting []string
	}{
		{0, "node-1", []string{"node-2", "node-3", "node-4"}},
		{500 * time.Millisecond, "node-2", []string{"node-3", "node-4"}},
		{12 * time.Second, "node-3", []string{"node-4"}},
	} {
		at(lift.Add(step.after))
		if lost := e.sweep(); len(lost) != 0 {
			t.Fatalf("%s after the hold's end, before %s reports: lost %v, want none", step.after, step.node, lost)
		}
		e.renew(step.node)
		back = append(back, step.node)
		if lost := e.sweep(); len(lost) != 0 {
			t.Fatalf("%s after the hold's end, %s reported: lost %v, want none", step.after, step.node, lost)
		}
		if w := waiting(); !slices.Equal(w, step.waiting) {
			t.Fatalf("%s after the hold's end, %s reported: waiting %v, want %v", step.after, step.node, w, step.waiting)
		}
		if got := e.nodesOfWeb(); !slices.Equal(got, before) {
			t.Fatalf("%s after the hold's end: web on %v, want it where it was, %v", step.after, got, before)
		}
	}
	w, ok := e.s.StartWait(e.clock.Now())
	if !ok || w.Held || !w.HoldEnded.Equal(lift) || !w.WaitEnds.Equal(lift.Add(2*time.Minute)) || !w.GraceEnds.Equal(started.Add(2*time.Minute)) {
		t.Fatalf("after the hold's end: %+v, %v; want it ended at %s, the others waiting to %s", w, ok, lift, lift.Add(2*time.Minute))
	}

	// node-4 never reports: it waits to a grace after the hold's end.
	for d := 20 * time.Second; d < 2*time.Minute; d += 10 * time.Second {
		at(lift.Add(d))
		if lost := e.sweep(); len(lost) != 0 {
			t.Fatalf("%s after the hold's end: lost %v, want none", d, lost)
		}
		if got := e.nodesOfWeb(); !slices.Equal(got, before) {
			t.Fatalf("%s after the hold's end: web on %v, want it where it was, %v", d, got, before)
		}
	}
	at(lift.Add(2 * time.Minute))
	if lost := e.sweep(); !slices.Equal(lost, []string{"node-4"}) {
		t.Fatalf("a grace after the hold's end: lost %v, want node-4", lost)
	}
	if w := waiting(); len(w) != 0 {
		t.Fatalf("a grace after the hold's end: waiting %v", w)
	}
	for i, n := range e.nodesOfWeb() {
		switch {
		case before[i] == "node-4" && (n == "node-4" || n == ""):
			t.Fatalf("web %d is on %q after node-4 was lost", i, n)
		case before[i] != "node-4" && n != before[i]:
			t.Fatalf("web %d moved from %s to %s", i, before[i], n)
		}
	}
}

// nodesOfWeb returns the node of each replica of web, by index.
func (e *schedEnv) nodesOfWeb() []string {
	e.t.Helper()
	var out []string
	for _, r := range replicasOf(e.t, e.s, "web") {
		for len(out) <= r.Index {
			out = append(out, "")
		}
		out[r.Index] = r.NodeID
	}
	return out
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
		{ID: "node-old", CPU: 4, Memory: 1 << 30, LastLease: clock.Now().Add(-time.Minute), Eligible: true},
		{ID: "node-new", CPU: 4, Memory: 1 << 30, LastLease: clock.Now(), ReadySince: clock.Now(), Eligible: true},
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
