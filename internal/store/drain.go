package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Eligibility and drains.
//
// An ineligible node takes no new replicas and keeps the ones it runs:
// placement passes it over, and so does the choice of a node for a surge. The
// flag is a column of the node's row, so it survives a restart of the server,
// and a node keeps it when its lease lapses and comes back. The model is
// Nomad's node eligibility and node drain.
//
// A drain makes the node ineligible, then moves its replicas off it with
// surges, the mechanism rollouts use (see rollout.go): a copy of the replica,
// at the replica's own generation, starts on another eligible node, and the
// replica moves there once that copy reports Ready. The drained node's agent
// then removes the old copy, because the replica is no longer assigned to it.
// A drain runs one move per app at a time, and the moves of different apps
// at once. So while another node has room, no app has fewer Ready copies than
// its replica count during a drain.
//
// A replica that cannot move stays, and the drain records why: it has a
// volume, whose data is on this node; no other eligible node has its host
// ports free, or room for it; its app has a rollout in progress, which moves
// its replicas itself; or its copy elsewhere crash-looped or was not Ready
// within the rollout deadline. A replica whose copy failed is not tried
// again before the drain's deadline. The others are tried again at each step.
//
// At the deadline, a move still in flight ends at once: the replica moves to
// the node where its copy is starting. Every other replica on the node that
// has no volume loses the node and is placed again, as on node loss, and the
// node's agent stops its container. A replica with a volume stays, and keeps
// running. The drain is then done. Before the deadline, the drain is done when
// only replicas with a volume are left on the node, and the node no longer
// reports a container of a replica that has left it.
//
// A node that is lost during its drain gives up its replicas as on any node
// loss (releaseNode): the ones without a volume are placed again at once. The
// drain counts them as lost, apart from those it moved or stopped, and
// records when the node was lost.
//
// A finished drain's record stays, and the node stays ineligible, until it is
// made eligible again or removed. The server advances drains with
// AdvanceDrains, on its sweep and after each pass of its own agent. All of
// the state is in the store, so a drain carries on after a restart.

// DefaultDrainDeadline is the deadline of a drain started without one.
const DefaultDrainDeadline = 10 * time.Minute

// MaxDrainDeadline is the longest deadline a drain may have.
const MaxDrainDeadline = 7 * 24 * time.Hour

// DrainState is where a drain is.
type DrainState string

const (
	// DrainDraining means replicas are still being moved off the node.
	DrainDraining DrainState = "draining"
	// DrainDone means the drain finished, before or at its deadline.
	DrainDone DrainState = "done"
)

// Drain is the drain of one node.
type Drain struct {
	NodeID    string
	State     DrainState
	StartedAt time.Time
	Deadline  time.Time
	// FinishedAt is when the drain finished. It is zero while it runs.
	FinishedAt time.Time
	// Moved counts the replicas that moved to another node.
	Moved int
	// Stopped counts the replicas that lost the node at the deadline, to be
	// placed again.
	Stopped int
	// Lost counts the replicas that lost the node because the node itself
	// was lost during the drain (its lease lapsed), to be placed again.
	// LostAt is when the node was first lost during the drain, and zero if
	// it was not.
	Lost   int
	LostAt time.Time
	// Remaining counts the replicas still placed on the node that should run.
	Remaining int
	// OldContainers counts the containers the node still reports that no
	// replica on it should run: old copies its agent has yet to remove.
	OldContainers int
	// Moving lists the moves in flight, by app.
	Moving []DrainMove
	// Blocked lists the replicas on the node that did not move at the
	// drain's last step, and why, by app and index.
	Blocked []DrainBlocked
}

// DrainMove is a move in flight: a copy of the replica is starting on To.
type DrainMove struct {
	App   string
	Index int
	To    string
	Since time.Time
}

// DrainBlocked is a replica that stays on the drained node, and why.
type DrainBlocked struct {
	App    string
	Index  int
	Reason string
}

// Eligibility is what SetNodeEligibility did.
type Eligibility struct {
	ID       string
	Name     string
	Eligible bool
	// DrainCancelled is true when the node was draining. Making a node
	// eligible ends its drain.
	DrainCancelled bool
}

// dbQuerier is a *sql.DB or a *sql.Tx.
type dbQuerier interface {
	querier
	rowQuerier
}

// drainMoveRow is one row of drain_moves.
type drainMoveRow struct {
	app       string
	idx       int
	surgeNode string
	gen       int
	since     int64
}

