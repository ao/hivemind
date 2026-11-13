package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

const node1 = "node-1"

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func openT(t *testing.T, path string, opts ...Option) *Store {
	t.Helper()
	s, err := Open(ctxT(t), path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newT opens a store in a fresh directory with one node registered. Its
// nodes take replicas at once: the MinReady wait is tested on its own, with
// the scheduler, in placement_test.go.
func newT(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), append([]Option{WithMinReady(0)}, opts...)...)
	if err := s.UpsertNode(ctxT(t), Node{ID: node1, Name: "one", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	return s
}

func web(replicas int) spec.App {
	return spec.App{Name: "web", Image: "nginx:alpine", Replicas: replicas, Env: map[string]string{"A": "1"}}
}

func mustPut(t *testing.T, s *Store, a spec.App) App {
	t.Helper()
	got, err := s.PutApp(ctxT(t), a)
	if err != nil {
		t.Fatalf("PutApp: %v", err)
	}
	checkInvariants(t, s)
	return got
}

func replicasOf(t *testing.T, s *Store, app string) []Replica {
	t.Helper()
	r, err := s.Replicas(ctxT(t), app)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// checkInvariants checks what must hold after any committed change.
func checkInvariants(t *testing.T, s *Store) {
	t.Helper()
	if err := invariants(context.Background(), s.db); err != nil {
		t.Fatal(err)
	}
}

func invariants(ctx context.Context, db *sql.DB) error {
	var ic string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ic); err != nil {
		return err
	}
	if ic != "ok" {
		return fmt.Errorf("integrity_check: %s", ic)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	bad := rows.Next()
	rows.Close()
	if bad {
		return errors.New("foreign_key_check reports violations")
	}
	var v int
	if err := db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	if v != SchemaVersion {
		return fmt.Errorf("schema version %d, want %d", v, SchemaVersion)
	}

	type app struct {
		name, specJSON, tmpl   string
		gen, replicas, deleted int
	}
	var apps []app
	rows, err = db.QueryContext(ctx, `SELECT name, spec, template, generation, replicas, deleted FROM apps`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a app
		if err := rows.Scan(&a.name, &a.specJSON, &a.tmpl, &a.gen, &a.replicas, &a.deleted); err != nil {
			rows.Close()
			return err
		}
		apps = append(apps, a)
	}
	rows.Close()
	for _, a := range apps {
		var sp spec.App
		if err := json.Unmarshal([]byte(a.specJSON), &sp); err != nil {
			return fmt.Errorf("app %s: spec: %v", a.name, err)
		}
		if err := sp.Validate(); err != nil {
			return fmt.Errorf("app %s: stored spec invalid: %v", a.name, err)
		}
		if sp.Name != a.name || sp.Template() != a.tmpl || sp.Replicas != a.replicas {
			return fmt.Errorf("app %s: name, template or replicas columns disagree with the spec", a.name)
		}
		type rep struct {
			idx, gen int
			desired  string
			node     sql.NullString
		}
		var reps []rep
		rr, err := db.QueryContext(ctx, `SELECT idx, generation, desired, node_id FROM replicas WHERE app = ? ORDER BY idx`, a.name)
		if err != nil {
			return err
		}
		for rr.Next() {
			var r rep
			if err := rr.Scan(&r.idx, &r.gen, &r.desired, &r.node); err != nil {
				rr.Close()
				return err
			}
			reps = append(reps, r)
		}
		rr.Close()
		running := 0
		// A running replica is at the app's generation, or, while a rollout to
		// it is in progress, at an older one. It is never ahead of the app.
		var rolling bool
		var target int
		var rstate string
		err = db.QueryRowContext(ctx, `SELECT state, target FROM rollouts WHERE app = ?`, a.name).Scan(&rstate, &target)
		if err == nil {
			rolling = rstate == "in_progress" && target == a.gen
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, r := range reps {
			switch r.desired {
			case "running":
				if a.deleted == 1 {
					return fmt.Errorf("app %s: deleted but replica %d is running", a.name, r.idx)
				}
				if r.idx >= a.replicas || r.gen > a.gen || (r.gen != a.gen && !rolling) {
					return fmt.Errorf("app %s: running replica %d gen %d, app has %d replicas gen %d", a.name, r.idx, r.gen, a.replicas, a.gen)
				}
				running++
			case "removed":
				if a.deleted == 0 && r.idx < a.replicas {
					return fmt.Errorf("app %s: replica %d removed but within the replica count", a.name, r.idx)
				}
				if !r.node.Valid {
					return fmt.Errorf("app %s: removed replica %d has no node", a.name, r.idx)
				}
			}
		}
		if a.deleted == 0 && running != a.replicas {
			return fmt.Errorf("app %s: %d running replicas, want %d", a.name, running, a.replicas)
		}
		if a.deleted == 1 && len(reps) == 0 {
			return fmt.Errorf("app %s: deleted with no replicas but not purged", a.name)
		}
	}
	return nil
}

func TestOpenFromEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "hivemind.db")
	s := openT(t, path)
	checkInvariants(t, s)

	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600, path + "-wal": 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", filepath.Base(p), got, want)
		}
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode %q, %v", mode, err)
	}
	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout == 0 {
		t.Fatalf("busy_timeout %d, %v", timeout, err)
	}
	apps, err := s.ListApps(ctxT(t))
	if err != nil || len(apps) != 0 {
		t.Fatalf("ListApps on an empty store: %v, %v", apps, err)
	}
}

func TestOpenTightensModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hivemind.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	openT(t, path)
	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		fi, _ := os.Stat(p)
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", filepath.Base(p), got, want)
		}
	}
}

func TestOpenRejectsBadPath(t *testing.T) {
	for _, p := range []string{"", "a?b", "a#b"} {
		if _, err := Open(ctxT(t), p); !errors.Is(err, ErrInvalid) {
			t.Errorf("Open(%q): %v, want ErrInvalid", p, err)
		}
	}
}

func TestMigrateForwardAndRefuseNewer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := ctxT(t)
	version := func() int {
		var v int
		if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	list := []string{`CREATE TABLE a (x INTEGER)`}
	if err := migrate(ctx, db, list); err != nil {
		t.Fatal(err)
	}
	if v := version(); v != 1 {
		t.Fatalf("version %d after the first migration", v)
	}
	if _, err := db.Exec(`INSERT INTO a VALUES (7)`); err != nil {
		t.Fatal(err)
	}
	// A later release appends a migration; the data written before is kept.
	list = append(list, `ALTER TABLE a ADD COLUMN y INTEGER NOT NULL DEFAULT 3`)
	if err := migrate(ctx, db, list); err != nil {
		t.Fatal(err)
	}
	var x, y int
	if err := db.QueryRow(`SELECT x, y FROM a`).Scan(&x, &y); err != nil || x != 7 || y != 3 {
		t.Fatalf("after migration 2: %d %d %v", x, y, err)
	}
	if v := version(); v != 2 {
		t.Fatalf("version %d after the second migration", v)
	}
	// Running again changes nothing.
	if err := migrate(ctx, db, list); err != nil || version() != 2 {
		t.Fatalf("re-run: %v", err)
	}
	// An older build refuses the newer file and leaves it alone.
	if err := migrate(ctx, db, list[:1]); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("older build: %v, want ErrNewerSchema", err)
	}
	if version() != 2 {
		t.Fatal("refused migration changed the version")
	}
	// A failing migration rolls back the whole step.
	bad := append(list, `CREATE TABLE b (x INTEGER); CREATE TABLE a (x INTEGER)`)
	if err := migrate(ctx, db, bad); err == nil {
		t.Fatal("failing migration returned nil")
	}
	if version() != 2 {
		t.Fatal("failed migration changed the version")
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'b'`).Scan(&n)
	if n != 0 {
		t.Fatal("failed migration left table b behind")
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hivemind.db")
	s := openT(t, path)
	if _, err := s.db.Exec(`UPDATE schema_version SET version = ?`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctxT(t), path); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("Open: %v, want ErrNewerSchema", err)
	}
}

func TestPutAppGenerationRule(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)

	a := mustPut(t, s, web(2))
	if a.Generation != 1 || a.Spec.Replicas != 2 {
		t.Fatalf("create: gen %d replicas %d", a.Generation, a.Spec.Replicas)
	}

	// The same spec, and one that only differs in replica count or in fields
	// Normalize fills in, keep the generation.
	same := web(2)
	same.Image = " nginx:alpine "
	if a = mustPut(t, s, same); a.Generation != 1 {
		t.Fatalf("same spec: gen %d", a.Generation)
	}
	if a = mustPut(t, s, web(3)); a.Generation != 1 || a.Spec.Replicas != 3 {
		t.Fatalf("replicas change: gen %d replicas %d", a.Generation, a.Spec.Replicas)
	}
	if n := len(replicasOf(t, s, "web")); n != 3 {
		t.Fatalf("%d replicas after scaling by PutApp", n)
	}

	// A template change bumps the generation once and starts a rollout. The
	// replicas keep their generation until the rollout moves them.
	changed := web(3)
	changed.Env = map[string]string{"A": "2"}
	if a = mustPut(t, s, changed); a.Generation != 2 {
		t.Fatalf("env change: gen %d", a.Generation)
	}
	for _, r := range replicasOf(t, s, "web") {
		if r.Generation != 1 || r.Desired != Running || r.NodeID != node1 {
			t.Fatalf("replica after the change: %+v", r)
		}
	}
	if ro, err := s.GetRollout(ctx, "web"); err != nil || ro.Target != 2 || ro.State != RolloutInProgress {
		t.Fatalf("rollout after the change: %+v %v", ro, err)
	}
	// One node, so the first move is in place: replica 0 goes to generation 2
	// and the others wait for it.
	if err := s.AdvanceRollouts(ctx); err != nil {
		t.Fatal(err)
	}
	if reps := replicasOf(t, s, "web"); reps[0].Generation != 2 || reps[1].Generation != 1 || reps[2].Generation != 1 {
		t.Fatalf("after the first step: %+v", reps)
	}
	if a = mustPut(t, s, changed); a.Generation != 2 {
		t.Fatalf("same spec again: gen %d", a.Generation)
	}

	got, err := s.GetApp(ctx, "web")
	if err != nil || got.Generation != 2 || got.Spec.Env["A"] != "2" {
		t.Fatalf("GetApp: %+v %v", got, err)
	}
}

func TestPutAppInvalidWritesNothing(t *testing.T) {
	s := newT(t)
	bad := web(1)
	bad.Name = "Bad Name"
	if _, err := s.PutApp(ctxT(t), bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("PutApp: %v, want ErrInvalid", err)
	}
	if apps, _ := s.ListApps(ctxT(t)); len(apps) != 0 {
		t.Fatalf("an invalid app was stored: %+v", apps)
	}
}

func TestPutAppDoesNotModifyCaller(t *testing.T) {
	s := newT(t)
	a := web(1)
	a.Ports = []spec.Port{{Host: 8080, Container: 80}}
	mustPut(t, s, a)
	if a.Ports[0].Bind != "" || a.Ports[0].Protocol != "" {
		t.Fatalf("PutApp changed the caller's spec: %+v", a.Ports[0])
	}
	got, _ := s.GetApp(ctxT(t), "web")
	if got.Spec.Ports[0].Bind != spec.DefaultBind {
		t.Fatalf("stored bind %q, want the 127.0.0.1 default", got.Spec.Ports[0].Bind)
	}
}

func TestDeleteApp(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	mustPut(t, s, web(2))
	// The node reports both containers running.
	st := []Status{
		{Name: "hivemind-web-0-g1", ContainerID: "c0", App: "web", Replica: 0, Generation: 1, State: "running"},
		{Name: "hivemind-web-1-g1", ContainerID: "c1", App: "web", Replica: 1, Generation: 1, State: "running"},
	}
	if err := s.ReportStatus(ctx, node1, st); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteApp(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, s)
	if _, err := s.GetApp(ctx, "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetApp after delete: %v", err)
	}
	if err := s.DeleteApp(ctx, "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	as, _ := s.AssignmentsForNode(ctx, node1)
	if len(as) != 2 || as[0].Desired != Removed || as[1].Desired != Removed {
		t.Fatalf("assignments after delete: %+v", as)
	}

	// One container is gone; its replica row goes, the other stays.
	if err := s.ReportStatus(ctx, node1, st[1:]); err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, s)
	if r := replicasOf(t, s, "web"); len(r) != 1 || r[0].Index != 1 {
		t.Fatalf("replicas after one container went: %+v", r)
	}
	// Both gone: the app is purged.
	if err := s.ReportStatus(ctx, node1, nil); err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, s)
	if r := replicasOf(t, s, "web"); len(r) != 0 {
		t.Fatalf("replicas left after purge: %+v", r)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM apps`).Scan(&n)
	if n != 0 {
		t.Fatal("deleted app row not purged")
	}
}

