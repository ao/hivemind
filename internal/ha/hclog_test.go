package ha

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
)

// TestRaftsFormatValuesAreFormatted: a value raft gives as hclog.Fmt is
// printed formatted, not as its format and arguments (the fifth acceptance
// run's seventh finding), in a line's arguments and in With's.
func TestRaftsFormatValuesAreFormatted(t *testing.T) {
	var buf lockedBuffer
	h := newHCLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	servers := []raft.Server{{Suffrage: raft.Voter, ID: "node-a", Address: "10.0.0.1:4486"}}
	args := []any{"index", 0, "servers", hclog.Fmt("%+v", servers)}
	h.Info("initial configuration", args...)
	h.With("progress", hclog.Fmt("%0.2f%%", 100.0)).Info("snapshot network transfer progress")
	lines := buf.take()
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	if strings.Contains(lines[0], "%+v") || !strings.Contains(lines[0], "ID:node-a") || !strings.Contains(lines[0], "Address:10.0.0.1:4486") {
		t.Fatalf("the configuration: %q", lines[0])
	}
	if strings.Contains(lines[1], "%0.2f") || !strings.Contains(lines[1], "progress=100.00%") {
		t.Fatalf("the progress: %q", lines[1])
	}
	if _, ok := args[3].(hclog.Format); !ok {
		t.Fatal("the caller's arguments were changed")
	}
}

// TestRaftsBehindTheLogLinesAreInfo: a leader's "failed to get log" with
// "log not found", which it follows with a snapshot, as at every server
// join, and a follower's "failed to get previous log" are info; the same
// lines with another error keep their level.
func TestRaftsBehindTheLogLinesAreInfo(t *testing.T) {
	var buf lockedBuffer
	h := newHCLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	h.Error("failed to get log", "index", 1, "error", raft.ErrLogNotFound)
	h.Warn("failed to get previous log", "previous-index", 5, "last-index", 3, "error", raft.ErrLogNotFound)
	h.Error("failed to get log", "index", 1, "error", errors.New("disk I/O error"))
	lines := buf.take()
	if len(lines) != 3 || !strings.Contains(lines[0], "level=INFO") || !strings.Contains(lines[1], "level=INFO") ||
		!strings.Contains(lines[2], "level=ERROR") {
		t.Fatalf("lines: %q", lines)
	}
}
