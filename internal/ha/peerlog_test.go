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

// String is what was written since the last take.
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
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

// TestRaftsRepeatedLinesAreLimited: raft's election timeouts while a server
// cannot win one, and its failed flushes of an answer on a broken
// connection, are a warning, then one every 30 s with the number left out;
// a leader's "peer has newer term" is info while a handover it began is
// under way, and a warning otherwise, not an error (the ninth soak's
// fifth finding). Raft's other warnings are as they were.
func TestRaftsRepeatedLinesAreLimited(t *testing.T) {
	var buf lockedBuffer
	h := newHCLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	now := time.Unix(1_700_000_000, 0)
	h.fails.now = func() time.Time { return now }
	for i := 0; i < 43; i++ {
		now = now.Add(time.Second)
		h.Warn("Election timeout reached, restarting election")
	}
	h.Error("failed to flush response", "error", errors.New("write tcp 10.0.0.2:4486->10.0.0.3:51234: write: broken pipe"))
	h.Error("failed to flush response", "error", errors.New("write: broken pipe"))
	lines := buf.take()
	if len(lines) != 3 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "Election timeout reached") ||
		strings.Contains(lines[0], "repeated") || !strings.Contains(lines[1], "Election timeout reached") || !strings.Contains(lines[1], "repeated=29") ||
		!strings.Contains(lines[2], "level=WARN") || !strings.Contains(lines[2], "failed to flush response") {
		t.Fatalf("43 election timeouts in 43 s and two failed flushes: %q", lines)
	}

	h.Error(newerTermMsg, "peer", "10.0.0.3:4486")
	h.fails.handingOver()
	now = now.Add(time.Second)
	h.Named("replication").Error(newerTermMsg, "peer", "10.0.0.3:4486")
	now = now.Add(handoverQuiet)
	h.Error(newerTermMsg, "peer", "10.0.0.3:4486")
	h.Warn("heartbeat timeout reached, starting election", "last-leader-addr", "10.0.0.3:4486")
	h.Warn("heartbeat timeout reached, starting election", "last-leader-addr", "10.0.0.3:4486")
	lines = buf.take()
	if len(lines) != 5 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[1], "level=INFO") ||
		!strings.Contains(lines[2], "level=WARN") || !strings.Contains(lines[3], "level=WARN") || !strings.Contains(lines[4], "level=WARN") {
		t.Fatalf("a newer term before, during and after a handover, and two other warnings: %q", lines)
	}
}