// SetNodeEligibility makes the node eligible or ineligible for new replicas.
// Its replicas stay where they are either way, and so does a surge already
// placed on it. Making a node eligible also deletes its drain record: a drain
// in progress is cancelled, the surges of its moves in flight are removed,
// and the replicas stay on the node. An unknown or removed node is
// ErrNotFound.
func (s *Store) SetNodeEligibility(ctx context.Context, id string, eligible bool) (Eligibility, error) {
	var out Eligibility
	err := s.tx(ctx, func(tx *sql.Tx) error {
		name, err := nodeName(ctx, tx, id)
		if err != nil {
			return err
		}
		out = Eligibility{ID: id, Name: name, Eligible: eligible}
		flag := 0
		if eligible {
			flag = 1
			var state string
			switch err := tx.QueryRowContext(ctx, `SELECT state FROM drains WHERE node_id = ?`, id).Scan(&state); {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return err
			default:
				out.DrainCancelled = state == string(DrainDraining)
				if err := s.dropDrainMoves(ctx, tx, id); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM drains WHERE node_id = ?`, id); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE nodes SET eligible = ? WHERE id = ?`, flag, id); err != nil {
			return err
		}
		return s.placePending(ctx, tx)
	})
	return out, err
}

// DrainNode makes the node ineligible, starts a drain of it that ends at
// deadline from now, and takes the drain's first step, so the reply shows the
// first moves and what cannot move. On a node that is already draining, a
// deadline above zero replaces the drain's deadline, and zero keeps it. A new
// drain with a deadline of zero gets DefaultDrainDeadline. A finished drain
// is replaced by a new one. An unknown or removed node is ErrNotFound.
func (s *Store) DrainNode(ctx context.Context, id string, deadline time.Duration) (Drain, error) {
	if deadline < 0 || deadline > MaxDrainDeadline {
		return Drain{}, fmt.Errorf("%w: a drain deadline must be above zero and at most %s, got %s", ErrInvalid, MaxDrainDeadline, deadline)
	}
	var out Drain
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := nodeName(ctx, tx, id); err != nil {
			return err
		}
		now := s.now().UnixNano()
		if _, err := tx.ExecContext(ctx, `UPDATE nodes SET eligible = 0 WHERE id = ?`, id); err != nil {
			return err
		}
		var state string
		err := tx.QueryRowContext(ctx, `SELECT state FROM drains WHERE node_id = ?`, id).Scan(&state)
		switch {
		case err == nil && state == string(DrainDraining):
			if deadline > 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE drains SET deadline = ?, updated_at = ? WHERE node_id = ?`,
					now+int64(deadline), now, id); err != nil {
					return err
				}
			}
		case err == nil || errors.Is(err, sql.ErrNoRows):
			if deadline == 0 {
				deadline = DefaultDrainDeadline
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM drains WHERE node_id = ?`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO drains (node_id, state, started_at, deadline, updated_at)
				VALUES (?, 'draining', ?, ?, ?)`, id, now, now+int64(deadline), now); err != nil {
				return err
			}
		default:
			return err
		}
		if _, err := s.advanceDrain(ctx, tx, id, time.Time{}); err != nil {
			return err
		}
		out, err = getDrain(ctx, tx, id)
		return err
	})
	return out, err
}

// GetDrain returns the node's drain, in progress or finished. A node without
// one is ErrNotFound.
func (s *Store) GetDrain(ctx context.Context, id string) (Drain, error) {
	return getDrain(ctx, s.db, id)
}

// ListDrains returns every drain, in progress or finished, sorted by node.
func (s *Store) ListDrains(ctx context.Context) ([]Drain, error) {
	ids, err := drainIDs(ctx, s.db, `1`)
	if err != nil {
		return nil, err
	}
	out := make([]Drain, 0, len(ids))
	for _, id := range ids {
		d, err := getDrain(ctx, s.db, id)
		if errors.Is(err, ErrNotFound) {
			// Removed, or made eligible, since the list was read.
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// AdvanceDrains takes one step of every drain in progress: it checks each
// move in flight, starts the next moves, and finishes a drain that is done or
// past its deadline. It returns the drains that finished in this call, so the
// caller can log them. It is safe to call at any time, as often as wanted.
//
// since floors the start of each move, as in ReconcileNodes. serve passes the
// time it started, so a copy that was starting while the server was down gets
// a whole rollout deadline from the restart to report Ready. The zero time
// means no floor. The drain's own deadline has no floor: it is a time of day.
func (s *Store) AdvanceDrains(ctx context.Context, since time.Time) ([]Drain, error) {
	var finished []Drain
	err := s.tx(ctx, func(tx *sql.Tx) error {
		finished = nil
		ids, err := drainIDs(ctx, tx, `state = 'draining'`)
		if err != nil {
			return err
		}
		for _, id := range ids {
			done, err := s.advanceDrain(ctx, tx, id, since)
			if err != nil {
				return fmt.Errorf("drain of %s: %w", id, err)
			}
			if done {
				d, err := getDrain(ctx, tx, id)
				if err != nil {
					return err
				}
				finished = append(finished, d)
			}
		}
		return nil
	})
	return finished, err
}

// advanceDrain takes one step of the node's drain. It returns true when the
// drain finished in this step. A drain that is not in progress is left alone.
func (s *Store) advanceDrain(ctx context.Context, tx *sql.Tx, node string, since time.Time) (bool, error) {
	var deadline int64
	err := tx.QueryRowContext(ctx, `SELECT deadline FROM drains WHERE node_id = ? AND state = 'draining'`, node).Scan(&deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	moves, err := loadDrainMoves(ctx, tx, node)
	if err != nil {
		return false, err
	}
	for _, m := range moves {
		if err := s.checkDrainMove(ctx, tx, node, m, since); err != nil {
			return false, err
		}
	}
	if s.now().UnixNano() >= deadline {
		return true, s.drainDeadline(ctx, tx, node)
	}
	if err := s.startDrainMoves(ctx, tx, node); err != nil {
		return false, err
	}
	done, err := drainEmptied(ctx, tx, node)
	if err != nil || !done {
		return false, err
	}
	return true, s.finishDrain(ctx, tx, node)
}

// liveDrainMove reports whether a move still belongs to the drain of node:
// the replica should run, is still on node at the move's generation, and its
// surge is the one the move placed. It also returns the replica, if it
// exists, and whether the move's surge is still there.
func liveDrainMove(ctx context.Context, tx *sql.Tx, node string, m drainMoveRow) (rep Assignment, live, ours bool, err error) {
	reps, err := queryAssignments(ctx, tx, `r.app = ? AND r.idx = ?`, m.app, m.idx)
	if err != nil {
		return Assignment{}, false, false, err
	}
	surge, hasSurge, err := surgeOf(ctx, tx, m.app, m.idx)
	if err != nil {
		return Assignment{}, false, false, err
	}
	ours = hasSurge && surge.NodeID == m.surgeNode && surge.Generation == m.gen
	if len(reps) == 0 {
		return Assignment{}, false, ours, nil
	}
	rep = reps[0]
	live = ours && rep.Desired == Running && rep.NodeID == node && rep.Generation == m.gen
	return rep, live, ours, nil
}

// checkDrainMove looks at one move in flight. A copy that is Ready takes the
// replica, a copy that failed leaves it blocked until the deadline, and a
// move that something else overtook is dropped.
func (s *Store) checkDrainMove(ctx context.Context, tx *sql.Tx, node string, m drainMoveRow, since time.Time) error {
	rep, live, ours, err := liveDrainMove(ctx, tx, node, m)
	if err != nil {
		return err
	}
	rolling, err := rolloutInProgress(ctx, tx, m.app)
	if err != nil {
		return err
	}
	if !live || rolling {
		// The replica was scaled away, deleted, released by node loss, or
		// changed by a rollout, or the surge went with its node. A surge the
		// drain still owns goes too. Where the replica now runs on the surge's
		// node, the surge is the same container, so only its row goes.
		if ours {
			if rep.Desired == Running && rep.NodeID == m.surgeNode && rep.Generation == m.gen {
				if _, err := tx.ExecContext(ctx, `DELETE FROM surges WHERE app = ? AND idx = ?`, m.app, m.idx); err != nil {
					return err
				}
			} else if err := s.dropSurge(ctx, tx, m.app, m.idx); err != nil {
				return err
			}
		}
		return deleteDrainMove(ctx, tx, node, m.app)
	}
	ready, crash, err := containerState(ctx, tx, m.surgeNode, m.app, m.idx, m.gen)
	if err != nil {
		return err
	}
	switch {
	case ready:
		return s.completeDrainMove(ctx, tx, node, m)
	case crash:
		return s.failDrainMove(ctx, tx, node, m, fmt.Sprintf("its copy on node %s was crash-looping; it stays until the deadline", m.surgeNode))
	}
	start := m.since
	if !since.IsZero() {
		start = max(start, since.UnixNano())
	}
	limit := RolloutDeadline(rep.Spec)
	if s.now().UnixNano()-start >= int64(limit) {
		return s.failDrainMove(ctx, tx, node, m, fmt.Sprintf("its copy on node %s was not ready after %s; it stays until the deadline", m.surgeNode, limit))
	}
	return nil
}

// completeDrainMove moves the replica to its surge's node, and counts it.
func (s *Store) completeDrainMove(ctx context.Context, tx *sql.Tx, node string, m drainMoveRow) error {
	if err := s.promoteSurge(ctx, tx, m.app, m.idx, m.surgeNode, m.gen); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drains SET moved = moved + 1, updated_at = ? WHERE node_id = ?`,
		s.now().UnixNano(), node); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM drain_blocked WHERE node_id = ? AND app = ? AND idx = ?`, node, m.app, m.idx); err != nil {
		return err
	}
	return deleteDrainMove(ctx, tx, node, m.app)
}

// failDrainMove removes the move's surge and marks its replica as failed,
// with reason, so it is not tried again before the deadline.
func (s *Store) failDrainMove(ctx context.Context, tx *sql.Tx, node string, m drainMoveRow, reason string) error {
	if err := s.dropSurge(ctx, tx, m.app, m.idx); err != nil {
		return err
	}
	if err := blockReplica(ctx, tx, node, m.app, m.idx, reason, true); err != nil {
		return err
	}
	return deleteDrainMove(ctx, tx, node, m.app)
}

// startDrainMoves starts a move for each app on the node that has none in
// flight, and records why each other replica on the node does not move.
func (s *Store) startDrainMoves(ctx context.Context, tx *sql.Tx, node string) error {
	reps, err := queryAssignments(ctx, tx, `r.node_id = ? AND r.desired = 'running'`, node)
	if err != nil {
		return err
	}
	moves, err := loadDrainMoves(ctx, tx, node)
	if err != nil {
		return err
	}
	moving := map[string]bool{}
	for _, m := range moves {
		moving[m.app] = true
	}
	type key struct {
		app string
		idx int
	}
	failed := map[key]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT app, idx FROM drain_blocked WHERE node_id = ? AND failed = 1`, node)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.app, &k.idx); err != nil {
			rows.Close()
			return err
		}
		failed[k] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `DELETE FROM drain_blocked WHERE node_id = ? AND failed = 0`, node); err != nil {
		return err
	}
	// Surges count against their nodes, so the moves of two apps do not both
	// take the last room on one node.
	placed, err := placedOn(ctx, tx)
	if err != nil {
		return err
	}
	for _, rep := range reps {
		if moving[rep.App] || failed[key{rep.App, rep.Index}] {
			continue
		}
		var reason string
		rolling, err := rolloutInProgress(ctx, tx, rep.App)
		if err != nil {
			return err
		}
		_, surged, err := surgeOf(ctx, tx, rep.App, rep.Index)
		if err != nil {
			return err
		}
		switch {
		case rolling:
			reason = "its app has a rollout in progress; the drain moves it after the rollout, unless the rollout moves it first"
		case surged:
			reason = "it has a copy starting on another node"
		default:
			to, why, err := s.surgeNode(ctx, tx, rep.Spec, rep, placed, false)
			if err != nil {
				return err
			}
			if to == "" {
				reason = why
				break
			}
			if err := s.placeSurge(ctx, tx, rep.App, rep.Index, to, rep.Generation); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO drain_moves (node_id, app, idx, surge_node, generation, since)
				VALUES (?, ?, ?, ?, ?, ?)`, node, rep.App, rep.Index, to, rep.Generation, s.now().UnixNano()); err != nil {
				return err
			}
			moving[rep.App] = true
			copyOn := rep
			copyOn.NodeID = to
			placed = append(placed, copyOn)
			continue
		}
		if err := blockReplica(ctx, tx, node, rep.App, rep.Index, reason, false); err != nil {
			return err
		}
	}
	return nil
}

// drainDeadline ends the node's drain at its deadline. A move in flight ends
// with the replica on its surge's node, where its copy is starting. Every
// other replica on the node without a volume loses the node and is placed
// again, as on node loss. A replica with a volume stays.
func (s *Store) drainDeadline(ctx context.Context, tx *sql.Tx, node string) error {
	moves, err := loadDrainMoves(ctx, tx, node)
	if err != nil {
		return err
	}
	for _, m := range moves {
		if err := s.completeDrainMove(ctx, tx, node, m); err != nil {
			return err
		}
	}
	reps, err := queryAssignments(ctx, tx, `r.node_id = ? AND r.desired = 'running'`, node)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM drain_blocked WHERE node_id = ?`, node); err != nil {
		return err
	}
	now := s.now().UnixNano()
	stopped := 0
	for _, r := range reps {
		if len(r.Spec.Volumes) > 0 {
			if err := blockReplica(ctx, tx, node, r.App, r.Index, "it has a volume, and its data is on this node; it keeps running here", false); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE replicas SET node_id = NULL, pending_reason = '', updated_at = ? WHERE app = ? AND idx = ?`,
			now, r.App, r.Index); err != nil {
			return err
		}
		stopped++
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drains SET stopped = stopped + ?, updated_at = ? WHERE node_id = ?`, stopped, now, node); err != nil {
		return err
	}
	if err := s.finishDrain(ctx, tx, node); err != nil {
		return err
	}
	return s.placePending(ctx, tx)
}

// finishDrain marks the node's drain done. Its blocked rows stay, as the
// reasons for what is left on the node.
func (s *Store) finishDrain(ctx context.Context, tx *sql.Tx, node string) error {
	now := s.now().UnixNano()
	_, err := tx.ExecContext(ctx, `UPDATE drains SET state = 'done', finished_at = ?, updated_at = ? WHERE node_id = ?`, now, now, node)
	return err
}

// drainEmptied reports whether the drain of node has nothing left to do: no
// move in flight, no replica without a volume on the node, and no container
// on the node that no replica there should run.
func drainEmptied(ctx context.Context, tx *sql.Tx, node string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM drain_moves WHERE node_id = ?`, node).Scan(&n); err != nil || n > 0 {
		return false, err
	}
	reps, err := queryAssignments(ctx, tx, `r.node_id = ? AND r.desired = 'running'`, node)
	if err != nil {
		return false, err
	}
	for _, r := range reps {
		if len(r.Spec.Volumes) == 0 {
			return false, nil
		}
	}
	old, err := oldContainers(ctx, tx, node)
	return old == 0, err
}

// oldContainers counts the containers the node reports that no replica on
// it should run.
func oldContainers(ctx context.Context, q rowQuerier, node string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM container_status c WHERE c.node_id = ?
		AND NOT EXISTS (SELECT 1 FROM replicas r WHERE r.node_id = c.node_id AND r.app = c.app AND r.idx = c.idx AND r.desired = 'running')`,
		node).Scan(&n)
	return n, err
}

