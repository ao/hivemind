package store

import (
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// attemptAt returns when placement last wrote replica idx of app: each run
// that leaves it pending writes its reason and the time.
func (e *schedEnv) attemptAt(app string, idx int) (time.Time, string) {
	e.t.Helper()
	var at int64
	var reason string
	if err := e.s.db.QueryRow(`SELECT updated_at, pending_reason FROM replicas WHERE app = ? AND idx = ?`, app, idx).Scan(&at, &reason); err != nil {
		e.t.Fatal(err)
	}
	return time.Unix(0, at), reason
}

// The scale run: one replica that no node could take made every sweep and
// every node's report run the whole placement again, to the same end. With
// nothing changed, placement is skipped; a change to what it reads (a node,
// the time a node becomes ready, a replica that leaves a node) runs it.
func TestPlacementSkippedWhenNothingChanged(t *testing.T) {
	e := newSchedEnv(t, 30*time.Second, node1)
	ctx := ctxT(t)
	// tick moves the clock by d, renewing the leases of nodes every 10 s so
	// that none lapses.
	tick := func(d time.Duration, nodes ...string) {
		t.Helper()
		for d > 0 {
			step := min(d, 10*time.Second)
			e.clock.Advance(step)
			d -= step
			for _, n := range nodes {
				e.renew(n)
			}
		}
	}
	tick(30*time.Second, node1)
	port := []spec.Port{{Host: 8080, Container: 80}}
	if _, err := e.s.PutApp(ctx, spec.App{Name: "blk", Image: "nginx:alpine", Replicas: 1, Ports: port}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.PutApp(ctx, spec.App{Name: "web", Image: "nginx:alpine", Replicas: 1, Ports: port}); err != nil {
		t.Fatal(err)
	}
	first, reason := e.attemptAt("web", 0)
	if !strings.Contains(reason, "8080") {
		t.Fatalf("web's reason: %q, want the host port", reason)
	}

	// Nothing changes: the sweeps, a lease renewal and a report place
	// nothing and write nothing.
	for range 3 {
		tick(time.Second, node1)
		e.sweep()
	}
	if err := e.s.ReportStatus(ctx, node1, []Status{{Name: "hivemind-blk-0-g1", ContainerID: "b", App: "blk", Generation: 1, State: "running", Ready: true}}); err != nil {
		t.Fatal(err)
	}
	if at, _ := e.attemptAt("web", 0); !at.Equal(first) {
		t.Fatalf("placement ran at %v with nothing changed since %v", at, first)
	}

	// A new node: placement runs, and the node is not ready for 30 s.
	e.clock.Advance(time.Second)
	e.renew(nodeB)
	second, reason := e.attemptAt("web", 0)
	if !second.After(first) {
		t.Fatalf("placement did not run for a new node: last at %v", second)
	}
	tick(20*time.Second, node1, nodeB)
	e.sweep()
	if at, r := e.attemptAt("web", 0); !at.Equal(second) || r != reason {
		t.Fatalf("placement ran at %v (%q), with nothing changed since %v (%q)", at, r, second, reason)
	}
	// 30 s after it registered, the node takes the replica.
	tick(10*time.Second, node1, nodeB)
	e.sweep()
	reps := replicasOf(t, e.s, "web")
	if len(reps) != 1 || reps[0].NodeID != nodeB {
		t.Fatalf("web once %s is ready: %+v", nodeB, reps)
	}
	checkInvariants(t, e.s)

	// A replica that leaves a node frees its port: placement runs again.
	if _, err := e.s.PutApp(ctx, spec.App{Name: "web", Image: "nginx:alpine", Replicas: 3, Ports: port}); err != nil {
		t.Fatal(err)
	}
	pend := func() int {
		n := 0
		for _, r := range replicasOf(t, e.s, "web") {
			if r.NodeID == "" {
				n++
			}
		}
		return n
	}
	if n := pend(); n != 2 {
		t.Fatalf("%d of web's 3 replicas pending, want 2", n)
	}
	e.sweep()
	if err := e.s.DeleteApp(ctx, "blk"); err != nil {
		t.Fatal(err)
	}
	// blk's container still holds the port until node1 reports it gone.
	if n := pend(); n != 2 {
		t.Fatalf("%d pending while blk's container runs, want 2", n)
	}
	if err := e.s.ReportStatus(ctx, node1, nil); err != nil {
		t.Fatal(err)
	}
	if n := pend(); n != 1 {
		t.Fatalf("%d pending after blk's container went, want 1", n)
	}
	checkInvariants(t, e.s)
}

// A placement function other than the scheduler may read anything, so
// placement is never skipped with one.
func TestPlacementNotSkippedWithAnotherPlaceFunc(t *testing.T) {
	calls := 0
	refuse := func([]Node, []Assignment, spec.App, int, time.Time) (string, error) {
		calls++
		return "", errNoNode
	}
	s := newT(t, WithPlacement(refuse))
	ctx := ctxT(t)
	mustPut(t, s, web(1))
	before := calls
	for range 3 {
		if _, err := s.ReconcileNodes(ctx, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if calls-before != 3 {
		t.Fatalf("%d placements in 3 sweeps, want 3", calls-before)
	}
}

var errNoNode = errString("no node will do")

type errString string

func (e errString) Error() string { return string(e) }
