package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

func dbApp() spec.App {
	return spec.App{Name: "db", Image: "postgres:16", Replicas: 1, Volumes: []spec.Volume{{Name: "data", Path: "/var/lib/data"}}}
}

// pinVolumesTo2 places volume apps on node-2 and everything else on the
// first node, so a test knows where each replica runs.
func pinVolumesTo2(nodes []Node, placed []Assignment, app spec.App, idx int, now time.Time) (string, error) {
	if len(app.Volumes) > 0 {
		for _, n := range nodes {
			if n.ID == "node-2" {
				return n.ID, nil
			}
		}
		return "", errors.New("node-2 is not registered")
	}
	return FirstNode(nodes, placed, app, idx, now)
}

func newRemoveEnv(t *testing.T, place PlaceFunc) *schedEnv {
	t.Helper()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	opts := []Option{WithClock(clock.Now), WithMinReady(0)}
	if place != nil {
		opts = append(opts, WithPlacement(place))
	}
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), opts...)
	if err := s.InitCluster(ctxT(t), Cluster{AdminTokenHash: [32]byte{1}, NodeID: "node-1"}); err != nil {
		t.Fatal(err)
	}
	return &schedEnv{t: t, s: s, clock: clock}
}

func TestRemoveNodeRefusesTheServersNode(t *testing.T) {
	e := newRemoveEnv(t, nil)
	e.renew("node-1")
	e.clock.Advance(time.Minute)
	_, err := e.s.RemoveNode(ctxT(t), "node-1", true)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "runs the server") {
		t.Fatalf("removing the server's node: %v", err)
	}
	if _, err := e.s.GetNode(ctxT(t), "node-1"); err != nil {
		t.Fatalf("the server's node is gone: %v", err)
	}
}

func TestRemoveNodeRefusesALiveNodeWithoutForce(t *testing.T) {
	e := newRemoveEnv(t, nil)
	e.renew("node-1")
	e.renew("node-2")
	e.clock.Advance(5 * time.Second)
	_, err := e.s.RemoveNode(ctxT(t), "node-2", false)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "renewed its lease 5s ago") {
		t.Fatalf("removing a live node: %v", err)
	}
	if removed, _ := e.s.IsNodeRemoved(ctxT(t), "node-2"); removed {
		t.Fatal("a refused removal put the node on the denylist")
	}
	r, err := e.s.RemoveNode(ctxT(t), "node-2", true)
	if err != nil || r.ID != "node-2" || r.Name != "node-2" {
		t.Fatalf("forced removal: %+v, %v", r, err)
	}
	checkInvariants(t, e.s)
}

