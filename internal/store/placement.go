package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/spec"
)

// Placement and node loss.
//
// The scheduler chooses the node for each replica. This file connects it to
// the store, and runs the sweep that finds lost nodes. The sweep is
// ReconcileNodes; the server calls it every few seconds.
//
// A node is lost when its lease has not renewed for scheduler.DefaultLeaseTimeout,
// except a node that waits for its first report after serve starts (the
// start grace, start.go). Its replicas then give up their node, and are placed again, except replicas
// with a volume: a volume lives on one node, so those stay pinned to it and
// are reported unavailable until it returns. A returning node gets only the
// assignments it still holds, so a replica that moved is not started there
// again, and its agent removes the old container. That is the fencing the
// generation numbers allow.

// readySinceExpr is the new ready_since of a node whose lease is renewed at
// now. It takes two arguments: the lease cutoff (now minus the lease timeout)
// and now. The cluster's own node is always ready, so it is 0. A node whose
// lease had lapsed is ready again from now. Any other node keeps its value.
const readySinceExpr = `CASE WHEN nodes.id = (SELECT node_id FROM cluster WHERE id = 1) THEN 0
	WHEN nodes.last_lease < ? THEN ? ELSE nodes.ready_since END`

// schedulerPlace is the default PlaceFunc. It runs scheduler.Place for one
// replica against the nodes and what is already placed.
func (s *Store) schedulerPlace(nodes []Node, placed []Assignment, app spec.App, replica int, now time.Time) (string, error) {
	if len(nodes) == 0 {
		return "", errors.New("no nodes are registered")
	}
	sn := schedNodes(nodes, now)
	// Bytes bound what each assignment takes in its node's list
	// (MaxNodeAssignmentBytes). The new one's node ID is the longest any
	// node has, and its reason gets room for one the server may set later.
	creds := s.registryBytesCached()
	existing := make([]scheduler.Assignment, 0, len(placed))
	for _, a := range placed {
		r := schedReplica(a.App, a.Index, a.Generation, a.Spec)
		r.Bytes = assignmentBytes(a, a.specLen, credFor(creds, a.Desired, a.Spec))
		existing = append(existing, scheduler.Assignment{NodeID: a.NodeID, Replica: r})
	}
	want := schedReplica(app.Name, replica, 0, app)
	longest := ""
	for _, n := range nodes {
		if len(n.ID) > len(longest) {
			longest = n.ID
		}
	}
	want.Bytes = reasonRoom + assignmentBytes(Assignment{Replica: Replica{App: app.Name, NodeID: longest, Desired: Running}},
		specBytes(app), credFor(creds, Running, app))

	out, unplaced := scheduler.Place(sn, existing, []scheduler.Replica{want}, now, s.schedOptions())
	if len(out) == 1 {
		return out[0].NodeID, nil
	}
	if len(unplaced) == 1 {
		return "", errors.New(unplaced[0].Reason)
	}
	return "", errors.New("the replica could not be placed")
}

// schedNodes describes the nodes to the scheduler as it sees them at now.
func schedNodes(nodes []Node, now time.Time) []scheduler.Node {
	sn := make([]scheduler.Node, 0, len(nodes))
	for _, n := range nodes {
		sn = append(sn, scheduler.Node{
			ID:         n.ID,
			CPU:        float64(n.CPU),
			Memory:     n.Memory,
			Ready:      now.Sub(n.LastLease) <= scheduler.DefaultLeaseTimeout,
			ReadySince: n.ReadySince,
			Ineligible: !n.Eligible,
		})
	}
	return sn
}

// schedOptions are the scheduler's options for this store.
func (s *Store) schedOptions() scheduler.Options {
	// scheduler.Options treats zero as the default wait. Here zero means no
	// wait, so it is passed as a negative value, which Place reads as no wait.
	minReady := s.minReady
	if minReady == 0 {
		minReady = -1
	}
	return scheduler.Options{MinReady: minReady, MaxNodeBytes: MaxNodeAssignmentBytes}
}

// schedReplica describes one replica to the scheduler. Its resources and host
// ports come from the spec of its generation.
func schedReplica(app string, idx, gen int, a spec.App) scheduler.Replica {
	return scheduler.Replica{
		App:        app,
		Index:      idx,
		Generation: int64(gen),
		Resources:  a.Resources,
		HostPorts:  scheduler.HostPortsOf(a),
	}
}

