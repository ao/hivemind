package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ao/hivemind/internal/spec"
)

// CheckInvariants checks what must hold in the store after any committed
// change: SQLite's integrity and foreign key checks, the schema version, and
// for each app, that its stored spec is valid and matches its columns, and
// that its running replicas match its replica count and generation. It
// returns the first violation. The tests run it after every change; the
// crash and concurrency tests run it after the fact.
func (s *Store) CheckInvariants(ctx context.Context) error { return invariants(ctx, s.db) }

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
		// A deleted app goes once its last replica and its last surge are
		// gone (purgeDeleted). A surge marked for removal waits for its node
		// to report its container gone, as a replica does.
		var surges int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM surges WHERE app = ?`, a.name).Scan(&surges); err != nil {
			return err
		}
		if a.deleted == 1 && len(reps) == 0 && surges == 0 {
			return fmt.Errorf("app %s: deleted with no replicas or surges but not purged", a.name)
		}
	}
	return nil
}
