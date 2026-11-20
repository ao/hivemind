package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Replication (docs/ha.md). With several servers, every write goes through
// the Raft log: the leader runs each transaction as it always has, with a
// recorder on the write connection, rolls it back, and appends the write
// statements it recorded to the log as one batch. Every server, the leader
// included, applies a committed batch by running its statements in one
// transaction (ApplyEntry). A server that is not the leader writes nothing
// itself: its transactions fail with ErrNotLeader before they start.

// Errors of a replicated store.
var (
	// ErrNotLeader means this server cannot write now: another server
	// leads the cluster, or none does. Nothing was written.
	ErrNotLeader = errors.New("this server is not the leader of the cluster")
	// ErrOutcomeUnknown means the write went into the log, and the leader
	// lost the leadership before it was applied: the next leader may apply
	// it, or not.
	ErrOutcomeUnknown = errors.New("the leader changed while the write was in the log, so it may or may not have been applied")
	// ErrOtherSchema means a batch or a snapshot is from a store at another
	// schema version, which this build will not apply.
	ErrOtherSchema = errors.New("the batch is from another schema version")
)

// Replicator is the log a replicated store writes through.
type Replicator interface {
	// CanWrite reports whether this server may write: it is the leader,
	// and has applied every entry of the terms before its own. It returns
	// the leadership the answer is for, which Replicate checks again, or
	// an error wrapping ErrNotLeader.
	CanWrite() (term uint64, err error)
	// Replicate appends one encoded batch to the log, and returns once
	// this store has applied it (ApplyEntry). It refuses, with an error
	// wrapping ErrNotLeader, when the leadership is no longer term, and
	// then nothing was appended; any other error may leave the batch in
	// the log (ErrOutcomeUnknown).
	Replicate(ctx context.Context, term uint64, batch []byte) error
}

type replicator struct{ Replicator }

// SetReplicator makes every later write go through r. It waits for the
// transaction in progress, if any. There is no way back: a store that has
// been in a Raft cluster stays in it.
func (s *Store) SetReplicator(r Replicator) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.repl.Store(&replicator{r})
}

// Replicated reports whether writes go through a log.
func (s *Store) Replicated() bool { return s.repl.Load() != nil }

// EnableReplication runs handover while no transaction runs, and then,
// if it succeeds, makes every later write go through the replicator it
// returns, as SetReplicator does. Turning replication on in a running
// cluster of one uses it: the store handover gives Raft as the log's first
// snapshot is then the store every later write starts from.
func (s *Store) EnableReplication(handover func() (Replicator, error)) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.repl.Load() != nil {
		return errors.New("store: replication is on already")
	}
	r, err := handover()
	if err != nil {
		return err
	}
	s.repl.Store(&replicator{r})
	return nil
}

// replicatedTx is tx for a replicated store. The caller holds writeMu. A
// transaction that writes nothing, a read, runs on any server, on its copy
// of the store. One that writes needs the leadership from before it
// started, so that what it read is the state its batch is applied to.
func (s *Store) replicatedTx(ctx context.Context, r *replicator, fn func(*sql.Tx) error) error {
	term, werr := r.CanWrite()
	rec, c, err := s.dryRun(ctx, fn)
	if err != nil {
		return err
	}
	if len(rec.stmts) > 0 {
		if werr != nil {
			return werr
		}
		seq := s.seq.Add(1)
		data, err := EncodeBatch(Batch{Schema: SchemaVersion, Origin: s.origin, Seq: seq, Stmts: rec.stmts})
		if err != nil {
			return err
		}
		s.awaiting.Store(seq)
		err = r.Replicate(ctx, term, data)
		s.awaiting.Store(0)
		if err != nil {
			// The batch may be applied later, under another leader, when no
			// one waits for it: what this process keeps in memory may then
			// be out of date.
			if !errors.Is(err, ErrNotLeader) {
				s.dropCaches()
			}
			return err
		}
		if rec.changed && !c.routesKept {
			s.routesChanged()
		}
	}
	for _, f := range c.onCommit {
		f()
	}
	return nil
}

