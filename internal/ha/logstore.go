package ha

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/raft"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// logStore is Raft's log store and stable store, in one SQLite file of its
// own (docs/ha.md): raft.db in the Raft directory, in WAL mode with
// synchronous=FULL, as the store is. The log is a table keyed by index;
// the stable store, the current term and the vote, is a table of keys.
type logStore struct {
	db  *sql.DB // the one write connection
	rdb *sql.DB // reads
}

var (
	_ raft.LogStore          = (*logStore)(nil)
	_ raft.StableStore       = (*logStore)(nil)
	_ raft.MonotonicLogStore = (*logStore)(nil)
)

const logSchema = `
CREATE TABLE IF NOT EXISTS logs (
	idx         INTEGER PRIMARY KEY CHECK (idx >= 1),
	term        INTEGER NOT NULL,
	type        INTEGER NOT NULL,
	data        BLOB,
	extensions  BLOB,
	appended_at INTEGER NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS stable (
	key   TEXT PRIMARY KEY,
	value BLOB NOT NULL
) STRICT;
`

// logOpTimeout bounds one operation: raft's interfaces have no context.
const logOpTimeout = 30 * time.Second

// openLogStore opens or creates the log store at path, mode 0600.
func openLogStore(path string) (*logStore, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("raft log path %q has ? or #", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), logOpTimeout)
	defer cancel()
	if _, err := db.ExecContext(ctx, logSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("raft log %s: %w", path, err)
	}
	rdb, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=query_only(1)")
	if err != nil {
		db.Close()
		return nil, err
	}
	rdb.SetMaxOpenConns(4)
	return &logStore{db: db, rdb: rdb}, nil
}

func (s *logStore) Close() error { return errors.Join(s.rdb.Close(), s.db.Close()) }

func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), logOpTimeout)
}

// IsMonotonic says the log has no gaps: after a snapshot is restored, raft
// deletes the log rather than leave a gap before the next entry.
func (s *logStore) IsMonotonic() bool { return true }

func (s *logStore) FirstIndex() (uint64, error) {
	ctx, cancel := opCtx()
	defer cancel()
	var v sql.NullInt64
	err := s.rdb.QueryRowContext(ctx, `SELECT min(idx) FROM logs`).Scan(&v)
	return uint64(v.Int64), err
}

func (s *logStore) LastIndex() (uint64, error) {
	ctx, cancel := opCtx()
	defer cancel()
	var v sql.NullInt64
	err := s.rdb.QueryRowContext(ctx, `SELECT max(idx) FROM logs`).Scan(&v)
	return uint64(v.Int64), err
}

func (s *logStore) GetLog(index uint64, l *raft.Log) error {
	ctx, cancel := opCtx()
	defer cancel()
	var term, typ, appended int64
	var data, ext []byte
	err := s.rdb.QueryRowContext(ctx, `SELECT term, type, data, extensions, appended_at FROM logs WHERE idx = ?`, int64(index)).
		Scan(&term, &typ, &data, &ext, &appended)
	if errors.Is(err, sql.ErrNoRows) {
		return raft.ErrLogNotFound
	}
	if err != nil {
		return err
	}
	*l = raft.Log{Index: index, Term: uint64(term), Type: raft.LogType(typ), Data: data, Extensions: ext}
	if appended != 0 {
		l.AppendedAt = time.Unix(0, appended)
	}
	return nil
}

func (s *logStore) StoreLog(l *raft.Log) error { return s.StoreLogs([]*raft.Log{l}) }

func (s *logStore) StoreLogs(logs []*raft.Log) error {
	ctx, cancel := opCtx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO logs (idx, term, type, data, extensions, appended_at) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, l := range logs {
		if l.Index == 0 {
			return errors.New("raft log: entry at index 0")
		}
		var appended int64
		if !l.AppendedAt.IsZero() {
			appended = l.AppendedAt.UnixNano()
		}
		if _, err := ins.ExecContext(ctx, int64(l.Index), int64(l.Term), int64(l.Type), l.Data, l.Extensions, appended); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *logStore) DeleteRange(lo, hi uint64) error {
	ctx, cancel := opCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE idx >= ? AND idx <= ?`, int64(lo), int64(min(hi, 1<<63-1)))
	return err
}

func (s *logStore) Set(key, val []byte) error {
	ctx, cancel := opCtx()
	defer cancel()
	if val == nil {
		val = []byte{}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO stable (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		string(key), val)
	return err
}

// Get returns the value at key, or an empty value for a key never set, as
// raft expects.
func (s *logStore) Get(key []byte) ([]byte, error) {
	ctx, cancel := opCtx()
	defer cancel()
	var v []byte
	err := s.rdb.QueryRowContext(ctx, `SELECT value FROM stable WHERE key = ?`, string(key)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return []byte{}, nil
	}
	if v == nil {
		v = []byte{}
	}
	return v, err
}

func (s *logStore) SetUint64(key []byte, val uint64) error {
	return s.Set(key, binary.BigEndian.AppendUint64(nil, val))
}

func (s *logStore) GetUint64(key []byte) (uint64, error) {
	v, err := s.Get(key)
	if err != nil || len(v) == 0 {
		return 0, err
	}
	if len(v) != 8 {
		return 0, fmt.Errorf("raft stable store: %q holds %d bytes, not a number", key, len(v))
	}
	return binary.BigEndian.Uint64(v), nil
}
