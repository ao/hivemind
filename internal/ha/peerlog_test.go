package ha

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// take returns the lines written since the last take.
func (l *lockedBuffer) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := strings.TrimSpace(l.b.String())
	l.b.Reset()
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// TestPeerFailuresAreLimited: 100 failed heartbeats to a server that is down,
// in one second, are one line at warn, not 100 at error (the eighth soak's
// sixth finding); 30 s later the next failure, of another RPC, is a line
// with the count left out; another server's failures are its own; when
// the server answers again, a line says so, and its next failure is logged
// at once. Raft's other errors stay errors.
func TestPeerFailuresAreLimited(t *testing.T) {
	var buf lockedBuffer
	h := newHCLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	now := time.Unix(1_700_000_000, 0)
	h.fails.now = func() time.Time { return now }
	l := h.Named("replication")
	down := raft.Server{ID: "node-b", Address: "10.0.0.2:4486"}
	dial := errors.New("dial tcp 10.0.0.2:4486: connect: no route to host")
	for i := 0; i < 100; i++ {
		now = now.Add(10 * time.Millisecond)
		l.Error("failed to heartbeat to", "peer", down.Address, "backoff time", 50*time.Millisecond, "error", dial)
	}
	lines := buf.take()
	if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "failed to heartbeat to") ||
		!strings.Contains(lines[0], "10.0.0.2:4486") {
		t.Fatalf("100 failed heartbeats in 1 s logged %d lines: %q", len(lines), lines)
	}

	now = now.Add(peerFailEvery)
	l.Error("failed to appendEntries to", "peer", down, "error", dial)
	l.Error("failed to make requestVote RPC", "target", raft.Server{ID: "node-c", Address: "10.0.0.3:4486"}, "error", dial, "term", 4)
	lines = buf.take()
	if len(lines) != 2 || !strings.Contains(lines[0], "failed to appendEntries to") || !strings.Contains(lines[0], "repeated=99") ||
		!strings.Contains(lines[1], "10.0.0.3:4486") || strings.Contains(lines[1], "repeated") {
		t.Fatalf("after 30 s, and for another server: %q", lines)
	}

	h.fails.answered("10.0.0.2:4486")
	lines = buf.take()
	if len(lines) != 1 || !strings.Contains(lines[0], "level=INFO") || !strings.Contains(lines[0], "answers again") ||
		!strings.Contains(lines[0], "failures=101") {
		t.Fatalf("the server answering again: %q", lines)
	}
	l.Error("failed to heartbeat to", "peer", down.Address, "error", dial)
	l.Error("failed to take snapshot", "error", errors.New("disk full"))
	l.Error("failed to decode incoming command", "error", errors.New("read tcp: connection timed out"))
	lines = buf.take()
	if len(lines) != 3 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[1], "level=ERROR") ||
		!strings.Contains(lines[2], "level=WARN") {
		t.Fatalf("a failure after the answer, another error, a broken connection: %q", lines)
	}
}