// placedOn returns every assignment that holds a node, including replicas
// marked for removal (their containers still run) and surges. In a
// transaction it reads them once, and again only when they changed.
func placedOn(ctx context.Context, q querier) ([]Assignment, error) {
	return placedFrom(ctx, q, nil)
}

// ReconcileNodes is the server's sweep. It marks nodes whose lease has lapsed
// as lost and releases their replicas, drops the replica reasons on nodes
// that are back, releases the replicas whose host ports bind an address their
// node does not have (see placeMisplaced), and places every pending replica.
// It returns the IDs of the
// lost nodes, sorted. It is safe to call at any time.
//
// A node that is waiting for its first report after serve started (see
// Waiting) is not lost: a server that was down, or was restored from a
// backup, finds every lease as old as its outage, and without the start
// grace its first sweep would move the replicas of nodes that are fine and
// have not reached it yet, as when their resolvers still have the old
// record of the server's name. When the grace ends with no report from any
// of the nodes that were waiting, they go on waiting (see the hold in
// start.go).
func (s *Store) ReconcileNodes(ctx context.Context) ([]string, error) {
	var lost []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		nodes, err := listNodes(ctx, tx)
		if err != nil {
			return err
		}
		leases := make([]scheduler.NodeLease, 0, len(nodes))
		for _, n := range nodes {
			if s.Waiting(n, now) {
				continue
			}
			leases = append(leases, scheduler.NodeLease{NodeID: n.ID, RenewedAt: n.LastLease})
		}
		lost = scheduler.LostNodes(leases, now, scheduler.DefaultLeaseTimeout)
		for _, id := range lost {
			if err := s.releaseNode(ctx, tx, id); err != nil {
				return err
			}
		}
		// A volume replica on a node that is back is no longer unavailable.
		if _, err := tx.ExecContext(ctx, `UPDATE replicas SET pending_reason = '', updated_at = ?
			WHERE pending_reason != '' AND node_id IS NOT NULL
			AND node_id IN (SELECT id FROM nodes WHERE last_lease >= ?)`,
			now.UnixNano(), now.UnixNano()-int64(scheduler.DefaultLeaseTimeout)); err != nil {
			return err
		}
		if err := s.placeMisplaced(ctx, tx); err != nil {
			return err
		}
		return s.placePending(ctx, tx)
	})
	return lost, err
}

// releaseNode frees what a lost node was running. A running replica without a
// volume loses its node and is placed again. A replica with a volume keeps its
// node, and its reason says the app is unavailable until the node returns.
// Surges on the node are dropped: their rollout pauses at its deadline. Replicas
// marked for removal stay, and are cleaned up when the node reports. A drain
// in progress on the node counts the replicas that lost it, and records when
// the node was first lost.
func (s *Store) releaseNode(ctx context.Context, tx *sql.Tx, id string) error {
	now := s.now().UnixNano()
	reps, err := queryAssignments(ctx, tx, `r.node_id = ? AND r.desired = 'running'`, id)
	if err != nil {
		return err
	}
	released := 0
	for _, r := range reps {
		if len(r.Spec.Volumes) > 0 {
			reason := fmt.Sprintf("volume is on node %s, which is unavailable; the app stays down until the node returns", id)
			if _, err := tx.ExecContext(ctx, `UPDATE replicas SET pending_reason = ?, updated_at = ? WHERE app = ? AND idx = ? AND pending_reason != ?`,
				reason, now, r.App, r.Index, reason); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE replicas SET node_id = NULL, pending_reason = '', updated_at = ? WHERE app = ? AND idx = ?`,
			now, r.App, r.Index); err != nil {
			return err
		}
		released++
	}
	// A drain of the node counts what the loss took, so its result adds up,
	// and records that the node was lost.
	if _, err := tx.ExecContext(ctx, `UPDATE drains SET lost = lost + ?, lost_at = CASE WHEN lost_at = 0 THEN ? ELSE lost_at END,
		updated_at = ? WHERE node_id = ? AND state = 'draining'`, released, now, now, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM surges WHERE node_id = ?`, id)
	return err
}
