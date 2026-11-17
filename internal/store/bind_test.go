package store

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// bindEnv is a store with the scheduler, no readiness wait, a fake clock, and
// nodes that report the given addresses ("" for none).
type bindEnv struct {
	*rolloutEnv
	addrs map[string]string
}

func newBindEnv(t *testing.T, addrs map[string]string) *bindEnv {
	t.Helper()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), WithClock(clock.Now), WithMinReady(0))
	e := &bindEnv{rolloutEnv: &rolloutEnv{t: t, s: s, clock: clock, ctx: ctxT(t)}, addrs: addrs}
	e.registerAll()
	return e
}

// registerAll registers every node, or renews its lease at the fake clock's
// time.
func (e *bindEnv) registerAll() {
	e.t.Helper()
	for _, id := range slices.Sorted(maps.Keys(e.addrs)) {
		e.register(id)
	}
}

func (e *bindEnv) register(id string) {
	e.t.Helper()
	if err := e.s.UpsertNode(e.ctx, Node{ID: id, Name: id, CPU: 4, Memory: 8 << 30, Address: e.addrs[id]}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *bindEnv) sweep() {
	e.t.Helper()
	if _, err := e.s.ReconcileNodes(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// replica returns replica idx of app.
func (e *bindEnv) replica(app string, idx int) Replica {
	e.t.Helper()
	for _, r := range replicasOf(e.t, e.s, app) {
		if r.Index == idx {
			return r
		}
	}
	e.t.Fatalf("app %s has no replica %d", app, idx)
	return Replica{}
}

// misplace puts replica idx of app on node, as placement did before it
// checked bind addresses.
func (e *bindEnv) misplace(app string, idx int, node string) {
	e.t.Helper()
	if _, err := e.s.db.Exec(`UPDATE replicas SET node_id = ?, pending_reason = '' WHERE app = ? AND idx = ?`, node, app, idx); err != nil {
		e.t.Fatal(err)
	}
}

// failedStart is what an agent reports for a container whose start failed
// because the machine does not have the address a port binds.
func failedStart(app string, idx, gen int, addr string) Status {
	return Status{Name: fmt.Sprintf("hivemind-%s-%d-g%d", app, idx, gen), App: app, Replica: idx, Generation: gen,
		ContainerID: "c0ffee", State: "created",
		Error: "start: Error starting userland proxy: listen tcp4 " + addr + ":5432: bind: cannot assign requested address"}
}

func bound(name, bind string, volume bool) spec.App {
	a := spec.App{Name: name, Image: "postgres:16", Env: map[string]string{"A": "1"},
		Ports: []spec.Port{{Bind: bind, Host: 5432, Container: 5432}}}
	if volume {
		a.Volumes = []spec.Volume{{Name: "pgdata", Path: "/var/lib/postgresql/data"}}
	}
	return a
}

var threeAddrs = map[string]string{"node-a": "10.0.0.1", "node-b": "10.0.0.2", "node-c": ""}

// A port bound to a node's address takes the replica to that node, whatever
// the scheduler would otherwise choose (node-a: lowest ID, all empty). An
// IPv4-mapped form of the address counts as the address. Loopback and
// wildcard binds go anywhere.
func TestBoundPortGoesToTheNodeWithItsAddress(t *testing.T) {
	e := newBindEnv(t, threeAddrs)
	mustPut(t, e.s, bound("db", "10.0.0.2", true))
	if r := e.replica("db", 0); r.NodeID != "node-b" || r.PendingReason != "" {
		t.Fatalf("db bound to 10.0.0.2: %+v, want it on node-b", r)
	}
	mapped := bound("mapped", "::ffff:10.0.0.2", false)
	mapped.Ports[0].Host = 6000
	mustPut(t, e.s, mapped)
	if r := e.replica("mapped", 0); r.NodeID != "node-b" {
		t.Fatalf("mapped bound to ::ffff:10.0.0.2: %+v, want it on node-b", r)
	}
	for i, bind := range []string{"127.0.0.1", "0.0.0.0", "::", "::1"} {
		name := fmt.Sprintf("any-%d", i)
		a := bound(name, bind, false)
		a.Ports[0].Host = 6001 + i
		mustPut(t, e.s, a)
		if r := e.replica(name, 0); r.NodeID == "" {
			t.Fatalf("%s bound to %s: %+v, want it placed", name, bind, r)
		}
	}
}

// A port bound to an address no node has stays pending, and the reason names
// the address. So do ports bound to two nodes' addresses. When the node that
// has the address cannot take the replica, the reason says why, and that it
// is the only node; once it can, the replica goes there.
func TestBoundPortWithNoNodeToTakeItIsPending(t *testing.T) {
	e := newBindEnv(t, threeAddrs)
	mustPut(t, e.s, bound("db", "10.0.0.9", false))
	r := e.replica("db", 0)
	if r.NodeID != "" || !strings.Contains(r.PendingReason, "no node has address 10.0.0.9, and host port 10.0.0.9:5432/tcp binds address 10.0.0.9") {
		t.Fatalf("db bound to 10.0.0.9: %+v, want pending with the address in the reason", r)
	}

	two := bound("two", "10.0.0.1", false)
	two.Ports = append(two.Ports, spec.Port{Bind: "10.0.0.2", Host: 5433, Container: 5432})
	mustPut(t, e.s, two)
	if r := e.replica("two", 0); r.NodeID != "" || !strings.Contains(r.PendingReason, "bind addresses 10.0.0.1, 10.0.0.2, and no one node has all of them") {
		t.Fatalf("two bound to two nodes' addresses: %+v", r)
	}

	if _, err := e.s.SetNodeEligibility(e.ctx, "node-b", false); err != nil {
		t.Fatal(err)
	}
	mustPut(t, e.s, bound("late", "10.0.0.2", false))
	r = e.replica("late", 0)
	want := "every ready node is ineligible for new replicas (host port 10.0.0.2:5432/tcp binds address 10.0.0.2, which only node node-b has)"
	if r.NodeID != "" || r.PendingReason != want {
		t.Fatalf("late, with node-b ineligible: %+v, want pending with %q", r, want)
	}
	if _, err := e.s.SetNodeEligibility(e.ctx, "node-b", true); err != nil {
		t.Fatal(err)
	}
	if r := e.replica("late", 0); r.NodeID != "node-b" {
		t.Fatalf("late after node-b is eligible: %+v, want it on node-b", r)
	}
	checkInvariants(t, e.s)
}

// The server's node without --advertise reports 127.0.0.1. It has no
// workers, so it takes a port bound to any address, as before.
func TestServerWithoutAdvertiseTakesAnyAddress(t *testing.T) {
	e := newBindEnv(t, map[string]string{"node-s": "127.0.0.1"})
	mustPut(t, e.s, bound("db", "192.0.2.7", true))
	if r := e.replica("db", 0); r.NodeID != "node-s" {
		t.Fatalf("db on a single server without --advertise: %+v, want it on node-s", r)
	}
}

// A replica placed on a node without the address it binds, before placement
// checked addresses, has a container that fails to start there. The sweep
// places it again on the node that has the address. One whose container runs
// on its node is left alone: that machine has the address too.
func TestSweepPlacesAgainAReplicaOnANodeWithoutItsAddress(t *testing.T) {
	e := newBindEnv(t, threeAddrs)
	mustPut(t, e.s, bound("db", "10.0.0.2", false))
	e.misplace("db", 0, "node-a")
	e.report("node-a", failedStart("db", 0, 1, "10.0.0.2"))
	e.sweep()
	if r := e.replica("db", 0); r.NodeID != "node-b" {
		t.Fatalf("db after the sweep: %+v, want it placed again on node-b", r)
	}
	checkInvariants(t, e.s)

	mustPut(t, e.s, bound("multi", "10.0.0.2", false))
	e.misplace("multi", 0, "node-c")
	e.report("node-c", container("multi", 0, 1))
	e.sweep()
	if r := e.replica("multi", 0); r.NodeID != "node-c" {
		t.Fatalf("multi, running on node-c: %+v, want it left there", r)
	}

	// With no node that has the address, the replica waits with the reason.
	mustPut(t, e.s, bound("lost", "10.0.0.9", false))
	e.misplace("lost", 0, "node-a")
	e.report("node-a", failedStart("lost", 0, 1, "10.0.0.9"))
	e.sweep()
	if r := e.replica("lost", 0); r.NodeID != "" || !strings.Contains(r.PendingReason, "no node has address 10.0.0.9") {
		t.Fatalf("lost after the sweep: %+v, want pending with the address in the reason", r)
	}
}

// A volume replica on a node without its address stays, since its data is
// there, and its reason says why it cannot run and what to do. A deploy with
// the port on that node's address runs it there.
func TestSweepGivesAVolumeReplicaOnANodeWithoutItsAddressAReason(t *testing.T) {
	e := newBindEnv(t, threeAddrs)
	mustPut(t, e.s, bound("db", "10.0.0.2", true))
	e.misplace("db", 0, "node-a")
	e.report("node-a", failedStart("db", 0, 1, "10.0.0.2"))
	for range 3 {
		e.clock.Advance(5 * time.Second)
		e.registerAll()
		e.sweep()
	}
	r := e.replica("db", 0)
	for _, part := range []string{
		"host port 10.0.0.2:5432/tcp binds address 10.0.0.2, and node node-a has address 10.0.0.1",
		"its volume is on that node, so it stays there",
		"deploy the app with the port on 10.0.0.1",
	} {
		if r.NodeID != "node-a" || !strings.Contains(r.PendingReason, part) {
			t.Fatalf("db after the sweeps: %+v, want it on node-a with %q in the reason", r, part)
		}
	}
	checkInvariants(t, e.s)

	mustPut(t, e.s, bound("db", "10.0.0.1", true))
	e.advance()
	e.sweep()
	if r := e.replica("db", 0); r.NodeID != "node-a" || r.Generation != 2 || r.PendingReason != "" {
		t.Fatalf("db after a deploy on node-a's address: %+v, want generation 2 on node-a with no reason", r)
	}
}

// A rollout to a spec whose port binds another node's address moves the
// replica there with a surge: to node-c, which has the address, not node-b,
// which placement would choose otherwise. A replica with a volume cannot
// move, so the rollout stops with the reason, before the replica changes.
func TestRolloutToAnotherNodesAddress(t *testing.T) {
	addrs := map[string]string{"node-a": "10.0.0.1", "node-b": "10.0.0.2", "node-c": "10.0.0.3"}
	e := newBindEnv(t, addrs)
	mustPut(t, e.s, bound("api", "10.0.0.1", false))
	e.report("node-a", container("api", 0, 1))
	mustPut(t, e.s, bound("api", "10.0.0.3", false))
	e.advance()
	if got := e.surgesOf("api"); len(got) != 1 || got[0] != "node-c" {
		t.Fatalf("api's surge is on %v, want node-c", got)
	}
	e.report("node-c", container("api", 0, 2))
	e.advance()
	if r := e.replica("api", 0); r.NodeID != "node-c" || r.Generation != 2 {
		t.Fatalf("api after the rollout: %+v, want generation 2 on node-c", r)
	}

	mustPut(t, e.s, bound("db", "10.0.0.1", true))
	e.report("node-a", container("db", 0, 1))
	mustPut(t, e.s, bound("db", "10.0.0.3", true))
	e.advance()
	ro := e.rollout("db")
	if ro.State != RolloutPaused || !strings.Contains(ro.Reason, "replica 0 has its volume on node node-a, which cannot run generation 2: "+
		"host port 10.0.0.3:5432/tcp binds address 10.0.0.3, and node node-a has address 10.0.0.1") {
		t.Fatalf("db's rollout: %+v, want it paused with the reason", ro)
	}
	if r := e.replica("db", 0); r.NodeID != "node-a" || r.Generation != 1 {
		t.Fatalf("db after the paused rollout: %+v, want generation 1 on node-a", r)
	}
}

// A drain cannot move a replica whose port binds the drained node's address,
// and says so.
func TestDrainSaysABoundReplicaCannotLeave(t *testing.T) {
	e := newBindEnv(t, map[string]string{"node-a": "10.0.0.1", "node-b": "10.0.0.2"})
	mustPut(t, e.s, bound("db", "10.0.0.2", false))
	d, err := e.s.DrainNode(e.ctx, "node-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := "db/0: host port 10.0.0.2:5432/tcp binds address 10.0.0.2, which no other node has"
	if got := blocked(d); len(got) != 1 || got[0] != want || len(d.Moving) != 0 {
		t.Fatalf("drain of node-b: blocked %v, moving %v; want %q", got, moving(d), want)
	}
}

// surgesOf returns the node of each surge of app.
func (e *bindEnv) surgesOf(app string) []string {
	e.t.Helper()
	ss, err := querySurges(e.ctx, e.s.db, `s.app = ?`, app)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, s := range ss {
		out = append(out, s.NodeID)
	}
	return out
}
