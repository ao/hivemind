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

// repeatedMsgs are raft's lines, at warn or error, that come over and over
// while a server cannot reach the others or one comes and goes: each is
// logged at warn, then once every peerFailEvery with the number left out.
// In the ninth soak a server cut off from the others logged 43 election
// timeouts in 65 s, and a server whose process was stopped and resumed
// one failed flush of an answer at error, for a connection that had broken.
var repeatedMsgs = map[string]bool{
	"Election timeout reached, restarting election": true,
	"failed to flush response":                      true,
}

// newerTermMsg is the line of a leader's replication to a server that
// answered with a later term: another server was elected. It is info while
// a handover that this server began is under way (handoverQuiet), as the
// new leader's election is what it asked for, and a warning otherwise; it
// was an error, at every planned handover (ninth soak).
const newerTermMsg = "peer has newer term, stopping replication"

// handoverQuiet is how long after a handover began a newer term is
// expected.
const handoverQuiet = 10 * time.Second

// peerFailEvery is how often a server's failed RPCs are logged.
const peerFailEvery = 30 * time.Second

// peerFailures limits the lines for each server's failed RPCs. It is
// shared by an hcLogger and every logger made from it.
type peerFailures struct {
	mu    sync.Mutex
	now   func() time.Time
	l     *slog.Logger
	peers map[string]*peerFail // by server address, or "msg:" and a repeatedMsgs line
	// handover is when this server last began a handover (handingOver).
	handover time.Time
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

// handingOver notes that this server begins a handover of the leadership.
func (p *peerFailures) handingOver() {
	p.mu.Lock()
	p.handover = p.now()
	p.mu.Unlock()
}

// inHandover reports whether a handover this server began is under way.
func (p *peerFailures) inHandover() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.handover.IsZero() && p.now().Sub(p.handover) < handoverQuiet
}

// limit decides how hcLogger logs a line of raft's at warn or error, lv:
// unchanged, at another level, or not at all; with the arguments to add.
func (p *peerFailures) limit(lv slog.Level, msg string, args []any) (slog.Level, bool, []any) {
	switch {
	case p == nil:
		return lv, true, nil
	case repeatedMsgs[msg]:
		return p.limited("msg:" + msg)
	case lv != slog.LevelError:
		return lv, true, nil
	case warnMsgs[msg]:
		return slog.LevelWarn, true, nil
	case msg == newerTermMsg:
		if p.inHandover() {
			return slog.LevelInfo, true, nil
		}
		return slog.LevelWarn, true, nil
	}
	key, ok := peerFailMsgs[msg]
	if !ok {
		return slog.LevelError, true, nil
	}
	peer := ""
	for i := 0; i+1 < len(args); i += 2 {
		if k, _ := args[i].(string); k == key {
			peer = peerKey(args[i+1])
			break
		}
	}
	return p.limited(peer)
}

// limited is a line counted under key: at warn, or not at all, with the
// number left out since the last.
func (p *peerFailures) limited(key string) (slog.Level, bool, []any) {
	log, left := p.take(key)
	if !log {
		return 0, false, nil
	}
	if left > 0 {
		return slog.LevelWarn, true, []any{"repeated", left, "in", peerFailEvery.String()}
	}
	return slog.LevelWarn, true, nil
}
