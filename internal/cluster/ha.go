package cluster

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ao/hivemind/internal/ha"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/ratelimit"
	"github.com/ao/hivemind/internal/store"
)

// A cluster of several servers (docs/ha.md). A follower's node and join
// listeners pass every request to the leader; the leader serves them as
// its own listeners would, and every node API reply names the servers.

// Leadership is what the cluster listeners need of a cluster of several
// servers: whether this server leads, and how to pass a request to the
// leader. ha.Node is one.
type Leadership interface {
	IsLeader() bool
	Forward(kind string, w http.ResponseWriter, r *http.Request)
}

// HAHolder holds the Leadership of a cluster of several servers, or none
// for a cluster of one. Raft can be turned on while serve runs, at the
// first server join, so the listeners read it at each request.
type HAHolder struct{ p atomic.Pointer[leadershipBox] }

type leadershipBox struct{ l Leadership }

// Set makes l the cluster's Leadership.
func (h *HAHolder) Set(l Leadership) { h.p.Store(&leadershipBox{l}) }

// Get returns the Leadership, or nil for a cluster of one.
func (h *HAHolder) Get() Leadership {
	if h == nil {
		return nil
	}
	if b := h.p.Load(); b != nil {
		return b.l
	}
	return nil
}

// ServerJoiner admits a server that presented a valid server join token
// as node id: it turns Raft on if the cluster has none yet, adds the server
// as a non-voter, records it, and returns the reply. A refusal that the
// joining machine can act on wraps store.ErrConflict.
type ServerJoiner func(ctx context.Context, id string, req nodeapi.JoinServerRequest) (nodeapi.JoinServerReply, error)

// forwardOr passes a request to the leader when this server is a follower
// of a cluster of several, and serves it with next otherwise. Every
// request first loses its forwarding headers: only the leader's server
// port takes them, from another server. A log poll or log stream that it
// passes on ends when stop does, at the listener's Shutdown, as one this
// server serves itself ends when its log relay closes. Before, a follower's
// stop waited the whole shutdown timeout, 10 s, for the log poll its own
// node held through it (eighth soak: every join --role server took 10 s
// to stop, with a warning).
func forwardOr(kind string, h *HAHolder, stop context.Context, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ha.StripForwardHeaders(r.Header)
		if l := h.Get(); l != nil && !l.IsLeader() {
			if kind == ha.KindNode && isLogPath(r.URL.Path) {
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				defer context.AfterFunc(stop, cancel)()
				r = r.WithContext(ctx)
			}
			l.Forward(kind, w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLogPath reports whether p is a node's log poll or a log stream, which
// stay open for as long as they wait or stream.
func isLogPath(p string) bool {
	rest, ok := strings.CutPrefix(p, nodeapi.Prefix+"/nodes/")
	if !ok {
		return false
	}
	_, tail, ok := strings.Cut(rest, "/")
	return ok && (tail == "logs/requests" || strings.HasPrefix(tail, "logs/streams/"))
}

// serversEvery is how long the server list for ServersHeader is kept
// before it is read again.
const serversEvery = 2 * time.Second

// serverList is the value of ServersHeader, read from the store now and
// then: empty for a cluster of one.
type serverList struct {
	st *store.Store
	mu sync.Mutex
	at time.Time
	v  string
}

func (l *serverList) value(ctx context.Context) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.at) < serversEvery {
		return l.v
	}
	l.at = time.Now()
	servers, err := l.st.ListServers(ctx)
	if err != nil || len(servers) < 2 {
		l.v = ""
		return l.v
	}
	addrs := make([]string, 0, len(servers))
	for _, s := range servers {
		addrs = append(addrs, s.NodeAddr())
	}
	l.v = nodeapi.FormatServers(addrs)
	return l.v
}

// withServers names the servers on every reply.
func withServers(l *serverList, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := l.value(r.Context()); v != "" {
			w.Header().Set(nodeapi.ServersHeader, v)
		}
		next.ServeHTTP(w, r)
	})
}

