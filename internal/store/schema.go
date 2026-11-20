package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
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
	// 7: node certificate expiry. The server records the expiry of each
	// certificate it issues, at join, at renewal and for its own listener,
	// and of a certificate a node presents that is newer than the record.
	// It is not a foreign key to nodes: a node joins before it registers.
	`
CREATE TABLE node_certs (
	id        TEXT PRIMARY KEY CHECK (id != ''),
	not_after INTEGER NOT NULL,
	issued_at INTEGER NOT NULL
) STRICT;
`,
	// 8: node eligibility and drains. nodes.eligible is 0 for a node that
	// takes no new replicas; the ones it runs stay. A drains row is the drain
	// of one node: its deadline, whether it is still draining, and how many
	// replicas it moved, and stopped at the deadline. drain_moves holds the
	// move in flight for each app with a replica on the node: a surge of that
	// replica, at the replica's own generation, on another node. drain_blocked
	// holds the reason of each replica on the node that could not move at the
	// drain's last step. failed marks one whose move was tried and failed; it
	// is not tried again before the deadline.
	`
ALTER TABLE nodes ADD COLUMN eligible INTEGER NOT NULL DEFAULT 1 CHECK (eligible IN (0, 1));

CREATE TABLE drains (
	node_id     TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
	state       TEXT NOT NULL CHECK (state IN ('draining', 'done')),
	started_at  INTEGER NOT NULL,
	deadline    INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	moved       INTEGER NOT NULL DEFAULT 0 CHECK (moved >= 0),
	stopped     INTEGER NOT NULL DEFAULT 0 CHECK (stopped >= 0),
	updated_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE drain_moves (
	node_id    TEXT NOT NULL REFERENCES drains(node_id) ON DELETE CASCADE,
	app        TEXT NOT NULL REFERENCES apps(name) ON DELETE CASCADE,
	idx        INTEGER NOT NULL CHECK (idx >= 0),
	surge_node TEXT NOT NULL,
	generation INTEGER NOT NULL CHECK (generation >= 1),
	since      INTEGER NOT NULL,
	PRIMARY KEY (node_id, app)
) STRICT;

CREATE TABLE drain_blocked (
	node_id TEXT NOT NULL REFERENCES drains(node_id) ON DELETE CASCADE,
	app     TEXT NOT NULL REFERENCES apps(name) ON DELETE CASCADE,
	idx     INTEGER NOT NULL CHECK (idx >= 0),
	reason  TEXT NOT NULL,
	failed  INTEGER NOT NULL DEFAULT 0 CHECK (failed IN (0, 1)),
	PRIMARY KEY (node_id, app, idx)
) STRICT;
`,
	// 9: routing. nodes.address is the node's private address, where routed
	// containers publish their ports; '' for a node that reported none (a
	// worker older than 2.2). container_status.route_ports is the JSON list
	// of the container ports a routed container publishes and the host port
	// each got, or ''. app_hosts gives each route host to one app; the
	// primary key is what makes a host belong to one app. tls_storage and
	// tls_locks hold the routers' shared certificate and ACME state, and
	// their locks: certmagic's Storage, backed by the store. A lock row is
	// held by owner until expires (unix nanoseconds); an expired one is
	// stale and may be taken.
	`
ALTER TABLE nodes ADD COLUMN address TEXT NOT NULL DEFAULT '';
ALTER TABLE container_status ADD COLUMN route_ports TEXT NOT NULL DEFAULT '';

CREATE TABLE app_hosts (
	host TEXT PRIMARY KEY CHECK (host != ''),
	app  TEXT NOT NULL REFERENCES apps(name) ON DELETE CASCADE,
	port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535)
) STRICT;
CREATE INDEX app_hosts_app ON app_hosts(app);

CREATE TABLE tls_storage (
	key      TEXT PRIMARY KEY CHECK (key != ''),
	value    BLOB NOT NULL,
	modified INTEGER NOT NULL
) STRICT;

CREATE TABLE tls_locks (
	name    TEXT PRIMARY KEY CHECK (name != ''),
	owner   TEXT NOT NULL CHECK (owner != ''),
	expires INTEGER NOT NULL
) STRICT;
`,
	// 10: registry credentials. A row is the username and password that a
	// pull of an image from host sends. The password is stored as it is,
	// like environment values, because the Docker daemon needs it as it is.
	// host is in the form spec.ParseRegistryHost gives.
	`
CREATE TABLE registries (
	host       TEXT PRIMARY KEY CHECK (host != ''),
	username   TEXT NOT NULL CHECK (username != ''),
	password   TEXT NOT NULL CHECK (password != ''),
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
) STRICT;
`,
	// 11: node loss during a drain. drains.lost counts the replicas that the
	// node gave up when it was lost while it drained, to be placed again, and
	// lost_at is when it was first lost then, or 0.
	`
ALTER TABLE drains ADD COLUMN lost INTEGER NOT NULL DEFAULT 0 CHECK (lost >= 0);
ALTER TABLE drains ADD COLUMN lost_at INTEGER NOT NULL DEFAULT 0;
`,
	// 12: app origins. apps.origin names what deployed the app, such as
	// "compose:shop" for a compose project, or '' for hivemind deploy and the
	// API. A put that names an origin refuses an app with another one.
	`
ALTER TABLE apps ADD COLUMN origin TEXT NOT NULL DEFAULT '';
`,
	// 13: node versions. nodes.version is the hivemind version the node
	// last sent with a request, such as v2.2.0; '' for a node that sent
	// none, a build older than 2.2.
	`
ALTER TABLE nodes ADD COLUMN version TEXT NOT NULL DEFAULT '';
`,
	// 14: when a rollout's copy appeared. rollouts.step_seen is when the
	// store first saw a container of the move in flight reported by its
	// node, or 0 before that; the move's deadline counts from it.
	`
ALTER TABLE rollouts ADD COLUMN step_seen INTEGER NOT NULL DEFAULT 0;
`,
	// 15: path routes. app_routes replaces app_hosts: a route is a host and
	// a path prefix, '' for the whole host, and the pair is the primary key,
	// so one host can have routes in several apps, one app for each path.
	// Every row of app_hosts becomes a whole host route.
	`
CREATE TABLE app_routes (
	host TEXT NOT NULL CHECK (host != ''),
	path TEXT NOT NULL CHECK (path = '' OR (substr(path, 1, 1) = '/' AND path != '/')),
	app  TEXT NOT NULL REFERENCES apps(name) ON DELETE CASCADE,
	port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
	PRIMARY KEY (host, path)
) STRICT;
CREATE INDEX app_routes_app ON app_routes(app);
INSERT INTO app_routes (host, path, app, port) SELECT host, '', app, port FROM app_hosts;
DROP TABLE app_hosts;
`,
	// 16: the cluster's epoch, and what each node dials. cluster.epoch is
	// 0 when init writes the record, and a store from before this version
	// has 0 too; restore makes it one higher than the backup's. The server
	// sends it on every node API reply, and a worker refuses a server whose
	// epoch is lower than one it has seen: a copy of the cluster from
	// before a restore. nodes.dials is the server address, host:port, that
	// a worker registered from its node.json; '' for the server's own node
	// and for a worker that sent none, a build from before this version.
	`
ALTER TABLE cluster ADD COLUMN epoch INTEGER NOT NULL DEFAULT 0 CHECK (epoch >= 0);
ALTER TABLE nodes ADD COLUMN dials TEXT NOT NULL DEFAULT '';
`,
	// 17: cluster CA rotation (docs/ca-rotation.md). ca_rotation is the
	// rotation in progress, if any: the fingerprints of the CA in ca.crt
	// (current) and of the one in ca-next.crt (next), the phase, the CA it
	// goes towards (next, or current after an abort in the renew phase),
	// and the CA that signs the server's own certificate. 'ending' records
	// that the rotation's last step, which changes the CA files, has
	// begun. ca_rotation_nodes holds, for each node seen since the
	// rotation started or turned round, whether it trusts the CA the
	// rotation goes towards, whether it has presented a certificate from
	// it, and whether its requests say nothing of what it trusts (a build
	// without rotation).
	`
CREATE TABLE ca_rotation (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	current_fp BLOB NOT NULL CHECK (length(current_fp) = 32),
	next_fp    BLOB NOT NULL CHECK (length(next_fp) = 32 AND next_fp != current_fp),
	phase      TEXT NOT NULL CHECK (phase IN ('trust', 'renew', 'ending')),
	toward     TEXT NOT NULL CHECK (toward IN ('next', 'current')),
	listener   TEXT NOT NULL CHECK (listener IN ('current', 'next')),
	started_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE ca_rotation_nodes (
	node_id   TEXT PRIMARY KEY CHECK (node_id != ''),
	trusts    INTEGER NOT NULL DEFAULT 0 CHECK (trusts IN (0, 1)),
	renewed   INTEGER NOT NULL DEFAULT 0 CHECK (renewed IN (0, 1)),
	old_build INTEGER NOT NULL DEFAULT 0 CHECK (old_build IN (0, 1)),
	seen_at   INTEGER NOT NULL
) STRICT;
`,
	// 18: several servers (docs/ha.md). raft_applied is the index of the
	// last Raft log entry this store applied, written in the entry's own
	// transaction, so a restart applies none twice; 0 for a store that has
	// not been in a Raft cluster. servers lists the servers of a cluster
	// with more than one: each one's node ID, the address it advertises
	// and its node listener's port, which workers dial, and its Raft
	// address; it is empty for a cluster of one. join_tokens.role tells a
	// server's join token from a worker's.
	`
CREATE TABLE raft_applied (
	id  INTEGER PRIMARY KEY CHECK (id = 1),
	idx INTEGER NOT NULL CHECK (idx >= 0)
) STRICT;
INSERT INTO raft_applied (id, idx) VALUES (1, 0);

CREATE TABLE servers (
	id        TEXT PRIMARY KEY CHECK (id != ''),
	advertise TEXT NOT NULL CHECK (advertise != ''),
	node_port INTEGER NOT NULL CHECK (node_port BETWEEN 1 AND 65533),
	raft_addr TEXT NOT NULL CHECK (raft_addr != ''),
	added_at  INTEGER NOT NULL
) STRICT;

ALTER TABLE join_tokens ADD COLUMN role TEXT NOT NULL DEFAULT 'worker' CHECK (role IN ('worker', 'server'));
`,
}

