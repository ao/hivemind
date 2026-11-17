package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// seedSchema writes one row of each kind that exists at schema version k.
// The rows use the column set of that version, so a migration that drops or
// renames a column fails here.
func seedSchema(t *testing.T, db *sql.DB, k int) {
	t.Helper()
	specJSON, err := json.Marshal(web(2))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed (version %d): %v", k, err)
		}
	}
	exec(`INSERT INTO nodes (id, name, cpu, memory, last_lease) VALUES ('node-1', 'one', 4, ?, 100)`, int64(8)<<30)
	exec(`INSERT INTO apps (name, spec, template, generation, replicas, deleted, created_at, updated_at)
		VALUES ('web', ?, ?, 1, 2, 0, 100, 100)`, string(specJSON), web(2).Template())
	exec(`INSERT INTO replicas (app, idx, node_id, generation, desired, pending_reason, updated_at)
		VALUES ('web', 0, 'node-1', 1, 'running', '', 100), ('web', 1, 'node-1', 1, 'running', '', 100)`)
	exec(`INSERT INTO container_status (node_id, name, app, idx, generation, container_id, state, error, updated_at)
		VALUES ('node-1', 'web-0', 'web', 0, 1, 'c0', 'running', '', 100)`)
	if k >= 2 {
		exec(`UPDATE container_status SET restart_count = 3, health = 'healthy', ready = 1 WHERE name = 'web-0'`)
	}
	if k >= 3 {
		exec(`INSERT INTO cluster (id, admin_token_hash, node_id, created_at)
			VALUES (1, zeroblob(32), 'node-1', 100)`)
	}
	if k >= 4 {
		// From version 4 on, every write of an app also writes its spec for
		// the generation, as PutApp does.
		exec(`INSERT INTO app_specs (app, generation, spec) VALUES ('web', 1, ?)`, string(specJSON))
	}
	if k >= 9 && k < 15 {
		// Versions 9 to 14 give a route host to one app in app_hosts;
		// version 15 moves it to app_routes.
		exec(`INSERT INTO app_hosts (host, app, port) VALUES ('www.example.com', 'web', 80)`)
	}
	if k >= 15 {
		// From version 15 on, the route is a whole host route in
		// app_routes.
		exec(`INSERT INTO app_routes (host, path, app, port) VALUES ('www.example.com', '', 'web', 80)`)
	}
}

