package store

import (
	"context"
	"time"

	"github.com/ao/hivemind/internal/scheduler"
)

// The start grace.
//
// A server that starts, after a restart, an outage or a restore, finds every
// lease as old as the time it was down. A worker that was up then kept its
// containers running, and its router kept serving them, and it reports again
// once it reaches the new process. That can take a while: after a restore
// at a new address, the workers reach it once their resolvers have the
// name's new record, or once each is moved with set-server. For the start
// grace after serve starts, such a node is waiting: it is not lost, its
// replicas are not placed again, and the routing table keeps its endpoints
// as it last reported them. A node that renews its lease in the grace is
// live from then on, as any node is. A node whose lease had already lapsed
// when the server went down is not waiting, and is lost at once.
//
// When the server went down is taken to be the latest lease a worker renewed
// before the start: every worker that was up renewed within a few seconds of
// it. The server's own node is left out, because serve renews its lease as
// it starts, before or after SetStarted.
//
// The cost of the grace: a worker that really died while the server was
// down has its replicas placed again only when the grace ends.

// DefaultStartGrace is the start grace of a serve that is given none.
const DefaultStartGrace = 2 * time.Minute

// startState is serve's start, as SetStarted recorded it. All three are
// Unix nanoseconds.
type startState struct {
	at    int64 // when serve started
	until int64 // when the start grace ends
	last  int64 // the latest lease a worker renewed before at; 0 when none had
}

// SetStarted records that serve started at t, with a start grace of grace;
// a grace shorter than the lease timeout is the lease timeout. It reads the
// latest lease a worker renewed before t, so it is called once, as serve
// starts, before any worker can have reported. The routing table then
// leaves out the server's own node's reports from before t, and the nodes
// that were up before t wait for their first report until the grace ends
// (see Waiting).
func (s *Store) SetStarted(ctx context.Context, t time.Time, grace time.Duration) error {
	grace = max(grace, scheduler.DefaultLeaseTimeout)
	var last int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(max(last_lease), 0) FROM nodes
		WHERE last_lease < ? AND id IS NOT (SELECT node_id FROM cluster WHERE id = 1)`, t.UnixNano()).Scan(&last); err != nil {
		return err
	}
	s.start.Store(&startState{at: t.UnixNano(), until: t.Add(grace).UnixNano(), last: last})
	return nil
}

// startedAt is when serve started, in Unix nanoseconds, or 0 before
// SetStarted.
func (s *Store) startedAt() int64 {
	if st := s.start.Load(); st != nil {
		return st.at
	}
	return 0
}

// waitWindow returns the leases of the nodes that are waiting at now: a
// node whose last lease is at least from and before before. Outside the
// start grace no lease is in it.
func (s *Store) waitWindow(now time.Time) (from, before int64) {
	st := s.start.Load()
	n := now.UnixNano()
	if st == nil || st.last == 0 || n < st.at || n >= st.until {
		return 1, 0
	}
	return st.last - int64(scheduler.DefaultLeaseTimeout), st.at
}

// Waiting reports whether n is waiting for its first report at now: it is
// in the start grace, n has not renewed its lease since serve started, and
// n was up when the server went down. A waiting node is not lost.
func (s *Store) Waiting(n Node, now time.Time) bool {
	from, before := s.waitWindow(now)
	l := n.LastLease.UnixNano()
	return l >= from && l < before
}