// joinServer admits a server, as join admits a worker: the source address
// is not over its failure limit; the token parses and names this
// cluster's CA; no CA rotation is in progress; the joining server runs this
// server's version; the token, a server's, is redeemed; and the
// ServerJoiner adds it. The token is redeemed before the server is added,
// so a join that fails after that has used its token.
func joinServer(cfg Config, limiter *ratelimit.FailLimiter, log *slog.Logger, w http.ResponseWriter, r *http.Request) {
	src := ratelimit.SourceAddr(r)
	if !limiter.Allow(src) {
		w.Header().Set("Retry-After", strconv.Itoa(int(limiter.RetryAfter(src).Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed joins from this address; try again later")
		log.Warn("server join refused by rate limit", "remote", src)
		return
	}
	fail := func(code int, msg, why string) {
		limiter.Fail(src)
		log.Warn("server join failed", "remote", src, "reason", why)
		writeError(w, code, msg)
	}
	var req nodeapi.JoinServerRequest
	if err := nodeapi.Decode(r.Body, nodeapi.MaxJoinBytes, &req); err != nil {
		if errors.Is(err, nodeapi.ErrTooLarge) {
			fail(http.StatusRequestEntityTooLarge, "request body is too large", err.Error())
			return
		}
		fail(http.StatusBadRequest, "invalid join request", err.Error())
		return
	}
	tok, err := pki.ParseJoinToken(req.Token)
	if err != nil {
		fail(http.StatusUnauthorized, errJoinRefused, "malformed token")
		return
	}
	if cfg.Rotation.TokenCA(tok.Fingerprint) == nil {
		fail(http.StatusUnauthorized, errJoinRefused, "token names another CA")
		return
	}
	if cfg.JoinServer == nil || cfg.ServerTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "this server cannot add servers")
		return
	}
	if cfg.Rotation.Rotating() {
		writeError(w, http.StatusConflict, "a CA rotation is in progress, and a server cannot join during one: finish or abort it first (hivemind ca status)")
		return
	}
	if err := nodeapi.ValidateAdvertise(req.Advertise); err != nil {
		fail(http.StatusBadRequest, err.Error(), "advertise address")
		return
	}
	if req.NodePort < 1 || req.NodePort > 65533 {
		fail(http.StatusBadRequest, "the node port must be in 1-65533: the join port and the server port are above it", "node port")
		return
	}
	if v, mine := nodeapi.CleanVersion(req.Version), nodeapi.CleanVersion(cfg.Version); v != mine {
		writeError(w, http.StatusConflict, "every server of a cluster runs the same hivemind: this server runs "+orUnknown(mine)+
			", and the joining one "+orUnknown(v))
		return
	}
	if _, err := cfg.ServerTokens.Redeem(req.Token); err != nil {
		fail(http.StatusUnauthorized, errJoinRefused, err.Error())
		return
	}
	id, err := freshNodeID(r, cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		log.Error("node ID", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	reply, err := cfg.JoinServer(ctx, id, req)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotLeader) {
			code = http.StatusConflict
		}
		writeError(w, code, "the server could not be added: "+err.Error())
		log.Error("server join", "node", id, "remote", src, "err", err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
	log.Info("server joined the cluster as a non-voter; it becomes a voter once it has caught up", "node", id,
		"advertise", req.Advertise, "remote", src)
}

func orUnknown(v string) string {
	if v == "" {
		return "an unknown version"
	}
	return v
}

// epochRef is the epoch the node routes send and check: Listen's, which
// SetEpoch changes, or Epoch for routes made without Listen.
func (cfg Config) epochRef() *atomic.Int64 {
	if cfg.epoch != nil {
		return cfg.epoch
	}
	e := &atomic.Int64{}
	e.Store(cfg.Epoch)
	return e
}

// SetEpoch sets the epoch the node routes send and check, for a server
// that joined a cluster of several and learns it from the store the leader
// sends it, after Listen.
func (c *Cluster) SetEpoch(e int64) { c.epoch.Store(e) }
