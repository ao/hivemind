package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Fewer Raft entries (docs/ha.md#fewer-raft-entries). On the leader of a
// cluster of several, a lease renewal, or a report that changes nothing
// but the time, from a node whose stored lease is no older than KeepWithin
// and which was not removed, writes nothing: the leader keeps the time in
// memory, for the lease and for the container rows' stamps, and writes
// what it keeps for every node in one transaction every WriteKeptEvery
// (WriteKept). A live node's stored lease is then never older than
// KeepWithin + WriteKeptEvery, 13 s, inside the 15 s lease, so no reader of
// the stored leases sees it lapse; one that stops is lost 15 s after its
// last renewal, as before, since the write after that renewal records it.
// A cluster of one has no log, and writes every renewal, as before.

// Leases kept in memory.
const (
	// KeepWithin is how recent a node's stored lease must be for the
	// leader to keep a renewal in memory rather than write it.
	KeepWithin = 9 * time.Second
	// WriteKeptEvery is how often the leader writes what it keeps.
	WriteKeptEvery = 4 * time.Second
)

// keepable reports whether a renewal of node id at now may be kept in
// memory: the store replicates, this server can write, the node was not
// removed, and q has its stored lease no older than KeepWithin.
func (s *Store) keepable(ctx context.Context, q rowQuerier, id string, now int64) bool {
	r := s.repl.Load()
	if r == nil {
		return false
	}
	if _, err := r.CanWrite(); err != nil {
		return false
	}
	if removed, _ := s.IsNodeRemoved(ctx, id); removed {
		return false
	}
	var stored int64
	if err := q.QueryRowContext(ctx, `SELECT last_lease FROM nodes WHERE id = ?`, id).Scan(&stored); err != nil {
		return false
	}
	return stored <= now && now-stored <= int64(KeepWithin)
}

// keep records in memory that node id renewed its lease at now, and, with
// stamped, that its report changed nothing.
func (s *Store) keep(id string, now int64, stamped bool) {
	s.keptMu.Lock()
	if s.kept == nil {
		s.kept, s.stamp = map[string]int64{}, map[string]int64{}
	}
	s.kept[id] = max(s.kept[id], now)
	if stamped {
		s.stamp[id] = max(s.stamp[id], now)
	}
	s.keptMu.Unlock()
	s.noteLease(id, now)
}

// forgetKept drops what the leader keeps, as when it stops leading: the
// next leader judges the leases by what is stored, with the start grace.
func (s *Store) forgetKept() {
	s.keptMu.Lock()
	s.kept, s.stamp = nil, nil
	s.keptMu.Unlock()
}

// Kept is how many nodes' leases the leader keeps in memory now.
func (s *Store) Kept() int {
	s.keptMu.Lock()
	defer s.keptMu.Unlock()
	return len(s.kept)
}

// WriteKept writes the leases and stamps the leader keeps, for every node,
// in one transaction, and returns how many nodes' it wrote. The leader
// calls it every WriteKeptEvery, and before it hands the leadership on. A
// write that fails because this server no longer leads drops them: the
// next leader has its own; one that fails otherwise keeps them for the
// next try.
func (s *Store) WriteKept(ctx context.Context) (int, error) {
	s.keptMu.Lock()
	kept, stamp := s.kept, s.stamp
	s.kept, s.stamp = nil, nil
	s.keptMu.Unlock()
	if len(kept) == 0 && len(stamp) == 0 {
		return 0, nil
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// The leases were current when they were kept, and are written
		// later: the routing table, which counts the nodes with a current
		// lease, keeps them.
		keepsRoutes(tx)
		for id, t := range kept {
			if _, err := tx.ExecContext(ctx, `UPDATE nodes SET last_lease = ? WHERE id = ? AND last_lease < ?`, t, id, t); err != nil {
				return err
			}
		}
		for id, t := range stamp {
			if _, err := tx.ExecContext(ctx, `UPDATE container_status SET updated_at = ? WHERE node_id = ? AND updated_at < ?`, t, id, t); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, ErrNotLeader) {
			s.keptMu.Lock()
			if s.kept == nil {
				s.kept, s.stamp = map[string]int64{}, map[string]int64{}
			}
			for id, t := range kept {
				s.kept[id] = max(s.kept[id], t)
			}
			for id, t := range stamp {
				s.stamp[id] = max(s.stamp[id], t)
			}
			s.keptMu.Unlock()
		}
		return 0, err
	}
	return len(kept), nil
}

// withKept gives each node its kept lease, when it is later than the
// stored one: what the leader's own views show (ListNodes, GetNode).
func (s *Store) withKept(nodes []Node) []Node {
	s.keptMu.Lock()
	defer s.keptMu.Unlock()
	if len(s.kept) == 0 {
		return nodes
	}
	for i := range nodes {
		if t := s.kept[nodes[i].ID]; t > nodes[i].LastLease.UnixNano() {
			nodes[i].LastLease = time.Unix(0, t)
		}
	}
	return nodes
}

// statusWithKept gives each container row its node's kept stamp, when it
// is later than the stored one.
func (s *Store) statusWithKept(st []Status) []Status {
	s.keptMu.Lock()
	defer s.keptMu.Unlock()
	if len(s.stamp) == 0 {
		return st
	}
	for i := range st {
		if t := s.stamp[st[i].NodeID]; t > st[i].UpdatedAt.UnixNano() {
			st[i].UpdatedAt = time.Unix(0, t)
		}
	}
	return st
}