func TestDeleteThenPutAgainBumpsGeneration(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	mustPut(t, s, web(1))
	if err := s.ReportStatus(ctx, node1, []Status{{Name: "hivemind-web-0-g1", App: "web", Generation: 1, State: "running"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteApp(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	// Created again before the node removed the old container: the old
	// container's generation must not match the new replica.
	a := mustPut(t, s, web(1))
	if a.Generation != 2 {
		t.Fatalf("generation %d after delete and create, want 2", a.Generation)
	}
	r := replicasOf(t, s, "web")
	if len(r) != 1 || r[0].Generation != 2 || r[0].Desired != Running {
		t.Fatalf("replicas: %+v", r)
	}
}

func TestDeleteWithoutNodeIsImmediate(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "hivemind.db"))
	mustPut(t, s, web(2))
	for _, r := range replicasOf(t, s, "web") {
		if r.NodeID != "" || r.PendingReason == "" {
			t.Fatalf("replica without nodes: %+v", r)
		}
	}
	if err := s.DeleteApp(ctxT(t), "web"); err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, s)
	var n int
	s.db.QueryRow(`SELECT count(*) FROM apps`).Scan(&n)
	if n != 0 {
		t.Fatal("an app that never reached a node should be purged on delete")
	}
}

func TestScale(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	mustPut(t, s, web(2))

	a, err := s.Scale(ctx, "web", 4)
	if err != nil || a.Generation != 1 || a.Spec.Replicas != 4 {
		t.Fatalf("scale up: %+v %v", a, err)
	}
	checkInvariants(t, s)
	if n := len(replicasOf(t, s, "web")); n != 4 {
		t.Fatalf("%d replicas after scale up", n)
	}

	if _, err := s.Scale(ctx, "web", 1); err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, s)
	var running, removed int
	for _, r := range replicasOf(t, s, "web") {
		if r.Desired == Running {
			running++
		} else {
			removed++
		}
	}
	if running != 1 || removed != 3 {
		t.Fatalf("after scale down: %d running, %d removed", running, removed)
	}
	// Scaling back up revives the removed rows.
	if _, err := s.Scale(ctx, "web", 2); err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, s)
	got, _ := s.GetApp(ctx, "web")
	if got.Spec.Replicas != 2 || got.Generation != 1 {
		t.Fatalf("GetApp after scale: %+v", got)
	}

	for _, n := range []int{0, -1, spec.MaxReplicas + 1} {
		if _, err := s.Scale(ctx, "web", n); !errors.Is(err, ErrInvalid) {
			t.Errorf("Scale(%d): %v, want ErrInvalid", n, err)
		}
	}
	if _, err := s.Scale(ctx, "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Scale of a missing app: %v", err)
	}

	vol := spec.App{Name: "db", Image: "postgres:16", Volumes: []spec.Volume{{Name: "pgdata", Path: "/var/lib/postgresql/data"}}}
	mustPut(t, s, vol)
	if _, err := s.Scale(ctx, "db", 2); !errors.Is(err, ErrInvalid) {
		t.Errorf("scaling an app with a volume past 1: %v", err)
	}
	checkInvariants(t, s)
}

