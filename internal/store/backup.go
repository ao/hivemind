package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Snapshots use VACUUM INTO, which SQLite has had since 3.27.0; the
// modernc.org/sqlite in go.mod bundles 3.54.1. VACUUM INTO reads the database
// in one read transaction, so the file it writes is a consistent snapshot
// (https://sqlite.org/lang_vacuum.html#vacuuminto). In WAL mode a reader does
// not block the writer, and the writer does not block a reader
// (https://sqlite.org/wal.html), so a snapshot can be taken while serve runs,
// from this process or from another one. SQLite does not fsync the output
// file, so the functions here do.

// ErrNotStore means a file is not a hivemind store.
var ErrNotStore = errors.New("not a hivemind store")

// SnapshotTo writes a consistent copy of the store to dest, which must not
// exist. The copy is a single SQLite file with no WAL, mode 0600. It runs on
// the store's one connection, so other calls on this store wait for it; the
// store is small, and a snapshot takes milliseconds.
func (s *Store) SnapshotTo(ctx context.Context, dest string) error {
	return vacuumInto(ctx, s.db, dest)
}

// SnapshotFile writes a consistent copy of the store file at path to dest,
// without opening it as a Store: it applies no migrations and changes no
// modes, so it is safe to run beside a serve process, of this version or
// another. A path that does not exist, or a file that is not a hivemind
// store, is an error.
func SnapshotFile(ctx context.Context, path, dest string) error {
	db, err := openExisting(path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := schemaVersion(ctx, db); err != nil {
		return err
	}
	return vacuumInto(ctx, db, dest)
}

// FileInfo describes a store file.
type FileInfo struct {
	// SchemaVersion is the version the file was written at.
	SchemaVersion int
	// NodeID is the cluster's own node, from the cluster record. Empty if
	// init never ran on it.
	NodeID string
	// CARotation is the CA rotation the file records, or nil for none, as
	// for a file from before CARotationSchemaVersion.
	CARotation *CARotation
}

// InspectFile checks a store file without migrating it: SQLite's
// integrity check must pass, and it must have a schema version. It returns
// the version and the cluster's node ID. It does not compare the version
// with this build's; the caller does.
func InspectFile(ctx context.Context, path string) (FileInfo, error) {
	db, err := openExisting(path)
	if err != nil {
		return FileInfo{}, err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check(1)`).Scan(&res); err != nil {
		return FileInfo{}, fmt.Errorf("%w: %s: %v", ErrNotStore, path, err)
	}
	if res != "ok" {
		return FileInfo{}, fmt.Errorf("%w: %s fails SQLite's integrity check: %s", ErrNotStore, path, res)
	}
	v, err := schemaVersion(ctx, db)
	if err != nil {
		return FileInfo{}, err
	}
	info := FileInfo{SchemaVersion: v}
	if v >= 3 {
		err := db.QueryRowContext(ctx, `SELECT node_id FROM cluster WHERE id = 1`).Scan(&info.NodeID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return FileInfo{}, err
		}
	}
	if v >= CARotationSchemaVersion {
		r, err := getCARotation(ctx, db)
		switch {
		case err == nil:
			info.CARotation = &r
		case !errors.Is(err, ErrNotFound):
			return FileInfo{}, err
		}
	}
	return info, nil
}

// openExisting opens the SQLite file at path, which must exist. database/sql
// would otherwise create an empty one.
func openExisting(path string) (*sql.DB, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("%w: store path %q: must be non-empty, without ? or #", ErrInvalid, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrNotStore, path)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("%w: no schema version: %v", ErrNotStore, err)
	}
	return v, nil
}

// vacuumInto runs VACUUM INTO dest on db. dest is created first, empty and
// with mode 0600, so the copy never exists with a wider mode; VACUUM INTO
// accepts an empty file. The copy is synced to disk before it returns.
func vacuumInto(ctx context.Context, db *sql.DB, dest string) error {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("snapshot file: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(dest)
		}
	}()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("snapshot of the store: %w", err)
	}
	if err := syncFile(dest); err != nil {
		return err
	}
	if err := os.Chmod(dest, 0o600); err != nil {
		return err
	}
	ok = true
	return nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	serr := f.Sync()
	if cerr := f.Close(); serr == nil {
		serr = cerr
	}
	return serr
}
