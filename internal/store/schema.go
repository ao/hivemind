package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations lists the schema changes in order. migrations[i] moves the schema
// from version i to version i+1. Released entries are never edited; a change
// to the schema is a new entry at the end.
var migrations = []string{
	// 1: apps, replicas, nodes and reported container status.
	`
CREATE TABLE nodes (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	cpu        INTEGER NOT NULL CHECK (cpu >= 0),
	memory     INTEGER NOT NULL CHECK (memory >= 0),
	last_lease INTEGER NOT NULL
) STRICT;

CREATE TABLE apps (
	name       TEXT PRIMARY KEY,
	spec       TEXT NOT NULL,
	template   TEXT NOT NULL,
	generation INTEGER NOT NULL CHECK (generation >= 1),
	replicas   INTEGER NOT NULL CHECK (replicas >= 1),
	deleted    INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE replicas (
	app            TEXT NOT NULL REFERENCES apps(name) ON DELETE CASCADE,
	idx            INTEGER NOT NULL CHECK (idx >= 0),
	node_id        TEXT REFERENCES nodes(id),
	generation     INTEGER NOT NULL CHECK (generation >= 1),
	desired        TEXT NOT NULL CHECK (desired IN ('running', 'removed')),
	pending_reason TEXT NOT NULL DEFAULT '',
	updated_at     INTEGER NOT NULL,
	PRIMARY KEY (app, idx)
) STRICT;
CREATE INDEX replicas_node ON replicas(node_id);

CREATE TABLE container_status (
	node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	app          TEXT NOT NULL,
	idx          INTEGER NOT NULL,
	generation   INTEGER NOT NULL,
	container_id TEXT NOT NULL,
	state        TEXT NOT NULL,
	error        TEXT NOT NULL,
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (node_id, name)
) STRICT;
CREATE INDEX container_status_app ON container_status(app, idx);
`,
	// 2: health and restart state for each reported container. next_restart is
	// unix nanoseconds, or 0 when no restart is scheduled.
	`
ALTER TABLE container_status ADD COLUMN restart_count INTEGER NOT NULL DEFAULT 0 CHECK (restart_count >= 0);
ALTER TABLE container_status ADD COLUMN health TEXT NOT NULL DEFAULT '';
ALTER TABLE container_status ADD COLUMN ready INTEGER NOT NULL DEFAULT 0 CHECK (ready IN (0, 1));
ALTER TABLE container_status ADD COLUMN crash_looping INTEGER NOT NULL DEFAULT 0 CHECK (crash_looping IN (0, 1));
ALTER TABLE container_status ADD COLUMN next_restart INTEGER NOT NULL DEFAULT 0;
`,
	// 3: the cluster record that init writes: the admin token's SHA-256
	// digest (never the token) and the ID of the local node.
	`
CREATE TABLE cluster (
	id               INTEGER PRIMARY KEY CHECK (id = 1),
	admin_token_hash BLOB NOT NULL CHECK (length(admin_token_hash) = 32),
	node_id          TEXT NOT NULL CHECK (node_id != ''),
	created_at       INTEGER NOT NULL
) STRICT;
`,
	// 4: rolling updates. A surge is the new-generation copy of one replica,
	// on another node, that runs beside the old copy until the store moves
	// the replica there. A rollout records the generation being rolled out,
	// the replica in flight and its state. replicas.generation is the
	// generation each replica should run now, so the rollout moves it one
	// replica at a time.
	`
CREATE TABLE surges (
	app        TEXT NOT NULL REFERENCES apps(name) ON DELETE CASCADE,
	idx        INTEGER NOT NULL CHECK (idx >= 0),
	node_id    TEXT NOT NULL REFERENCES nodes(id),
	generation INTEGER NOT NULL CHECK (generation >= 1),
	desired    TEXT NOT NULL CHECK (desired IN ('running', 'removed')),
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (app, idx)
) STRICT;
CREATE INDEX surges_node ON surges(node_id);

-- The spec each generation runs with. A replica still on an old generation
-- keeps running, and is restarted, from the spec of its own generation.
CREATE TABLE app_specs (
	app        TEXT NOT NULL REFERENCES apps(name) ON DELETE CASCADE,
	generation INTEGER NOT NULL CHECK (generation >= 1),
	spec       TEXT NOT NULL,
	PRIMARY KEY (app, generation)
) STRICT;
INSERT INTO app_specs (app, generation, spec) SELECT name, generation, spec FROM apps;

CREATE TABLE rollouts (
	app        TEXT PRIMARY KEY REFERENCES apps(name) ON DELETE CASCADE,
	target     INTEGER NOT NULL CHECK (target >= 1),
	state      TEXT NOT NULL CHECK (state IN ('in_progress', 'paused', 'done')),
	reason     TEXT NOT NULL DEFAULT '',
	step_idx   INTEGER NOT NULL DEFAULT -1 CHECK (step_idx >= -1),
	step_from  INTEGER NOT NULL DEFAULT 0 CHECK (step_from >= 0),
	step_since INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
) STRICT;
`,
	// 5: join tokens, and the time a node became ready. A token row holds the
	// SHA-256 of the token's secret, its expiry in unix nanoseconds, and when
	// it was used (0 if not). ready_since is when the node last became ready:
	// when it first registered, or when its lease renewed after lapsing. 0
	// means always ready, which is the cluster's own node. Placement waits
	// MinReady after ready_since.
	`
CREATE TABLE join_tokens (
	hash     BLOB PRIMARY KEY CHECK (length(hash) = 32),
	expires  INTEGER NOT NULL,
	used_at  INTEGER NOT NULL DEFAULT 0 CHECK (used_at >= 0)
) STRICT;
CREATE INDEX join_tokens_expires ON join_tokens(expires);

ALTER TABLE nodes ADD COLUMN ready_since INTEGER NOT NULL DEFAULT 0;
`,
	// 6: node removal. removed_nodes is the denylist of node IDs that were
	// removed: the node API refuses their certificates, and no node is
	// registered or joined under one again. replicas.held marks a volume
	// replica whose node was removed: it has no node, and placement leaves it
	// alone, because its data was on that node. A deploy that changes its
	// generation clears the mark.
	`
CREATE TABLE removed_nodes (
	id         TEXT PRIMARY KEY CHECK (id != ''),
	name       TEXT NOT NULL,
	removed_at INTEGER NOT NULL
) STRICT;

ALTER TABLE replicas ADD COLUMN held INTEGER NOT NULL DEFAULT 0 CHECK (held IN (0, 1));
`,
}

// SchemaVersion is the schema version this build writes.
var SchemaVersion = len(migrations)

// migrate brings the schema up to len(list), in one transaction. A database
// whose version is newer than list is refused, because this build does not
// know what the newer schema means.
func migrate(ctx context.Context, db *sql.DB, list []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("creating schema_version: %w", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_version`).Scan(&n); err != nil {
		return err
	}
	version := 0
	switch n {
	case 0:
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
	case 1:
		if err := tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version); err != nil {
			return err
		}
	default:
		return fmt.Errorf("schema_version has %d rows, want 1", n)
	}
	if version > len(list) {
		return fmt.Errorf("%w: the store has schema version %d and this build knows up to %d; use a newer hivemind", ErrNewerSchema, version, len(list))
	}
	for v := version; v < len(list); v++ {
		if _, err := tx.ExecContext(ctx, list[v]); err != nil {
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, len(list)); err != nil {
		return err
	}
	return tx.Commit()
}
