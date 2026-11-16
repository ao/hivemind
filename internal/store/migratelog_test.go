package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Opening an existing store at an older schema logs one line per step,
// with what the step adds. A new store logs none.
func TestMigrationLogsEachStep(t *testing.T) {
	if len(migrationNotes) != len(migrations) {
		t.Fatalf("%d migration notes for %d migrations", len(migrationNotes), len(migrations))
	}
	path := filepath.Join(t.TempDir(), "hivemind.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctxT(t), db, migrations[:6]); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var logs bytes.Buffer
	openT(t, path, WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != SchemaVersion-6 {
		t.Fatalf("%d lines for %d steps:\n%s", len(lines), SchemaVersion-6, logs.String())
	}
	for i, l := range lines {
		from := 6 + i
		note := migrationNotes[from]
		if strings.ContainsAny(note, " =\"") {
			note = strconv.Quote(note)
		}
		want := fmt.Sprintf(`msg="store schema migrated" from=%d to=%d adds=%s`, from, from+1, note)
		if !strings.Contains(l, want) {
			t.Errorf("line %d: %s, want %s", i, l, want)
		}
	}

	var fresh bytes.Buffer
	openT(t, filepath.Join(t.TempDir(), "new.db"), WithLogger(slog.New(slog.NewTextHandler(&fresh, nil))))
	if fresh.Len() != 0 {
		t.Fatalf("a new store logged:\n%s", fresh.String())
	}
}