// TestOpenMigratesStoresFromEarlierSchemas builds a store at schema version
// 1 to SchemaVersion-1 with the migrations of that version, seeds it, and opens it with
// this build. Every row must survive, the new schema must be complete, and
// the version must be the current one.
func TestOpenMigratesStoresFromEarlierSchemas(t *testing.T) {
	for k := 1; k < SchemaVersion; k++ {
		t.Run(fmt.Sprintf("from-%d", k), func(t *testing.T) {
			ctx := ctxT(t)
			path := filepath.Join(t.TempDir(), "hivemind.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if err := migrate(ctx, db, migrations[:k]); err != nil {
				t.Fatalf("migrate to %d: %v", k, err)
			}
			seedSchema(t, db, k)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			s := openT(t, path)
			checkInvariants(t, s)

			var version int
			if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != SchemaVersion {
				t.Fatalf("version %d after migration, want %d", version, SchemaVersion)
			}

			// Rows from the old schema are unchanged.
			app, err := s.GetApp(ctx, "web")
			if err != nil {
				t.Fatalf("App: %v", err)
			}
			if app.Generation != 1 || app.Spec.Replicas != 2 || app.Spec.Image != "nginx:alpine" || app.Origin != "" {
				t.Fatalf("app after migration: generation %d, %+v", app.Generation, app.Spec)
			}
			if reps := replicasOf(t, s, "web"); len(reps) != 2 {
				t.Fatalf("replicas after migration: %d, want 2", len(reps))
			}

			// Migration 4 copies each app's spec into app_specs.
			var specs int
			if err := s.db.QueryRow(`SELECT count(*) FROM app_specs WHERE app = 'web' AND generation = 1`).Scan(&specs); err != nil {
				t.Fatal(err)
			}
			if specs != 1 {
				t.Fatalf("app_specs rows for web generation 1: %d, want 1", specs)
			}

			// Columns added in migration 2 take their defaults for rows
			// written before them, and keep the values written after.
			var restarts int
			var health string
			if err := s.db.QueryRow(`SELECT restart_count, health FROM container_status WHERE name = 'web-0'`).Scan(&restarts, &health); err != nil {
				t.Fatal(err)
			}
			wantRestarts, wantHealth := 0, ""
			if k >= 2 {
				wantRestarts, wantHealth = 3, "healthy"
			}
			if restarts != wantRestarts || health != wantHealth {
				t.Fatalf("container_status restart_count %d, health %q; want %d, %q", restarts, health, wantRestarts, wantHealth)
			}

			// Migration 3 adds the cluster record; a store from version 1 or 2
			// has none, and init has not run. Migration 16 puts a record
			// from before it at epoch 0.
			cl, err := s.GetCluster(ctx)
			if k < 3 {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("GetCluster before the cluster table had rows: %v, %v", cl, err)
				}
			} else if err != nil || cl.NodeID != "node-1" || cl.Epoch != 0 {
				t.Fatalf("GetCluster: %+v, %v", cl, err)
			}

			// Migration 6 adds an empty denylist, and no replica is held.
			if removed, err := s.IsNodeRemoved(ctx, "node-1"); err != nil || removed {
				t.Fatalf("IsNodeRemoved after migration: %v, %v", removed, err)
			}
			var held int
			if err := s.db.QueryRow(`SELECT count(*) FROM replicas WHERE held != 0`).Scan(&held); err != nil || held != 0 {
				t.Fatalf("held replicas after migration: %d, %v", held, err)
			}

			// Migration 8 leaves every node eligible, with no drain.
			if n, err := s.GetNode(ctx, "node-1"); err != nil || !n.Eligible {
				t.Fatalf("node after migration: %+v, %v", n, err)
			}
			if ds, err := s.ListDrains(ctx); err != nil || len(ds) != 0 {
				t.Fatalf("drains after migration: %+v, %v", ds, err)
			}

			// Migration 9 gives nodes no address, and migration 15 makes
			// each route host of versions 9 to 14 a whole host route.
			if n, _ := s.GetNode(ctx, "node-1"); n.Address != "" {
				t.Fatalf("node address after migration: %q", n.Address)
			}
			rt, err := s.RouteTable(ctx)
			switch {
			case err != nil:
				t.Fatal(err)
			case k >= 9 && (len(rt) != 1 || rt[0].Host != "www.example.com" || rt[0].Path != "" || rt[0].App != "web" || rt[0].Port != 80):
				t.Fatalf("route table after migration from %d: %+v", k, rt)
			case k < 9 && len(rt) != 0:
				t.Fatalf("route table after migration from %d: %+v", k, rt)
			}
			if sts, err := s.ListStatus(ctx, ""); err != nil || len(sts) == 0 || sts[0].RoutePorts != nil {
				t.Fatalf("status after migration: %+v, %v", sts, err)
			}

			// Migration 10 adds no registry credential, and the replicas
			// get none.
			if rs, err := s.ListRegistries(ctx); err != nil || len(rs) != 0 {
				t.Fatalf("registries after migration: %+v, %v", rs, err)
			}
			if as, err := s.AssignmentsForNode(ctx, "node-1"); err != nil || len(as) != 2 || as[0].Auth != nil {
				t.Fatalf("assignments after migration: %+v, %v", as, err)
			}

			// The migrated store takes writes that use the new schema.
			mustPut(t, s, web(3))
			if _, err := s.PutRegistry(ctx, "docker.io", "u", "p"); err != nil {
				t.Fatalf("registry credential after migration: %v", err)
			}
			if as, _ := s.AssignmentsForNode(ctx, "node-1"); len(as) < 2 || as[0].Auth == nil || as[0].Auth.Username != "u" {
				t.Fatalf("assignments after a login: %+v", as)
			}
			if app, _ := s.GetApp(ctx, "web"); app.Spec.Replicas != 3 {
				t.Fatalf("replicas after a write: %d, want 3", app.Spec.Replicas)
			}
		})
	}
}
