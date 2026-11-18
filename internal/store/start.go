package store

import (
	"context"
	"sync/atomic"
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
//
// The hold. When the grace ends and none of the workers that were waiting
// has reported since the start, they are not lost: they go on waiting, and
// the server places none of their replicas, until one of them reports or an
// operator removes them. Every worker dying while the server was down is
// rare. A server that hears from none of its workers is more often an old
// copy of a cluster that was restored elsewhere, back on, whose workers
// refuse it, or a server cut off from its workers. In the fourth soak such
// an old server marked all three workers lost at its grace's end and ran
// all 17 movable replicas on its own machine, and its router served those
// copies. Once one of the waiting workers reports, the others that have not
// are lost as at the end of any grace. The cost of the hold: a cluster
// whose every worker died while the server was down keeps their replicas
// unplaced until an operator removes the nodes (hivemind node remove).

// DefaultStartGrace is the start grace of a serve that is given none.
const DefaultStartGrace = 2 * time.Minute

// startState is serve's start, as SetStarted recorded it. at, until and
// last are Unix nanoseconds.
type startState struct {
	at    int64 // when serve started
	until int64 // when the start grace ends
	last  int64 // the latest lease a worker renewed before at; 0 when none had
	// waiting holds the workers that were waiting at the start, the
	// server's own node left out.
	waiting map[string]bool
	// reported is set once one of them has renewed its lease since at
	// (noteReport). Until then the grace holds past until.
	reported atomic.Bool
}

// held reports whether the start holds the waiting workers past the grace:
// some were waiting, and none of them has reported.
func (st *startState) held() bool { return len(st.waiting) > 0 && !st.reported.Load() }

// SetStarted records that serve started at t, with a start grace of grace;
// a grace shorter than the lease timeout is the lease timeout. It reads the
// latest lease a worker renewed before t, and the workers that were up then,
// so it is called once, as serve starts, before any worker can have
// reported. The routing table then leaves out the server's own node's
// reports from before t, and the nodes that were up before t wait for their
// first report until the grace ends, or, while none of them has reported,
// past it (see Waiting).
func (s *Store) SetStarted(ctx context.Context, t time.Time, grace time.Duration) error {
	grace = max(grace, scheduler.DefaultLeaseTimeout)
	var last int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(max(last_lease), 0) FROM nodes
		WHERE last_lease < ? AND id IS NOT (SELECT node_id FROM cluster WHERE id = 1)`, t.UnixNano()).Scan(&last); err != nil {
		return err
	}
	st := &startState{at: t.UnixNano(), until: t.Add(grace).UnixNano(), last: last}
	if last != 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM nodes WHERE last_lease >= ? AND last_lease < ?
			AND id IS NOT (SELECT node_id FROM cluster WHERE id = 1)`, last-int64(scheduler.DefaultLeaseTimeout), st.at)
		if err != nil {
			return err
		}
		defer rows.Close()
		st.waiting = map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			st.waiting[id] = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	s.start.Store(st)
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

// noteReport records that node id renewed its lease, which every lease, report
// and registration does: when it is one of the workers that were waiting at
// the start, the grace holds no one past its end from then on. A renewal
// whose transaction fails later counts too: the worker reached this server.
func (s *Store) noteReport(id string) {
	if st := s.start.Load(); st != nil && st.waiting[id] && !st.reported.Load() {
		st.reported.Store(true)
	}
}

// waitWindow returns the leases of the nodes that are waiting at now: a
// node whose last lease is at least from and before before. Outside the
// start grace, and past it unless it holds, no lease is in it.
func (s *Store) waitWindow(now time.Time) (from, before int64) {
	st := s.start.Load()
	n := now.UnixNano()
	if st == nil || st.last == 0 || n < st.at || (n >= st.until && !st.held()) {
		return 1, 0
	}
	return st.last - int64(scheduler.DefaultLeaseTimeout), st.at
}

// Waiting reports whether n is waiting for its first report at now: n has
// not renewed its lease since serve started, n was up when the server went
// down, and the start grace has not ended, or holds (StartWait). A waiting
// node is not lost.
func (s *Store) Waiting(n Node, now time.Time) bool {
	from, before := s.waitWindow(now)
	l := n.LastLease.UnixNano()
	return l >= from && l < before
}

// StartWait is the start grace as it stands.
type StartWait struct {
	// GraceEnds is when the start grace ends, or ended.
	GraceEnds time.Time
	// Held is true once the grace has ended with no report since the start
	// from any worker that was waiting: they go on waiting, and none is
	// lost, until one of them reports or is removed.
	Held bool
}

// StartWait returns the start grace at now. ok is false before SetStarted,
// and when no worker was waiting at the start.
func (s *Store) StartWait(now time.Time) (w StartWait, ok bool) {
	st := s.start.Load()
	if st == nil || len(st.waiting) == 0 {
		return StartWait{}, false
	}
	return StartWait{GraceEnds: time.Unix(0, st.until), Held: now.UnixNano() >= st.until && st.held()}, true
}
