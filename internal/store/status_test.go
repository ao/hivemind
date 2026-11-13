package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStatusHealthFieldsRoundTrip(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	mustPut(t, s, web(1))
	next := time.Unix(1700000000, 123)
	st := []Status{{
		Name: "hivemind-web-0-g1", ContainerID: "abc", App: "web", Replica: 0, Generation: 1,
		State: "exited", RestartCount: 4, Health: "unhealthy", Ready: false,
		CrashLooping: true, NextRestart: next,
	}}
	if err := s.ReportStatus(ctx, node1, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListStatus(ctx, "web")
	if err != nil || len(got) != 1 {
		t.Fatalf("ListStatus: %+v %v", got, err)
	}
	x := got[0]
	if x.RestartCount != 4 || x.Health != "unhealthy" || x.Ready || !x.CrashLooping || !x.NextRestart.Equal(next) {
		t.Fatalf("round trip: %+v", x)
	}

	// A status with no restart scheduled stores a zero NextRestart.
	st[0].CrashLooping, st[0].NextRestart, st[0].Ready, st[0].Health, st[0].State = false, time.Time{}, true, "healthy", "running"
	if err := s.ReportStatus(ctx, node1, st); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListStatus(ctx, "web")
	if x := got[0]; !x.NextRestart.IsZero() || !x.Ready || x.CrashLooping || x.Health != "healthy" {
		t.Fatalf("after recovery: %+v", x)
	}

	// StatusForNode returns the same rows for the node, and none for another.
	byNode, err := s.StatusForNode(ctx, node1)
	if err != nil || len(byNode) != 1 || byNode[0].RestartCount != 4 {
		t.Fatalf("StatusForNode: %+v %v", byNode, err)
	}
	if other, _ := s.StatusForNode(ctx, "ghost"); len(other) != 0 {
		t.Fatalf("StatusForNode for another node: %+v", other)
	}
}

func TestStatusRejectsNegativeRestartCount(t *testing.T) {
	s := newT(t)
	mustPut(t, s, web(1))
	err := s.ReportStatus(ctxT(t), node1, []Status{{Name: "hivemind-web-0-g1", App: "web", Generation: 1, RestartCount: -1}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative restart count: %v", err)
	}
}

// A file written by the schema-1 build keeps its rows through migration 2,
// and the new columns take their defaults.
func TestMigrationTwoKeepsSchemaOneStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := ctxT(t)
	if err := migrate(ctx, db, migrations[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (id, name, cpu, memory, last_lease) VALUES ('n1', 'one', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO container_status (node_id, name, container_id, app, idx, generation, state, error, updated_at)
		VALUES ('n1', 'hivemind-web-0-g1', 'c1', 'web', 0, 1, 'running', '', 5)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations); err != nil {
		t.Fatalf("migration 2: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("version %d: %v", version, err)
	}
	var state string
	var restarts, ready, crash, next int
	var health string
	err = db.QueryRow(`SELECT state, restart_count, health, ready, crash_looping, next_restart FROM container_status WHERE name = 'hivemind-web-0-g1'`).
		Scan(&state, &restarts, &health, &ready, &crash, &next)
	if err != nil || state != "running" || restarts != 0 || health != "" || ready != 0 || crash != 0 || next != 0 {
		t.Fatalf("migrated row: state=%q restarts=%d health=%q ready=%d crash=%d next=%d err=%v",
			state, restarts, health, ready, crash, next, err)
	}
}