// dropDrainMoves removes the surges of the node's moves in flight that the
// drain still owns, and the moves. The replicas stay on the node.
func (s *Store) dropDrainMoves(ctx context.Context, tx *sql.Tx, node string) error {
	moves, err := loadDrainMoves(ctx, tx, node)
	if err != nil {
		return err
	}
	for _, m := range moves {
		if _, _, ours, err := liveDrainMove(ctx, tx, node, m); err != nil {
			return err
		} else if ours {
			if err := s.dropSurge(ctx, tx, m.app, m.idx); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM drain_moves WHERE node_id = ?`, node)
	return err
}

// promoteDrainMoves ends the node's moves in flight with each replica on its
// surge's node, for a removal of the node. It returns how many moved.
func (s *Store) promoteDrainMoves(ctx context.Context, tx *sql.Tx, node string) (int, error) {
	moves, err := loadDrainMoves(ctx, tx, node)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range moves {
		_, live, _, err := liveDrainMove(ctx, tx, node, m)
		if err != nil {
			return 0, err
		}
		if live {
			if err := s.promoteSurge(ctx, tx, m.app, m.idx, m.surgeNode, m.gen); err != nil {
				return 0, err
			}
			n++
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM drain_moves WHERE node_id = ?`, node)
	return n, err
}

// drainedEmpty reports whether the node is ineligible and runs nothing: no
// replica or surge is placed on it, and it reports no container.
func drainedEmpty(ctx context.Context, tx *sql.Tx, node string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM nodes WHERE id = ?1 AND eligible = 1) +
		(SELECT count(*) FROM replicas WHERE node_id = ?1) +
		(SELECT count(*) FROM surges WHERE node_id = ?1) +
		(SELECT count(*) FROM container_status WHERE node_id = ?1)`, node).Scan(&n)
	return n == 0, err
}

func blockReplica(ctx context.Context, tx *sql.Tx, node, app string, idx int, reason string, failed bool) error {
	f := 0
	if failed {
		f = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO drain_blocked (node_id, app, idx, reason, failed) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (node_id, app, idx) DO UPDATE SET reason = excluded.reason, failed = excluded.failed`,
		node, app, idx, reason, f)
	return err
}

