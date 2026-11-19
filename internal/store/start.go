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
// copies. The cost of the hold: a cluster whose every worker died while
// the server was down keeps their replicas unplaced until an operator
// removes the nodes (hivemind node remove).
//
// The end of the hold. Once one of the waiting workers reports, the hold
// ends, and the others that have not reported get the start grace again,
// from that report: they are waiting until then, as in the first grace,
// and are lost only if they have not reported by its end. A partition
// that heals heals for the workers at different moments, and an agent
// that cannot reach the server backs off to one pass every 30 s. In the
// fifth soak, when the hold ended at once for every worker, a worker that
// reached the server 0.52 s after the first one was lost, and one 12 s
// behind had its six replicas placed again on the server's machine, where
// they stayed. The start grace again, rather than the lease timeout or a
// fixed time: the lease timeout is shorter than that backoff, and the
// reasons a worker is late after the hold are those it is late after any
// start (a resolver with the old record, a partition), for which the
// operator sized --start-grace.

// DefaultStartGrace is the start grace of a serve that is given none.
const DefaultStartGrace = 2 * time.Minute

// startState is serve's start, as SetStarted recorded it. at, until, grace
// and last are Unix nanoseconds.
type startState struct {
	at    int64 // when serve started
	until int64 // when the start grace ends
	grace int64 // the start grace
	last  int64 // the latest lease a worker renewed before at; 0 when none had
	// waiting holds the workers that were waiting at the start, the
	// server's own node left out.
	waiting map[string]bool
	// reported is when one of them first renewed its lease since at
	// (noteReport); 0 until then. Until then the grace holds past until.
	reported atomic.Int64
}

// held reports whether the start holds the waiting workers past the grace:
// some were waiting, and none of them has reported.
func (st *startState) held() bool { return len(st.waiting) > 0 && st.reported.Load() == 0 }

// holdEnded returns when the hold ended: the first report of a waiting
// worker, when it came once the grace had ended. 0 while the grace lasts or
// holds, and when the first report came in the grace.
func (st *startState) holdEnded() int64 {
	if r := st.reported.Load(); r != 0 && r >= st.until {
		return r
	}
	return 0
}

// waitEnds returns when the workers that still wait stop waiting, and are
// lost: the end of the grace, or, once the hold has ended, a grace after
// that. While the grace holds it is the end of the grace, but they wait on
// (held).
func (st *startState) waitEnds() int64 {
	if h := st.holdEnded(); h != 0 {
		return h + st.grace
	}
	return st.until
}

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
	st := &startState{at: t.UnixNano(), until: t.Add(grace).UnixNano(), grace: int64(grace), last: last}
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

// noteReport records that node id renewed its lease at now (Unix
// nanoseconds), which every lease, report and registration does: when it is
// the first of the workers that were waiting at the start to do so, the
// grace holds no one past its end from then on, and if the grace has ended
// already, the hold ends, and the others wait a grace from now. A renewal
// whose transaction fails later counts too: the worker reached this server.
// It is called before the renewal commits, so the routing table, which
// reads the committed leases, can see the hold ended and the old lease at
// once; the others' window, opened rather than closed, keeps the renewing
// worker in it until then. In the fifth soak the hold closed that window,
// and a router read a table with neither the first worker's endpoints nor
// the others'.
func (s *Store) noteReport(id string, now int64) {
	if st := s.start.Load(); st != nil && st.waiting[id] {
		st.reported.CompareAndSwap(0, max(now, 1))
	}
}

// waitWindow returns the leases of the nodes that are waiting at now: a
// node whose last lease is at least from and before before. Outside the
// start grace, and past it unless it holds or a grace from the hold's end
// lasts, no lease is in it.
func (s *Store) waitWindow(now time.Time) (from, before int64) {
	st := s.start.Load()
	n := now.UnixNano()
	if st == nil || st.last == 0 || n < st.at || (!st.held() && n >= st.waitEnds()) {
		return 1, 0
	}
	return st.last - int64(scheduler.DefaultLeaseTimeout), st.at
}

// Waiting reports whether n is waiting for its first report at now: n has
// not renewed its lease since serve started, n was up when the server went
// down, and the start grace has not ended, or holds, or the grace that
// follows the end of a hold has not ended (StartWait). A waiting node is
// not lost.
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
	// HoldEnded is when the hold ended: when the first of the workers that
	// were waiting reported, after the grace had ended. Zero while the
	// grace lasts or holds, and when the first report came in the grace.
	HoldEnded time.Time
	// WaitEnds is when the workers that still wait are lost: GraceEnds, or,
	// once the hold has ended, HoldEnded and the start grace again. Zero
	// while the grace holds.
	WaitEnds time.Time
}

// StartWait returns the start grace at now. ok is false before SetStarted,
// and when no worker was waiting at the start.
func (s *Store) StartWait(now time.Time) (w StartWait, ok bool) {
	st := s.start.Load()
	if st == nil || len(st.waiting) == 0 {
		return StartWait{}, false
	}
	w = StartWait{GraceEnds: time.Unix(0, st.until), Held: now.UnixNano() >= st.until && st.held()}
	if h := st.holdEnded(); h != 0 {
		w.HoldEnded = time.Unix(0, h)
	}
	if !w.Held {
		w.WaitEnds = time.Unix(0, st.waitEnds())
	}
	return w, true
}