// migrationNotes says in a few words what each migration adds, for the log:
// migrationNotes[i] is migrations[i]'s.
var migrationNotes = []string{
	"apps, replicas, nodes and container status",
	"container health and restarts",
	"the cluster record",
	"rolling updates",
	"join tokens and node readiness",
	"node removal",
	"node certificate expiry",
	"node eligibility and drains",
	"routing",
	"registry credentials",
	"node loss during a drain",
	"app origins",
	"node versions",
	"rollout copy times",
	"path routes",
	"the cluster epoch and the server address each node dials",
	"cluster CA rotation",
	"several servers",
}

// SchemaVersion is the schema version this build writes.
var SchemaVersion = len(migrations)

// migrate brings the schema up to len(list), in one transaction. A database
// whose version is newer than list is refused, because this build does not
// know what the newer schema means.
func migrate(ctx context.Context, db *sql.DB, list []string) error {
	return migrateLogged(ctx, db, list, nil)
}

// migrateLogged is migrate, and once the transaction commits it logs one
// line to log for each step it took, unless the store was new. Nil logs
// nothing.
func migrateLogged(ctx context.Context, db *sql.DB, list []string, log *slog.Logger) error {
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
	if err := tx.Commit(); err != nil {
		return err
	}
	if log != nil && n == 1 {
		for v := version + 1; v <= len(list); v++ {
			note := ""
			if v <= len(migrationNotes) {
				note = migrationNotes[v-1]
			}
			log.Info("store schema migrated", "from", v-1, "to", v, "adds", note)
		}
	}
	return nil
}