// dryRun runs fn in a transaction on the write connection with a recorder
// on it, and rolls it back. It returns the statements recorded and the
// transaction's cache, with what fn asked to run after its commit. The
// connection is back in its pool when it returns, for ApplyEntry.
func (s *Store) dryRun(ctx context.Context, fn func(*sql.Tx) error) (*recorder, *txCache, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	rec := &recorder{}
	if err := setRecorder(conn, rec); err != nil {
		return nil, nil, err
	}
	defer func() { _ = setRecorder(conn, nil) }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	c := &txCache{}
	txCaches.Store(tx, c)
	defer txCaches.Delete(tx)
	err = fn(tx)
	rerr := tx.Rollback()
	if err != nil {
		return nil, nil, err
	}
	if rec.err != nil {
		return nil, nil, rec.err
	}
	if rerr != nil {
		return nil, nil, rerr
	}
	return rec, c, nil
}

// newOrigin names a process in the batches it records.
func newOrigin() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ApplyEntry applies the batch of the log entry at index: its statements,
// in one transaction with the store's applied index. An entry at or below
// the applied index was applied before, as on a restart, which replays the
// entries after the last snapshot, and is skipped. A batch from another
// schema version is ErrOtherSchema, and nothing is applied. Only the Raft
// FSM calls it, one entry at a time.
func (s *Store) ApplyEntry(ctx context.Context, index uint64, data []byte) error {
	b, err := DecodeBatch(data)
	if err != nil {
		return err
	}
	if b.Schema != SchemaVersion {
		return fmt.Errorf("%w: entry %d was written at schema version %d, and this build is at %d; run the same hivemind on every server",
			ErrOtherSchema, index, b.Schema, SchemaVersion)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var applied int64
	if err := tx.QueryRowContext(ctx, `SELECT idx FROM raft_applied WHERE id = 1`).Scan(&applied); err != nil {
		return fmt.Errorf("reading the applied index: %w", err)
	}
	if index <= uint64(applied) {
		return nil
	}
	for i, st := range b.Stmts {
		if _, err := tx.ExecContext(ctx, st.SQL, st.Args...); err != nil {
			return fmt.Errorf("entry %d, statement %d of %d (%s): %w", index, i+1, len(b.Stmts), firstSQLLine(st.SQL), err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE raft_applied SET idx = ? WHERE id = 1`, int64(index)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if b.Origin != s.origin || b.Seq == 0 || b.Seq != s.awaiting.Load() {
		s.dropCaches()
	}
	return nil
}

// AppliedIndex is the index of the last log entry the store applied.
func (s *Store) AppliedIndex(ctx context.Context) (uint64, error) {
	var idx int64
	err := s.rdb.QueryRowContext(ctx, `SELECT idx FROM raft_applied WHERE id = 1`).Scan(&idx)
	return uint64(idx), err
}

// ResetAppliedIndex sets the applied index to 0, for a store that starts a
// Raft log of its own: its entries begin at 1. It writes the store
// directly, so it is for a store that does not replicate yet.
func (s *Store) ResetAppliedIndex(ctx context.Context) error {
	if s.Replicated() {
		return errors.New("store: the applied index of a replicated store is the log's")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		keepsRoutes(tx)
		_, err := tx.ExecContext(ctx, `UPDATE raft_applied SET idx = 0 WHERE id = 1`)
		return err
	})
}

// RaiseAppliedIndex raises the applied index to at least idx, for a store
// that has just loaded the Raft snapshot at idx before Raft starts. It
// writes the store directly, so it is for a store that does not replicate
// yet.
func (s *Store) RaiseAppliedIndex(ctx context.Context, idx uint64) error {
	if s.Replicated() {
		return errors.New("store: the applied index of a replicated store is the log's")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		keepsRoutes(tx)
		_, err := tx.ExecContext(ctx, `UPDATE raft_applied SET idx = max(idx, ?) WHERE id = 1`, int64(idx))
		return err
	})
}

// LoadSnapshot replaces every row of the store with those of the store
// file at path, a snapshot SnapshotTo wrote, in one transaction: the Raft
// FSM's restore. The file must be at this build's schema version.
func (s *Store) LoadSnapshot(ctx context.Context, path string) error {
	info, err := InspectFile(ctx, path)
	if err != nil {
		return err
	}
	if info.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: the snapshot is at schema version %d, and this build is at %d; run the same hivemind on every server",
			ErrOtherSchema, info.SchemaVersion, SchemaVersion)
	}
	if strings.ContainsAny(path, "?#") {
		return fmt.Errorf("%w: snapshot path %q has ? or #", ErrInvalid, path)
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS snap`, path); err != nil {
		return fmt.Errorf("attaching the snapshot: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `DETACH DATABASE snap`) }()
	tables, err := tableNames(ctx, conn, "main")
	if err != nil {
		return err
	}
	snapTables, err := tableNames(ctx, conn, "snap")
	if err != nil {
		return err
	}
	if strings.Join(tables, ",") != strings.Join(snapTables, ",") {
		return fmt.Errorf("%w: the snapshot has the tables %v, and this store %v", ErrOtherSchema, snapTables, tables)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM main.`+quoteIdent(t)); err != nil {
			return fmt.Errorf("emptying %s: %w", t, err)
		}
	}
	for _, t := range tables {
		cols, err := columnNames(ctx, tx, "main", t)
		if err != nil {
			return err
		}
		snapCols, err := columnNames(ctx, tx, "snap", t)
		if err != nil {
			return err
		}
		if strings.Join(cols, ",") != strings.Join(snapCols, ",") {
			return fmt.Errorf("%w: table %s has the columns %v in the snapshot, and %v here", ErrOtherSchema, t, snapCols, cols)
		}
		list := make([]string, len(cols))
		for i, c := range cols {
			list[i] = quoteIdent(c)
		}
		cl := strings.Join(list, ", ")
		if _, err := tx.ExecContext(ctx, `INSERT INTO main.`+quoteIdent(t)+` (`+cl+`) SELECT `+cl+` FROM snap.`+quoteIdent(t)); err != nil {
			return fmt.Errorf("filling %s: %w", t, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.dropCaches()
	return nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// tableNames lists the tables of schema, sorted, without SQLite's own and
// without schema_version, which a snapshot at the same version has the
// same.
func tableNames(ctx context.Context, q queryer, schema string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM `+schema+`.sqlite_schema WHERE type = 'table'
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name != 'schema_version' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// columnNames lists the columns of table in schema, in their order.
func columnNames(ctx context.Context, q queryer, schema, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?, ?) ORDER BY cid`, table, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("table %s.%s has no columns", schema, table)
	}
	return out, nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// dropCaches forgets what this process keeps in memory of the store's
// rows, after rows changed that it did not write: an entry from another
// server, a snapshot, or a write whose outcome is not known. Each cache
// is read again, or rebuilt, from the store.
func (s *Store) dropCaches() {
	s.cacheGen.Add(1)
	s.routesChanged()
	s.leaseMu.Lock()
	s.leases = nil
	s.leaseMu.Unlock()
	s.forgetRegistryBytes()
	s.placeMu.Lock()
	s.placeSet = false
	s.placeMu.Unlock()
	// The denylist only grows, and an entry that removes a node adds to
	// it: a failed read leaves the old list, and the next drop reads again.
	if err := s.reloadRemoved(context.Background()); err != nil && s.log != nil {
		s.log.Warn("reading the removed nodes again", "err", err)
	}
}

// reloadRemoved reads the denylist from the read pool, which sees every
// commit.
func (s *Store) reloadRemoved(ctx context.Context) error {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id FROM removed_nodes`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		m[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.removedMu.Lock()
	s.removed = m
	s.removedMu.Unlock()
	return nil
}

// TookLeadership is what a server calls once it leads, after it has
// applied every entry before its term: it drops the caches, as the rows
// may have changed under another leader since this process last led.
func (s *Store) TookLeadership() { s.dropCaches() }

// checkReportGen drops the digests of the nodes' last reports and the
// times their rows were last rewritten when the caches were dropped since
// they were taken. The caller holds reportMu.
func (s *Store) checkReportGen() {
	if g := s.cacheGen.Load(); g != s.reportGen {
		s.reported = map[string][32]byte{}
		s.rewrote = map[string]int64{}
		s.reportGen = g
	}
}

// SetLocalNode names the node of the server this process runs, when it is
// not the cluster record's: on a server that joined a cluster of several.
// The start grace leaves it out of the nodes that wait, and the routing
// table leaves out its reports from before the start (SetStarted).
func (s *Store) SetLocalNode(id string) { s.localNode.Store(&id) }

// localNodeID is the node of this process's server: SetLocalNode's, or the
// cluster record's, or "" before init.
func (s *Store) localNodeID(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (string, error) {
	if p := s.localNode.Load(); p != nil {
		return *p, nil
	}
	var id string
	err := q.QueryRowContext(ctx, `SELECT node_id FROM cluster WHERE id = 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