func TestRemoveNodeUnknown(t *testing.T) {
	e := newRemoveEnv(t, nil)
	if _, err := e.s.RemoveNode(ctxT(t), "node-9", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
}

// A removed node's replicas are placed again; a volume replica is held with
// a reason and is not placed, even when nodes are free; the node's rows go.
func TestRemoveNodeReschedulesAndHoldsVolumeReplicas(t *testing.T) {
	e := newRemoveEnv(t, nil)
	e.renew("node-1")
	e.renew("node-2")
	e.renew("node-3")
	ctx := ctxT(t)
	// Place web's three replicas one per node, and db on node-2.
	e.s.place = func(nodes []Node, placed []Assignment, app spec.App, idx int, now time.Time) (string, error) {
		if len(app.Volumes) > 0 || app.Name == "old" {
			return "node-2", nil
		}
		return nodes[idx%len(nodes)].ID, nil
	}
	mustPut(t, e.s, web(3))
	mustPut(t, e.s, dbApp())
	if e.nodeOf("web", 1) != "node-2" || e.nodeOf("db", 0) != "node-2" {
		t.Fatalf("setup: web/1 on %s, db/0 on %s", e.nodeOf("web", 1), e.nodeOf("db", 0))
	}
	// node-2 reports its containers, and holds a replica marked for removal
	// and a surge.
	if err := e.s.ReportStatus(ctx, "node-2", []Status{
		{Name: "hivemind-web-1-g1", App: "web", Replica: 1, Generation: 1, State: "running"},
		{Name: "hivemind-db-0-g1", App: "db", Replica: 0, Generation: 1, State: "running"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec(`INSERT INTO surges (app, idx, node_id, generation, desired, updated_at) VALUES ('web', 0, 'node-2', 1, 'running', 0)`); err != nil {
		t.Fatal(err)
	}
	// A deleted app whose container is still on node-2.
	mustPut(t, e.s, spec.App{Name: "old", Image: "busybox", Replicas: 1})
	if err := e.s.DeleteApp(ctx, "old"); err != nil {
		t.Fatal(err)
	}

	// From now on placement is the default first-node rule over what is left.
	e.s.place = FirstNode
	r, err := e.s.RemoveNode(ctx, "node-2", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Rescheduled != 1 || r.Held != 1 {
		t.Fatalf("removal %+v, want 1 rescheduled and 1 held", r)
	}
	checkInvariants(t, e.s)
	if n := e.nodeOf("web", 1); n == "" || n == "node-2" {
		t.Fatalf("web/1 after removal on %q, want another node", n)
	}
	db := replicasOf(t, e.s, "db")[0]
	if db.NodeID != "" || !strings.Contains(db.PendingReason, "was removed") || !strings.Contains(db.PendingReason, "new volume name") {
		t.Fatalf("db after removal: %+v", db)
	}
	// Sweeps, registrations and reports do not place the held replica.
	e.renew("node-4")
	e.sweep()
	if db := replicasOf(t, e.s, "db")[0]; db.NodeID != "" {
		t.Fatalf("held replica placed on %s", db.NodeID)
	}
	if reps := replicasOf(t, e.s, "old"); len(reps) != 0 {
		t.Fatalf("replicas marked for removal on the node remain: %+v", reps)
	}
	var apps int
	if err := e.s.db.QueryRow(`SELECT count(*) FROM apps WHERE name = 'old'`).Scan(&apps); err != nil || apps != 0 {
		t.Fatalf("the deleted app is still stored: %d, %v", apps, err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM surges WHERE node_id = 'node-2'`,
		`SELECT count(*) FROM container_status WHERE node_id = 'node-2'`,
		`SELECT count(*) FROM nodes WHERE id = 'node-2'`,
		`SELECT count(*) FROM replicas WHERE node_id = 'node-2'`,
	} {
		var n int
		if err := e.s.db.QueryRow(q).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d, %v", q, n, err)
		}
	}
}

// A deploy that changes the spec releases a held replica, which is then
// placed like a new one. A deploy of the same spec does not.
func TestHeldReplicaIsReleasedByASpecChange(t *testing.T) {
	e := newRemoveEnv(t, pinVolumesTo2)
	e.renew("node-1")
	e.renew("node-2")
	mustPut(t, e.s, dbApp())
	e.clock.Advance(time.Minute)
	e.renew("node-1")
	if _, err := e.s.RemoveNode(ctxT(t), "node-2", false); err != nil {
		t.Fatal(err)
	}
	e.s.place = FirstNode
	mustPut(t, e.s, dbApp())
	if db := replicasOf(t, e.s, "db")[0]; db.NodeID != "" {
		t.Fatalf("the same spec released the held replica onto %s", db.NodeID)
	}
	changed := dbApp()
	changed.Volumes[0].Name = "data2"
	mustPut(t, e.s, changed)
	db := replicasOf(t, e.s, "db")[0]
	if db.NodeID != "node-1" || db.PendingReason != "" || db.Generation != 2 {
		t.Fatalf("db after a spec change: %+v", db)
	}
	checkInvariants(t, e.s)
}

// A removed node is on the denylist, which survives a restart, and the store
// refuses its ID everywhere a node writes.
func TestRemovedNodeIsDenied(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	path := filepath.Join(t.TempDir(), "data", "hivemind.db")
	s := openT(t, path, WithClock(clock.Now), WithMinReady(0))
	ctx := ctxT(t)
	if err := s.UpsertNode(ctx, Node{ID: "node-2", Name: "two", CPU: 1, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if _, err := s.RemoveNode(ctx, "node-2", false); err != nil {
		t.Fatalf("removing a lost node without force: %v", err)
	}
	s.Close()

	s = openT(t, path, WithClock(clock.Now))
	if removed, err := s.IsNodeRemoved(ctx, "node-2"); err != nil || !removed {
		t.Fatalf("IsNodeRemoved after reopen: %v, %v", removed, err)
	}
	if removed, _ := s.IsNodeRemoved(ctx, "node-3"); removed {
		t.Fatal("an unknown node is on the denylist")
	}
	list, err := s.RemovedNodes(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "node-2" || list[0].Name != "two" || !list[0].RemovedAt.Equal(clock.Now()) {
		t.Fatalf("RemovedNodes: %+v, %v", list, err)
	}
	if err := s.UpsertNode(ctx, Node{ID: "node-2", Name: "two"}); !errors.Is(err, ErrNodeRemoved) {
		t.Fatalf("register a removed node: %v", err)
	}
	if err := s.ReportStatus(ctx, "node-2", nil); !errors.Is(err, ErrNodeRemoved) {
		t.Fatalf("report from a removed node: %v", err)
	}
	if _, err := s.GetNode(ctx, "node-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetNode of a removed node: %v", err)
	}
	if _, err := s.RemoveNode(ctx, "node-2", true); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "already removed") {
		t.Fatalf("second removal: %v", err)
	}
	// A node that never registered is not removed.
	if err := s.ReportStatus(ctx, "node-3", nil); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrNodeRemoved) {
		t.Fatalf("report from an unknown node: %v", err)
	}
	checkInvariants(t, s)
}
