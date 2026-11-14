// Package upgrade refuses data directories that hivemind v1 wrote.
package upgrade

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// ErrV1Data means the data directory holds a v1 database.
var ErrV1Data = errors.New("hivemind v1 data directory")

// v1Files are the SQLite files v1 created in its data directory: app.db for
// the apps and storage.db for the volumes. v2 creates neither name; its
// store is hivemind.db.
var v1Files = []string{"app.db", "storage.db"}

// v1Tables are the tables v1 created in those files.
var v1Tables = []string{"apps", "volumes", "nodes", "audit_logs", "tenant_stats"}

const sqliteHeader = "SQLite format 3\x00"

// CheckDataDir returns an error wrapping ErrV1Data if dir holds a v1 database.
// A missing directory, an empty one, a v2 store, or a file that is not SQLite
// with v1 tables in it passes. A v1 file that cannot be read is an error, so
// the caller refuses rather than guess.
func CheckDataDir(dir string) error {
	for _, name := range v1Files {
		path := filepath.Join(dir, name)
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checking %s: %w", path, err)
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		isV1, err := hasV1Tables(path)
		if err != nil {
			return fmt.Errorf("checking %s for a v1 database: %w", path, err)
		}
		if isV1 {
			return fmt.Errorf("%w: %s is a v1 database. v1 never started containers, so there is nothing to migrate. "+
				"Use an empty data directory for v2 (see docs/upgrading.md)", ErrV1Data, path)
		}
	}
	return nil
}

// hasV1Tables reports whether path is a SQLite database with a table v1 created.
func hasV1Tables(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	head := make([]byte, len(sqliteHeader))
	_, err = io.ReadFull(f, head)
	f.Close()
	if err != nil {
		// Shorter than a SQLite header: not a v1 database.
		return false, nil
	}
	if string(head) != sqliteHeader {
		return false, nil
	}

	// Open read-only, so a check never changes a v1 file.
	db, err := sql.Open("sqlite", "file:"+uriPath(path)+"?mode=ro")
	if err != nil {
		return false, err
	}
	defer db.Close()
	args := make([]any, len(v1Tables))
	marks := make([]string, len(v1Tables))
	for i, t := range v1Tables {
		args[i] = t
		marks[i] = "?"
	}
	query := `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN (` + strings.Join(marks, ",") + `)`
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// uriPath escapes the characters that would end or change a file: URI.
func uriPath(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
}
