package ha

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// Raft logs at error every try of an RPC to another server that fails: for
// one server that is down, about 25 lines a minute on the leader (eighth
// soak: 71 in the 2.5 minutes of a power-off, 204 on the survivor of a
// quorum loss), which pages anyone who alerts on errors for a server that
// is known to be down. hcLogger logs these at warn instead, the first for
// a server, then one every peerFailEvery with the number left out since,
// and one at info when the server answers the leader again.

// peerFailMsgs are raft's lines for a failed RPC to another server, and the
// argument that names the server.
var peerFailMsgs = map[string]string{
	"failed to heartbeat to":                  "peer",
	"failed to appendEntries to":              "peer",
	"failed to start pipeline replication to": "peer",
	"failed to pipeline appendEntries":        "peer",
	"failed to make requestVote RPC":          "target",
}

// warnMsgs are raft's error lines that are logged at warn as they are: a
// connection from another server that broke, as when its machine went away.
var warnMsgs = map[string]bool{
	"failed to decode incoming command": true,
}

// peerFailEvery is how often a server's failed RPCs are logged.
const peerFailEvery = 30 * time.Second

// peerFailures limits the lines for each server's failed RPCs. It is
// shared by an hcLogger and every logger made from it.
type peerFailures struct {
	mu    sync.Mutex
	now   func() time.Time
	l     *slog.Logger
	peers map[string]*peerFail
}

type peerFail struct {
	logged   time.Time // when a line was last logged
	failed   time.Time // the last failure
	left     int       // failures left out since logged
	failures int       // failures since the first
}

func newPeerFailures(l *slog.Logger) *peerFailures {
	return &peerFailures{now: time.Now, l: l, peers: map[string]*peerFail{}}
}

// take records a failure of an RPC to peer, and reports whether to log it
// now, and how many failures were left out before it.
func (p *peerFailures) take(peer string) (bool, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	f := p.peers[peer]
	if f == nil {
		p.peers[peer] = &peerFail{logged: now, failed: now, failures: 1}
		return true, 0
	}
	f.failed = now
	f.failures++
	if now.Sub(f.logged) < peerFailEvery {
		f.left++
		return false, 0
	}
	left := f.left
	f.logged, f.left = now, 0
	return true, left
}

// answered notes that peer answered an RPC: a server whose failures were
// logged in the last peerFailEvery gets a line saying it answers again.
func (p *peerFailures) answered(peer string) {
	p.mu.Lock()
	f := p.peers[peer]
	if f == nil {
		p.mu.Unlock()
		return
	}
	delete(p.peers, peer)
	recent := p.now().Sub(f.failed) < peerFailEvery
	p.mu.Unlock()
	if recent {
		p.l.Info("raft: a server answers again", "peer", peer, "failures", f.failures)
	}
}

// peerKey is the address of the server a log argument names.
func peerKey(v any) string {
	switch p := v.(type) {
	case raft.Server:
		return string(p.Address)
	case *raft.Server:
		return string(p.Address)
	case raft.ServerAddress:
		return string(p)
	}
	return fmt.Sprint(v)
}

// limit decides how hcLogger logs a line of raft's at error: unchanged, at
// warn, or not at all; with the arguments to add.
func (p *peerFailures) limit(msg string, args []any) (slog.Level, bool, []any) {
	if warnMsgs[msg] {
		return slog.LevelWarn, true, nil
	}
	key, ok := peerFailMsgs[msg]
	if !ok || p == nil {
		return slog.LevelError, true, nil
	}
	peer := ""
	for i := 0; i+1 < len(args); i += 2 {
		if k, _ := args[i].(string); k == key {
			peer = peerKey(args[i+1])
			break
		}
	}
	log, left := p.take(peer)
	if !log {
		return 0, false, nil
	}
	if left > 0 {
		return slog.LevelWarn, true, []any{"repeated", left, "in", peerFailEvery.String()}
	}
	return slog.LevelWarn, true, nil
}
