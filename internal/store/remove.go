package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ao/hivemind/internal/scheduler"
)

// Node removal.
//
// Removing a node takes it out of the cluster for good. Its replicas are
// placed again, its rows go, and its ID goes on the removed_nodes denylist.
// There is no certificate revocation list: the node API checks the denylist
// on every request instead, so the node's certificate stops working the
// moment the removal commits, and UpsertNode refuses the ID, so the node
// cannot register its way back.

// Removal is what RemoveNode did.
type Removal struct {
	ID   string
	Name string
	// Rescheduled counts the running replicas that lost the node and were
	// placed again, or wait for a node.
	Rescheduled int
	// Held counts the volume replicas that now wait with no node, because
	// their data was on the removed node.
	Held int
}

// RemovedNode is one entry of the denylist.
type RemovedNode struct {
	ID        string
	Name      string
	RemovedAt time.Time
}

// RemoveNode removes the node id and puts its ID on the denylist, in one
// transaction:
//
//   - the server's own node is refused (ErrConflict);
//   - a node whose lease is current is refused (ErrConflict) unless force is
//     set, because it is still running and its containers would keep running
//     beside the copies placed elsewhere;
//   - each running replica on the node loses it. A replica without a volume is
//     placed again like a new replica. A replica with a volume is held: it has
//     no node, placement leaves it alone, and its reason says why, because its
//     data was on the removed node. A deploy that changes the app's spec
//     releases it;
//   - replicas marked for removal on the node, its surges and its reported
//     container status are deleted;
//   - the node row is deleted, and its ID is added to removed_nodes.
//
// An unknown node is ErrNotFound. A node already removed is ErrNotFound too,
// and its message says so.
func (s *Store) RemoveNode(ctx context.Context, id string, force bool) (Removal, error) {
	var out Removal
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		var local string
		err := tx.QueryRowContext(ctx, `SELECT node_id FROM cluster WHERE id = 1`).Scan(&local)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if id == local {
			return fmt.Errorf("%w: node %s runs the server, and the server's own node cannot be removed", ErrConflict, id)
		}
		var name string
		var lease int64
		err = tx.QueryRowContext(ctx, `SELECT name, last_lease FROM nodes WHERE id = ?`, id).Scan(&name, &lease)
		if errors.Is(err, sql.ErrNoRows) {
			if removed, rerr := isRemoved(ctx, tx, id); rerr != nil {
				return rerr
			} else if removed {
				return fmt.Errorf("node %q was already removed: %w", id, ErrNotFound)
			}
			return fmt.Errorf("node %q: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if age := now.Sub(time.Unix(0, lease)); age <= scheduler.DefaultLeaseTimeout && !force {
			return fmt.Errorf("%w: node %s renewed its lease %s ago, so it is still running; stop hivemind on it first, or force the removal (--force, or force=true in the API)",
				ErrConflict, id, max(age, 0).Round(time.Second))
		}
		out = Removal{ID: id, Name: name}

		reps, err := queryAssignments(ctx, tx, `r.node_id = ? AND r.desired = 'running'`, id)
		if err != nil {
			return err
		}
		ts := now.UnixNano()
		for _, r := range reps {
			if len(r.Spec.Volumes) > 0 {
				reason := fmt.Sprintf("volume was on node %s, which was removed, and no other node has its data; "+
					"deploy the app with a changed spec, such as a new volume name, to place it with an empty volume, or delete the app", id)
				if _, err := tx.ExecContext(ctx, `UPDATE replicas SET node_id = NULL, held = 1, pending_reason = ?, updated_at = ?
					WHERE app = ? AND idx = ?`, reason, ts, r.App, r.Index); err != nil {
					return err
				}
				out.Held++
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE replicas SET node_id = NULL, pending_reason = '', updated_at = ? WHERE app = ? AND idx = ?`,
				ts, r.App, r.Index); err != nil {
				return err
			}
			out.Rescheduled++
		}
		for _, q := range []string{
			// The node will never report these containers gone, so the
			// replicas marked for removal go now.
			`DELETE FROM replicas WHERE node_id = ? AND desired = 'removed'`,
			`DELETE FROM surges WHERE node_id = ?`,
			`DELETE FROM container_status WHERE node_id = ?`,
			`DELETE FROM nodes WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO removed_nodes (id, name, removed_at) VALUES (?, ?, ?)`, id, name, ts); err != nil {
			return err
		}
		if err := purgeDeleted(ctx, tx); err != nil {
			return err
		}
		return s.placePending(ctx, tx)
	})
	return out, err
}

// IsNodeRemoved reports whether id is on the denylist of removed nodes.
func (s *Store) IsNodeRemoved(ctx context.Context, id string) (bool, error) {
	return isRemoved(ctx, s.db, id)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func isRemoved(ctx context.Context, q rowQuerier, id string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM removed_nodes WHERE id = ?`, id).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// RemovedNodes returns the denylist, sorted by ID.
func (s *Store) RemovedNodes(ctx context.Context) ([]RemovedNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, removed_at FROM removed_nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RemovedNode
	for rows.Next() {
		var r RemovedNode
		var at int64
		if err := rows.Scan(&r.ID, &r.Name, &at); err != nil {
			return nil, err
		}
		r.RemovedAt = time.Unix(0, at)
		out = append(out, r)
	}
	return out, rows.Err()
}
