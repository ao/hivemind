package store

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"github.com/ao/hivemind/internal/scheduler"
)

// The routing table, kept between reads.
//
// Every router reads the table once a second, so ten nodes read it ten times
// a second, and each read ran two statements, one a join of every routed
// container's status with its node and its replica: in the second scale run,
// a quarter of the server's CPU, with nothing changed between most of them.
// The store keeps the last table it read, with the version it read it at,
// and answers from it while the version is the same and the table cannot
// have changed with time alone.
//
// The version goes up after every write transaction that changed a row
// (Store.tx), so a change of any kind makes the next read read again. Two
// writes that run every few seconds per node and leave the table as it is
// say so, and do not raise it: a lease renewal of a node whose lease was
// current, and a node's report that is the same as its last, whose rows the
// store only stamps, when it placed nothing. The table also changes with
// time alone, when a node's lease lapses, so a kept table is good only until
// the first lease it counts would lapse; and while nodes wait for their
// first report after serve started (start.go), it is read every time.

// routeCache is a routing table as read at version ver, good until until
// (Unix nanoseconds) at that version.
type routeCache struct {
	ver   uint64
	until int64
	table []Route
}

// routesChanged raises the version of the routing table, after a write that
// may have changed it.
func (s *Store) routesChanged() { s.routeVer.Add(1) }

// cachedRoutes returns the kept table, if it is good at version ver and time
// now.
func (s *Store) cachedRoutes(ver uint64, now int64) ([]Route, bool) {
	s.routeMu.Lock()
	c := s.routeCache
	s.routeMu.Unlock()
	if c == nil || c.ver != ver || now >= c.until {
		return nil, false
	}
	return cloneRoutes(c.table), true
}

// keepRoutes keeps t, read at version ver, until until, unless a table read
// at a later version is kept already.
func (s *Store) keepRoutes(ver uint64, until int64, t []Route) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if c := s.routeCache; c != nil && c.ver > ver {
		return
	}
	s.routeCache = &routeCache{ver: ver, until: until, table: cloneRoutes(t)}
}

// cloneRoutes copies t and each route's endpoints, so a caller cannot change
// the kept table.
func cloneRoutes(t []Route) []Route {
	if t == nil {
		return nil
	}
	out := slices.Clone(t)
	for i := range out {
		out[i].Endpoints = slices.Clone(out[i].Endpoints)
	}
	return out
}

// routesUntil returns when the first current lease of a node with an
// address lapses, as of now: the first time at which the table can change
// with no write. With no such node it is far off.
func routesUntil(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	var first sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT min(last_lease) FROM nodes WHERE address != '' AND last_lease >= ?`,
		now.UnixNano()-int64(scheduler.DefaultLeaseTimeout)).Scan(&first); err != nil {
		return 0, err
	}
	if !first.Valid {
		return now.Add(time.Hour).UnixNano(), nil
	}
	// A lease of exactly the cutoff still counts (last_lease >= cutoff).
	return first.Int64 + int64(scheduler.DefaultLeaseTimeout) + 1, nil
}

// leaseCurrent reports whether node id's lease, as this process last
// committed it, is current at now: the table counts the node, and a renewal
// leaves it as it is. A node whose lease this process has not seen may have
// lapsed.
func (s *Store) leaseCurrent(id string, now int64) bool {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	l, ok := s.leases[id]
	return ok && l >= now-int64(scheduler.DefaultLeaseTimeout)
}

// noteLease records that node id's lease was renewed at now, once that
// renewal has committed.
func (s *Store) noteLease(id string, now int64) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if s.leases == nil {
		s.leases = map[string]int64{}
	}
	s.leases[id] = max(s.leases[id], now)
}

// forgetLease drops what this process knows of node id's lease, as when the
// node is removed.
func (s *Store) forgetLease(id string) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	delete(s.leases, id)
}

// onCommit runs f once tx, a transaction of Store.tx, has committed.
func onCommit(tx *sql.Tx, f func()) {
	if c := cacheOf(tx); c != nil {
		c.onCommit = append(c.onCommit, f)
	}
}

// keepsRoutes says that tx, a transaction of Store.tx, leaves the routing
// table as it is, whatever rows it changes.
func keepsRoutes(tx *sql.Tx) {
	if c := cacheOf(tx); c != nil {
		c.routesKept = true
	}
}

// totalChanges is the count of rows the connection of tx has inserted,
// updated or deleted since it was opened. The statement is prepared once,
// on the one write connection, when the store opens.
func (s *Store) totalChanges(ctx context.Context, tx *sql.Tx) (int64, error) {
	var n int64
	err := tx.StmtContext(ctx, s.changes).QueryRowContext(ctx).Scan(&n)
	return n, err
}

// The denylist of removed nodes, kept in memory. The node API checks it on
// every request, and that read, a statement compiled each time, was 6% of
// the server's CPU in the second scale run. Removal is rare, and adds to the
// list only, so the store reads the list once when it opens, and adds to it
// when a removal commits.

// loadRemoved reads the denylist into memory.
func (s *Store) loadRemoved(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM removed_nodes`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		m[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.removedMu.Lock()
	s.removed = m
	s.removedMu.Unlock()
	return nil
}

func (s *Store) noteRemoved(id string) {
	s.removedMu.Lock()
	defer s.removedMu.Unlock()
	if s.removed == nil {
		s.removed = map[string]bool{}
	}
	s.removed[id] = true
}