func deleteDrainMove(ctx context.Context, tx *sql.Tx, node, app string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM drain_moves WHERE node_id = ? AND app = ?`, node, app)
	return err
}

func rolloutInProgress(ctx context.Context, tx *sql.Tx, app string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM rollouts WHERE app = ? AND state = 'in_progress'`, app).Scan(&n)
	return n > 0, err
}

func loadDrainMoves(ctx context.Context, q querier, node string) ([]drainMoveRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT app, idx, surge_node, generation, since FROM drain_moves WHERE node_id = ? ORDER BY app`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []drainMoveRow
	for rows.Next() {
		var m drainMoveRow
		if err := rows.Scan(&m.app, &m.idx, &m.surgeNode, &m.gen, &m.since); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func drainIDs(ctx context.Context, q querier, where string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT node_id FROM drains WHERE `+where+` ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// getDrain reads the node's drain, with its counts and lists.
func getDrain(ctx context.Context, q dbQuerier, node string) (Drain, error) {
	d := Drain{NodeID: node}
	var started, deadline, finished, lostAt int64
	err := q.QueryRowContext(ctx, `SELECT state, started_at, deadline, finished_at, moved, stopped, lost, lost_at FROM drains WHERE node_id = ?`, node).
		Scan(&d.State, &started, &deadline, &finished, &d.Moved, &d.Stopped, &d.Lost, &lostAt)
	if errors.Is(err, sql.ErrNoRows) {
		if _, nerr := nodeName(ctx, q, node); nerr != nil {
			return Drain{}, nerr
		}
		return Drain{}, fmt.Errorf("node %q has no drain: %w", node, ErrNotFound)
	}
	if err != nil {
		return Drain{}, err
	}
	d.StartedAt, d.Deadline = time.Unix(0, started), time.Unix(0, deadline)
	if finished != 0 {
		d.FinishedAt = time.Unix(0, finished)
	}
	if lostAt != 0 {
		d.LostAt = time.Unix(0, lostAt)
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM replicas WHERE node_id = ? AND desired = 'running'`, node).Scan(&d.Remaining); err != nil {
		return Drain{}, err
	}
	if d.OldContainers, err = oldContainers(ctx, q, node); err != nil {
		return Drain{}, err
	}
	moves, err := loadDrainMoves(ctx, q, node)
	if err != nil {
		return Drain{}, err
	}
	for _, m := range moves {
		d.Moving = append(d.Moving, DrainMove{App: m.app, Index: m.idx, To: m.surgeNode, Since: time.Unix(0, m.since)})
	}
	rows, err := q.QueryContext(ctx, `SELECT app, idx, reason FROM drain_blocked WHERE node_id = ? ORDER BY app, idx`, node)
	if err != nil {
		return Drain{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var b DrainBlocked
		if err := rows.Scan(&b.App, &b.Index, &b.Reason); err != nil {
			return Drain{}, err
		}
		d.Blocked = append(d.Blocked, b)
	}
	return d, rows.Err()
}

// nodeName returns the node's name. An unknown node is ErrNotFound, and the
// message of a removed one says so.
func nodeName(ctx context.Context, q rowQuerier, id string) (string, error) {
	var name string
	err := q.QueryRowContext(ctx, `SELECT name FROM nodes WHERE id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		if removed, rerr := isRemoved(ctx, q, id); rerr != nil {
			return "", rerr
		} else if removed {
			return "", fmt.Errorf("node %q was removed from the cluster: %w", id, ErrNotFound)
		}
		return "", fmt.Errorf("node %q: %w", id, ErrNotFound)
	}
	return name, err
}