func TestPendingUntilANodeRegisters(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "hivemind.db"), WithMinReady(0))
	ctx := ctxT(t)
	mustPut(t, s, web(2))
	for _, r := range replicasOf(t, s, "web") {
		if r.NodeID != "" || r.PendingReason != "no nodes are registered" {
			t.Fatalf("pending replica: %+v", r)
		}
	}
	if err := s.UpsertNode(ctx, Node{ID: node1, Name: "one", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	for _, r := range replicasOf(t, s, "web") {
		if r.NodeID != node1 || r.PendingReason != "" {
			t.Fatalf("after node registered: %+v", r)
		}
	}
	as, err := s.AssignmentsForNode(ctx, node1)
	if err != nil || len(as) != 2 || as[0].Spec.Image != "docker.io/library/nginx:alpine" && as[0].Spec.Image != "nginx:alpine" {
		t.Fatalf("assignments: %+v %v", as, err)
	}
	n, err := s.GetNode(ctx, node1)
	if err != nil || n.CPU != 2 || n.LastLease.IsZero() {
		t.Fatalf("GetNode: %+v %v", n, err)
	}
	if _, err := s.GetNode(ctx, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetNode(other): %v", err)
	}
}

func TestPlacementFunction(t *testing.T) {
	var calls int
	place := func(nodes []Node, placed []Assignment, app spec.App, replica int, _ time.Time) (string, error) {
		calls++
		if replica == 1 {
			return "", errors.New("no node has port 8080 free")
		}
		if replica == 2 {
			return "ghost", nil
		}
		return nodes[len(nodes)-1].ID, nil
	}
	s := newT(t, WithPlacement(place))
	if err := s.UpsertNode(ctxT(t), Node{ID: "node-2", Name: "two"}); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, web(3))
	r := replicasOf(t, s, "web")
	if r[0].NodeID != "node-2" {
		t.Errorf("replica 0 on %q, want node-2", r[0].NodeID)
	}
	if r[1].NodeID != "" || r[1].PendingReason != "no node has port 8080 free" {
		t.Errorf("replica 1: %+v", r[1])
	}
	if r[2].NodeID != "" || r[2].PendingReason == "" {
		t.Errorf("replica 2 placed on an unknown node: %+v", r[2])
	}
	if calls != 3 {
		t.Errorf("%d placement calls, want 3", calls)
	}
}

