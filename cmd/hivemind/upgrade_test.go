package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/upgrade"
)

// v1Dir makes a data directory holding a v1 app.db.
func v1Dir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "v1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE apps (id TEXT PRIMARY KEY, name TEXT NOT NULL, image TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return dir
}

func TestInitRefusesV1DataDir(t *testing.T) {
	isolate(t)
	dir := v1Dir(t)
	_, _, err := run(t, "", "init", "--data-dir", dir)
	if !errors.Is(err, upgrade.ErrV1Data) || !strings.Contains(err.Error(), "docs/upgrading.md") {
		t.Fatalf("init: err = %v, want the v1 refusal", err)
	}
	// Nothing new was written into the v1 directory.
	for _, name := range []string{storeFileName, "admin.token"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after a refused init: %v", name, err)
		}
	}
}

func TestServeRefusesV1DataDir(t *testing.T) {
	isolate(t)
	dir := v1Dir(t)
	_, _, err := run(t, "", "serve", "--data-dir", dir)
	if !errors.Is(err, upgrade.ErrV1Data) || !strings.Contains(err.Error(), "docs/upgrading.md") {
		t.Fatalf("serve: err = %v, want the v1 refusal", err)
	}
	if _, err := os.Stat(filepath.Join(dir, storeFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s was created by a refused serve: %v", storeFileName, err)
	}
}
