package upgrade

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/store"
)

// v1App is the apps table from v1's app.db (internal/app/manager.go, before
// f9958e3). v1 created it with CREATE TABLE IF NOT EXISTS.
const v1App = `CREATE TABLE IF NOT EXISTS apps (
	id TEXT PRIMARY KEY, name TEXT NOT NULL, image TEXT NOT NULL,
	command TEXT NOT NULL, env TEXT NOT NULL, volumes TEXT NOT NULL,
	ports TEXT NOT NULL, cpu REAL NOT NULL, memory INTEGER NOT NULL,
	gpu INTEGER NOT NULL, disk INTEGER NOT NULL, state TEXT NOT NULL,
	node_id TEXT, created_at INTEGER NOT NULL, started_at INTEGER, finished_at INTEGER)`

// v1Volumes is the volumes table from v1's storage.db.
const v1Volumes = `CREATE TABLE IF NOT EXISTS volumes (
	name TEXT PRIMARY KEY, path TEXT NOT NULL, size INTEGER NOT NULL,
	created_at INTEGER NOT NULL, state TEXT NOT NULL)`

// writeSQLite creates path with the given statements.
func writeSQLite(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+uriPath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheckDataDirAcceptsEmptyAndMissing(t *testing.T) {
	if err := CheckDataDir(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("missing directory: %v", err)
	}
	if err := CheckDataDir(t.TempDir()); err != nil {
		t.Fatalf("empty directory: %v", err)
	}
}

func TestCheckDataDirAcceptsV2Store(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := CheckDataDir(dir); err != nil {
		t.Fatalf("v2 store: %v", err)
	}
}

func TestCheckDataDirRefusesV1AppDB(t *testing.T) {
	dir := t.TempDir()
	writeSQLite(t, filepath.Join(dir, "app.db"), v1App)
	err := CheckDataDir(dir)
	if !errors.Is(err, ErrV1Data) {
		t.Fatalf("err = %v, want ErrV1Data", err)
	}
	if got := err.Error(); !strings.Contains(got, "docs/upgrading.md") || !strings.Contains(got, "app.db") {
		t.Fatalf("message does not name the file or the guide: %q", got)
	}
}

func TestCheckDataDirRefusesV1StorageDB(t *testing.T) {
	dir := t.TempDir()
	writeSQLite(t, filepath.Join(dir, "storage.db"), v1Volumes)
	if err := CheckDataDir(dir); !errors.Is(err, ErrV1Data) {
		t.Fatalf("err = %v, want ErrV1Data", err)
	}
}

// A v1 directory that someone pointed v2 at still holds the v1 files, so v2
// refuses it even though it now also has a hivemind.db.
func TestCheckDataDirRefusesV1WithV2Store(t *testing.T) {
	dir := t.TempDir()
	writeSQLite(t, filepath.Join(dir, "app.db"), v1App)
	if err := os.WriteFile(filepath.Join(dir, "hivemind.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(dir); !errors.Is(err, ErrV1Data) {
		t.Fatalf("err = %v, want ErrV1Data", err)
	}
}

func TestCheckDataDirIgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	// Not SQLite at all.
	if err := os.WriteFile(filepath.Join(dir, "app.db"), []byte("plain text"), 0o600); err != nil {
		t.Fatal(err)
	}
	// SQLite, under a v1 name, but with none of v1's tables.
	dir2 := t.TempDir()
	writeSQLite(t, filepath.Join(dir2, "storage.db"), `CREATE TABLE notes (x INTEGER)`)
	// A directory named like a v1 file.
	dir3 := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir3, "app.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, dir2, dir3} {
		if err := CheckDataDir(d); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
}

func TestCheckDataDirFailsClosedOnUnreadableV1File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	writeSQLite(t, path, v1App)
	// Corrupt the table pages after the header, so the header still matches
	// and the query fails.
	if err := os.WriteFile(path, append([]byte(sqliteHeader), make([]byte, 4096)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(dir); err == nil || errors.Is(err, ErrV1Data) {
		t.Fatalf("err = %v, want a read error that is not ErrV1Data", err)
	}
}

func TestUriPathEscapes(t *testing.T) {
	if got := uriPath("/a?b#c%d"); got != "/a%3Fb%23c%25d" {
		t.Fatalf("uriPath = %q", got)
	}
	// A path with ? in it is still opened as that file.
	dir := t.TempDir()
	path := filepath.Join(dir, "odd?name", "app.db")
	if err := os.Mkdir(filepath.Join(dir, "odd?name"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSQLite(t, path, v1App)
	ok, err := hasV1Tables(path)
	if err != nil || !ok {
		t.Fatalf("hasV1Tables(%q) = %v, %v", path, ok, err)
	}
}