// A change that fails part way leaves nothing behind. The placement function
// cancels the context, so the next statement in the transaction fails.
func TestFailedChangeRollsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	place := func(nodes []Node, _ []Assignment, _ spec.App, _ int, _ time.Time) (string, error) {
		cancel()
		return nodes[0].ID, nil
	}
	s := newT(t, WithPlacement(place))
	if _, err := s.PutApp(ctx, web(2)); err == nil {
		t.Fatal("PutApp with a cancelled context succeeded")
	}
	checkInvariants(t, s)
	var apps, reps int
	s.db.QueryRow(`SELECT count(*) FROM apps`).Scan(&apps)
	s.db.QueryRow(`SELECT count(*) FROM replicas`).Scan(&reps)
	if apps != 0 || reps != 0 {
		t.Fatalf("rolled-back PutApp left %d apps and %d replicas", apps, reps)
	}
}

func TestReportStatus(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	mustPut(t, s, web(1))
	before, _ := s.GetNode(ctx, node1)
	st := []Status{{Name: "hivemind-web-0-g1", ContainerID: "abc", App: "web", Replica: 0, Generation: 1, State: "running"}}
	if err := s.ReportStatus(ctx, node1, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListStatus(ctx, "web")
	if err != nil || len(got) != 1 || got[0].NodeID != node1 || got[0].ContainerID != "abc" || got[0].UpdatedAt.IsZero() {
		t.Fatalf("ListStatus: %+v %v", got, err)
	}
	after, _ := s.GetNode(ctx, node1)
	if after.LastLease.Before(before.LastLease) {
		t.Fatal("ReportStatus did not renew the lease")
	}

	// A bad report is refused whole, and the previous one stays.
	dup := []Status{st[0], st[0]}
	if err := s.ReportStatus(ctx, node1, dup); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate names: %v", err)
	}
	if err := s.ReportStatus(ctx, node1, []Status{{}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty name: %v", err)
	}
	if err := s.ReportStatus(ctx, "ghost", st); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
	if got, _ := s.ListStatus(ctx, ""); len(got) != 1 {
		t.Fatalf("status after refused reports: %+v", got)
	}

	// A new report replaces the old one.
	st2 := []Status{{Name: "hivemind-web-0-g1", App: "web", Generation: 1, State: "pending", Error: "pull: not found"}}
	if err := s.ReportStatus(ctx, node1, st2); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListStatus(ctx, "web")
	if len(got) != 1 || got[0].Error != "pull: not found" || got[0].ContainerID != "" {
		t.Fatalf("replaced status: %+v", got)
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hivemind.db")
	ctx := ctxT(t)
	s, err := Open(ctx, path, WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertNode(ctx, Node{ID: node1, Name: "one", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	changed := web(2)
	changed.Env = map[string]string{"A": "2"}
	mustPut(t, s, web(2))
	mustPut(t, s, changed)
	if err := s.ReportStatus(ctx, node1, []Status{{Name: "x", App: "web", Generation: 2, State: "running"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openT(t, path, WithMinReady(0))
	checkInvariants(t, s2)
	a, err := s2.GetApp(ctx, "web")
	if err != nil || a.Generation != 2 || a.Spec.Replicas != 2 || a.Spec.Env["A"] != "2" {
		t.Fatalf("after reopen: %+v %v", a, err)
	}
	if ro, err := s2.GetRollout(ctx, "web"); err != nil || ro.Target != 2 || ro.State != RolloutInProgress {
		t.Fatalf("rollout after reopen: %+v %v", ro, err)
	}
	if as, _ := s2.AssignmentsForNode(ctx, node1); len(as) != 2 || as[1].Generation != 1 {
		t.Fatalf("assignments after reopen: %+v", as)
	}
	if st, _ := s2.ListStatus(ctx, "web"); len(st) != 1 {
		t.Fatalf("status after reopen: %+v", st)
	}
}

func TestConcurrentChanges(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				a := web(1 + (i+w)%4)
				a.Env = map[string]string{"A": fmt.Sprint((i * w) % 3)}
				var err error
				switch i % 5 {
				case 3:
					_, err = s.Scale(ctx, "web", 1+i%3)
				case 4:
					err = s.ReportStatus(ctx, node1, nil)
				default:
					_, err = s.PutApp(ctx, a)
				}
				if err != nil && !errors.Is(err, ErrNotFound) {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	checkInvariants(t, s)
}
