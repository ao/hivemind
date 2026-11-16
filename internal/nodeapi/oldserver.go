package nodeapi

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrNoRoute means the server answered 404 for a route it does not have:
// it is older than this worker, and lacks the feature the route serves. It
// is not ErrNotRegistered. The node is registered, so registering or joining
// again does not help; upgrading the server does.
var ErrNoRoute = errors.New("the server has no such route, so it is older than this worker")

// ErrorHeader marks an error reply of the node API whose cause a worker acts
// on. Its one value is ErrorNotRegistered, on the 404 for a node the server
// has no record of. A 404 without it, from a server that sends its version,
// is a route the server lacks or another refusal; a server older than 2.2
// marks nothing.
const (
	ErrorHeader        = "Hivemind-Error"
	ErrorNotRegistered = "node-not-registered"
)

// serverState is what a client knows of the server from its replies.
type serverState struct {
	mu      sync.Mutex
	version string
	changed chan struct{} // closed when version changes, then replaced
}

func newServerState() *serverState { return &serverState{changed: make(chan struct{})} }

// see records the version a reply carried.
func (s *serverState) see(v string) {
	v = CleanVersion(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	if v == s.version {
		return
	}
	s.version = v
	close(s.changed)
	s.changed = make(chan struct{})
}

// ServerVersion is the version the server sent with its last reply, or ""
// when it sent none (it is older than 2.2) or has not replied yet.
func (c *Client) ServerVersion() string {
	c.tr.server.mu.Lock()
	defer c.tr.server.mu.Unlock()
	return c.tr.server.version
}

// ServerChanged returns a channel that is closed once a reply from the
// server carries another version than the last reply did, as after the
// server was upgraded.
func (c *Client) ServerChanged() <-chan struct{} {
	c.tr.server.mu.Lock()
	defer c.tr.server.mu.Unlock()
	return c.tr.server.changed
}

// ServerWatcher is what a MissingRoute needs to know of the server. Client
// is one.
type ServerWatcher interface {
	ServerVersion() string
	ServerChanged() <-chan struct{}
}

// DefaultOldServerRetry is how long a MissingRoute holds off before it lets
// a caller try a missing route again, when the server's version has not
// changed in the meantime.
const DefaultOldServerRetry = 10 * time.Minute

// MissingRoute holds off a worker's calls to one route that the server
// lacks, and logs that once, as "the server is older than this worker and
// has no <feature>; upgrade the server", rather than at every retry. The
// calls start again when the server's version changes, which is what an
// upgrade of the server does, or after Retry. It is safe for concurrent
// use, and a nil MissingRoute holds nothing off and logs nothing.
type MissingRoute struct {
	// Retry is how long to hold off when the server does not change. Zero
	// means DefaultOldServerRetry.
	Retry time.Duration

	w       ServerWatcher
	feature string
	log     *slog.Logger

	mu      sync.Mutex
	since   time.Time       // when the route was last missing; zero if it is not
	changed <-chan struct{} // the server's change channel at that time
	logged  bool
	loggedV string // the server version the line was logged for
}

// NewMissingRoute returns a MissingRoute for feature, such as "log
// streaming", that logs to log.
func NewMissingRoute(w ServerWatcher, feature string, log *slog.Logger) *MissingRoute {
	if log == nil {
		log = slog.Default()
	}
	return &MissingRoute{w: w, feature: feature, log: log}
}

func (m *MissingRoute) retry() time.Duration {
	if m.Retry > 0 {
		return m.Retry
	}
	return DefaultOldServerRetry
}

// Skip reports whether the caller should leave the route alone for now: its
// last call got ErrNoRoute, and since then the server's version has not
// changed and Retry has not passed.
func (m *MissingRoute) Skip() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.since.IsZero() {
		return false
	}
	select {
	case <-m.changed:
		m.since = time.Time{}
		return false
	default:
	}
	if time.Since(m.since) >= m.retry() {
		m.since = time.Time{}
		return false
	}
	return true
}

// Check takes the result of a call to the route, and returns err. On
// ErrNoRoute it logs, once for each version of the server, with args, and
// Skip holds off later calls.
func (m *MissingRoute) Check(err error, args ...any) error {
	if m == nil {
		return err
	}
	if !errors.Is(err, ErrNoRoute) {
		if err == nil {
			m.mu.Lock()
			m.since = time.Time{}
			m.mu.Unlock()
		}
		return err
	}
	v := m.w.ServerVersion()
	m.mu.Lock()
	m.since, m.changed = time.Now(), m.w.ServerChanged()
	first := !m.logged || m.loggedV != v
	m.logged, m.loggedV = true, v
	m.mu.Unlock()
	if first {
		sv := v
		if sv == "" {
			sv = "none (older than 2.2)"
		}
		m.log.Warn("the server is older than this worker and has no "+m.feature+"; upgrade the server",
			append([]any{"server_version", sv}, args...)...)
	}
	return err
}

// Wait waits until Skip would let a call through, or ctx ends.
func (m *MissingRoute) Wait(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	since, changed := m.since, m.changed
	m.mu.Unlock()
	if since.IsZero() {
		return
	}
	t := time.NewTimer(time.Until(since.Add(m.retry())))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-changed:
	case <-t.C:
	}
}
