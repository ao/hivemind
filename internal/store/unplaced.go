package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/spec"
)

// Replicas a write leaves with no node.
//
// A put or a scale places the app's new replicas in its own transaction, so
// it knows at once which of them no node took. The write still succeeds:
// deploying an app whose host port another app holds, and then deleting that
// app, is a sequence that must work. The write's reply lists them instead,
// with the reason status shows, and says which reasons last until someone
// acts, so that deploy --wait can stop at once rather than at its timeout.

// Unplaced is a replica that a write left with no node, and why.
type Unplaced struct {
	Index int
	// Reason is the replica's pending reason, as status shows it. When a
	// host port in use is what stops it, the apps that hold the port follow:
	// "...; held by app api". Placement's reasons name the port, not the
	// app.
	Reason string
	// Lasting is set when the reason stays until someone changes something:
	// a host port that a running replica holds on every node that could
	// take it (another app's, or another replica of this one), a bind
	// address that no node has, or more CPU or memory than any of those
	// nodes has in all. A node that is not ready, an ineligible node that
	// would fit, or a port held by a copy being removed may let the replica
	// on later, and then it is false.
	Lasting bool
}

// PutAppPlaced is PutAppWith, and also returns the app's replicas that
// should run and that the put left with no node, by index.
func (s *Store) PutAppPlaced(ctx context.Context, a spec.App, o PutOptions) (App, []Unplaced, error) {
	var un []Unplaced
	got, err := s.putApp(ctx, a, o, func(ctx context.Context, tx *sql.Tx, name string) error {
		var err error
		un, err = s.unplacedOf(ctx, tx, name)
		return err
	})
	return got, un, err
}

// ScalePlaced is Scale, and also returns the app's replicas that the scale
// left with no node, as PutAppPlaced does.
func (s *Store) ScalePlaced(ctx context.Context, name string, replicas int) (App, []Unplaced, error) {
	var un []Unplaced
	got, err := s.scale(ctx, name, replicas, func(ctx context.Context, tx *sql.Tx, name string) error {
		var err error
		un, err = s.unplacedOf(ctx, tx, name)
		return err
	})
	return got, un, err
}

// afterPlace runs at the end of a write's transaction, after its placement.
type afterPlace func(ctx context.Context, tx *sql.Tx, app string) error

// unplacedOf returns the replicas of app that should run and have no node,
// by index, with their reasons. A replica held because its volume was on a
// removed node keeps its reason, and is not lasting: it waits for that node.
func (s *Store) unplacedOf(ctx context.Context, tx *sql.Tx, app string) ([]Unplaced, error) {
	pending, err := queryAssignments(ctx, tx, `r.app = ? AND r.desired = 'running' AND r.node_id IS NULL AND r.held = 0`, app)
	if err != nil {
		return nil, err
	}
	held, err := queryAssignments(ctx, tx, `r.app = ? AND r.desired = 'running' AND r.node_id IS NULL AND r.held = 1`, app)
	if err != nil {
		return nil, err
	}
	if len(pending)+len(held) == 0 {
		return nil, nil
	}
	out := make([]Unplaced, 0, len(pending)+len(held))
	for _, h := range held {
		out = append(out, Unplaced{Index: h.Index, Reason: reasonOr(h.PendingReason)})
	}
	if len(pending) > 0 {
		nodes, err := listNodes(ctx, tx)
		if err != nil {
			return nil, err
		}
		existing, err := leavingMarked(ctx, tx)
		if err != nil {
			return nil, err
		}
		now := s.now()
		// What stops a replica depends on its spec, not its index, so it is
		// worked out once for each generation.
		type why struct {
			lasting bool
			held    string
		}
		byGen := map[int]why{}
		for _, p := range pending {
			w, ok := byGen[p.Generation]
			if !ok {
				candidates, err := nodesFor(nodes, p.Spec)
				if err != nil {
					// No node has the addresses the ports bind, which lasts.
					// A routed app whose nodes report no address waits for a
					// node of 2.2 or later, which may be one that registers
					// again after an upgrade: that is not counted as lasting.
					w.lasting = !errors.Is(err, errNoRoutableNode)
				} else {
					b := scheduler.Blocked(schedNodes(candidates, now), existing,
						schedReplica(p.App, p.Index, p.Generation, p.Spec), now, s.schedOptions())
					w.lasting = b.Lasting
					if len(b.Holders) > 0 {
						w.held = "; " + heldBy(app, b.Holders)
					}
				}
				byGen[p.Generation] = w
			}
			out = append(out, Unplaced{Index: p.Index, Reason: reasonOr(p.PendingReason) + w.held, Lasting: w.lasting})
		}
	}
	slices.SortFunc(out, func(x, y Unplaced) int { return cmp.Compare(x.Index, y.Index) })
	return out, nil
}

// leavingMarked returns every copy that holds a node, as the scheduler sees
// it, with Leaving set on the replicas marked for removal and on surges.
func leavingMarked(ctx context.Context, tx *sql.Tx) ([]scheduler.Assignment, error) {
	reps, err := queryAssignments(ctx, tx, `r.node_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	surges, err := querySurges(ctx, tx, `1`)
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.Assignment, 0, len(reps)+len(surges))
	for _, a := range reps {
		out = append(out, scheduler.Assignment{NodeID: a.NodeID, Replica: schedReplica(a.App, a.Index, a.Generation, a.Spec),
			Leaving: a.Desired != Running})
	}
	for _, a := range surges {
		out = append(out, scheduler.Assignment{NodeID: a.NodeID, Replica: schedReplica(a.App, a.Index, a.Generation, a.Spec),
			Leaving: true})
	}
	return out, nil
}

// heldBy names the apps that hold a host port that app needs: "held by app
// api", "held by apps api, db", "held by another replica of web", or "held
// by app api and another replica of web".
func heldBy(app string, holders []string) string {
	var others []string
	self := false
	for _, h := range holders {
		if h == app {
			self = true
		} else {
			others = append(others, h)
		}
	}
	var parts []string
	switch len(others) {
	case 0:
	case 1:
		parts = append(parts, "app "+others[0])
	default:
		parts = append(parts, "apps "+strings.Join(others, ", "))
	}
	if self {
		parts = append(parts, "another replica of "+app)
	}
	return "held by " + strings.Join(parts, " and ")
}

// reasonOr is a pending replica's reason, or a plain one when it has none.
func reasonOr(r string) string {
	if r == "" {
		return "it waits for a node"
	}
	return r
}
