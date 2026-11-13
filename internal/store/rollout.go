package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/spec"
)

// Rolling updates.
//
// When an app's Template changes, the store gives the app a new generation and
// starts a rollout to it. Each replica keeps running its old generation until
// the rollout moves it. The rollout moves one replica at a time, and it moves
// the next one only when the one in flight reports Ready at the new generation.
// Two things make a move:
//
//   - Surge. When another registered node can take the replica, the new copy
//     is placed there as a surge row, beside the old copy. The replica only
//     moves to the surge's node once the new copy is Ready, and then the old
//     copy's node removes it. No replica is unavailable during a surge.
//   - In place. When no other node can take the replica, the replica's
//     generation changes and its node replaces the old container: stop the
//     old one, start the new one. One replica is unavailable for that time.
//     This is the only option on a single node, and the fallback on a full
//     cluster. A surge also needs a node where the app's host ports are free,
//     because a replica that publishes host port N can't share a node with
//     another that publishes N, and host ports are the only networking here.
//
// The rollout never runs more than one move at a time, so at most one replica
// is unavailable and at most one extra copy runs.
//
// A move that is not Ready within its deadline, or whose container is
// crash-looping, pauses the rollout. The replica in flight goes back to its
// old generation, a surge is removed, and the other replicas keep the old
// generation. The rollout's Reason says why. Deploying a spec with another
// template starts a new rollout from the state the replicas are in. Rolling
// back is deploying the old spec again, which is a new generation.

// RolloutMargin is added to a health check's own timing to get a move's
// deadline.
const RolloutMargin = 30 * time.Second

// DefaultRolloutDeadline is how long a new replica may take to become Ready
// when its app has no health check.
const DefaultRolloutDeadline = 2 * time.Minute

// RolloutDeadline returns how long a replica of a may take to become Ready at
// its new generation: the health check's start period, plus its retries at
// its interval, plus RolloutMargin. An app without a health check gets
// DefaultRolloutDeadline.
func RolloutDeadline(a spec.App) time.Duration {
	h := a.Health
	if h == nil {
		return DefaultRolloutDeadline
	}
	return h.StartPeriod.D() + time.Duration(h.Retries)*h.Interval.D() + RolloutMargin
}

// RolloutState is where a rollout is.
type RolloutState string

const (
	// RolloutInProgress means replicas are still moving to Target.
	RolloutInProgress RolloutState = "in_progress"
	// RolloutPaused means a move failed. The rollout stays paused until a new
	// spec with another template is deployed.
	RolloutPaused RolloutState = "paused"
	// RolloutDone means every replica runs Target.
	RolloutDone RolloutState = "done"
)

// Rollout is the latest rollout of an app.
type Rollout struct {
	App string
	// Target is the generation being rolled out to.
	Target int
	State  RolloutState
	// Reason is set when the rollout is paused. It is a full sentence, such
	// as "rollout to generation 3 stopped: replica 0 is crash-looping".
	Reason string
	// Moving is the replica index whose move is in flight, or -1.
	Moving int
	// Since is when the move in flight started. Zero when none is.
	Since time.Time
}

// ErrNoRollout is returned by GetRollout for an app that never had a rollout.
var ErrNoRollout = errors.New("no rollout")

// rolloutRow is the rollouts table row for one app.
type rolloutRow struct {
	app       string
	target    int
	state     RolloutState
	reason    string
	stepIdx   int
	stepFrom  int
	stepSince int64
}

// startRollout records that the app's replicas are moving to target. A move
// still running for an earlier target is abandoned: its surge is removed, and
// the replica in place is moved again like the others.
func (s *Store) startRollout(ctx context.Context, tx *sql.Tx, app string, target int) error {
	now := s.now().UnixNano()
	if _, err := tx.ExecContext(ctx, `UPDATE surges SET desired = 'removed', updated_at = ? WHERE app = ?`, now, app); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO rollouts (app, target, state, reason, step_idx, step_from, step_since, updated_at)
		VALUES (?, ?, 'in_progress', '', -1, 0, 0, ?)
		ON CONFLICT (app) DO UPDATE SET target = excluded.target, state = 'in_progress', reason = '',
			step_idx = -1, step_from = 0, step_since = 0, updated_at = excluded.updated_at`, app, target, now)
	return err
}

// GetRollout returns the app's latest rollout, or ErrNoRollout.
func (s *Store) GetRollout(ctx context.Context, app string) (Rollout, error) {
	var r rolloutRow
	var since int64
	err := s.db.QueryRowContext(ctx, `SELECT app, target, state, reason, step_idx, step_from, step_since FROM rollouts WHERE app = ?`, app).
		Scan(&r.app, &r.target, &r.state, &r.reason, &r.stepIdx, &r.stepFrom, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return Rollout{}, fmt.Errorf("app %q: %w", app, ErrNoRollout)
	}
	if err != nil {
		return Rollout{}, err
	}
	out := Rollout{App: r.app, Target: r.target, State: r.state, Reason: r.reason, Moving: r.stepIdx}
	if r.stepIdx >= 0 {
		out.Since = time.Unix(0, since)
	}
	return out, nil
}

// AdvanceRollouts moves the rollouts of every app along by one step where it
// can. It is safe to call at any time, as often as wanted: a step waits for
// its replica to report Ready, so a call that changes nothing is harmless.
// The server calls it after each status report and on a timer.
func (s *Store) AdvanceRollouts(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT r.app FROM rollouts r JOIN apps a ON a.name = r.app
			WHERE r.state = 'in_progress' AND a.deleted = 0 ORDER BY r.app`)
		if err != nil {
			return err
		}
		var apps []string
		for rows.Next() {
			var app string
			if err := rows.Scan(&app); err != nil {
				rows.Close()
				return err
			}
			apps = append(apps, app)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, app := range apps {
			if err := s.advanceApp(ctx, tx, app); err != nil {
				return fmt.Errorf("rollout of %s: %w", app, err)
			}
		}
		return nil
	})
}

func (s *Store) advanceApp(ctx context.Context, tx *sql.Tx, app string) error {
	r, err := loadRollout(ctx, tx, app)
	if err != nil {
		return err
	}
	a, err := getAppTx(ctx, tx, app)
	if err != nil {
		return err
	}
	if r.stepIdx >= 0 {
		advance, err := s.checkMove(ctx, tx, r, a.Spec)
		if err != nil || !advance {
			return err
		}
	}
	pending, err := queryAssignments(ctx, tx, `r.app = ? AND r.desired = 'running' AND r.node_id IS NOT NULL AND r.generation != ?`,
		app, r.target)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		_, err := tx.ExecContext(ctx, `UPDATE rollouts SET state = 'done', reason = '', step_idx = -1, step_from = 0, step_since = 0, updated_at = ?
			WHERE app = ?`, s.now().UnixNano(), app)
		return err
	}
	return s.startMove(ctx, tx, r, a.Spec, pending[0])
}

// checkMove looks at the move in flight. It returns true when the rollout may
// take its next step: the move finished, or its replica is gone. It returns
// false while the move waits, and after a pause.
func (s *Store) checkMove(ctx context.Context, tx *sql.Tx, r rolloutRow, a spec.App) (bool, error) {
	now := s.now()
	reps, err := queryAssignments(ctx, tx, `r.app = ? AND r.idx = ?`, r.app, r.stepIdx)
	if err != nil {
		return false, err
	}
	surge, hasSurge, err := surgeOf(ctx, tx, r.app, r.stepIdx)
	if err != nil {
		return false, err
	}
	if len(reps) == 0 || reps[0].Desired != Running {
		// The replica was scaled away or deleted during the move.
		return true, s.endMove(ctx, tx, r)
	}
	node := reps[0].NodeID
	if hasSurge {
		node = surge.NodeID
	}
	ready, crash, err := containerState(ctx, tx, node, r.app, r.stepIdx, r.target)
	if err != nil {
		return false, err
	}
	switch {
	case ready:
		if hasSurge {
			// The copy on the surge node is Ready, so the replica moves there.
			// The old node's agent removes the old copy once it sees that.
			if _, err := tx.ExecContext(ctx, `UPDATE replicas SET node_id = ?, generation = ?, updated_at = ? WHERE app = ? AND idx = ?`,
				surge.NodeID, r.target, now.UnixNano(), r.app, r.stepIdx); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM surges WHERE app = ? AND idx = ?`, r.app, r.stepIdx); err != nil {
				return false, err
			}
		}
		return true, s.endMove(ctx, tx, r)
	case crash:
		return false, s.pause(ctx, tx, r, hasSurge, fmt.Sprintf("rollout to generation %d stopped: replica %d is crash-looping", r.target, r.stepIdx))
	}
	deadline := RolloutDeadline(a)
	if now.UnixNano()-r.stepSince >= int64(deadline) {
		return false, s.pause(ctx, tx, r, hasSurge, fmt.Sprintf("rollout to generation %d stopped: replica %d is not ready after %s", r.target, r.stepIdx, deadline))
	}
	return false, nil
}

// endMove clears the move in flight, so the next step can start.
func (s *Store) endMove(ctx context.Context, tx *sql.Tx, r rolloutRow) error {
	_, err := tx.ExecContext(ctx, `UPDATE rollouts SET step_idx = -1, step_from = 0, step_since = 0, updated_at = ? WHERE app = ?`,
		s.now().UnixNano(), r.app)
	return err
}

// pause stops the rollout at reason. The replica in flight goes back to its
// old generation: an in-place move is undone, and a surge is removed. The
// other replicas are not touched.
func (s *Store) pause(ctx context.Context, tx *sql.Tx, r rolloutRow, surged bool, reason string) error {
	now := s.now().UnixNano()
	if surged {
		if _, err := tx.ExecContext(ctx, `UPDATE surges SET desired = 'removed', updated_at = ? WHERE app = ? AND idx = ?`,
			now, r.app, r.stepIdx); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE replicas SET generation = ?, updated_at = ? WHERE app = ? AND idx = ?`,
		r.stepFrom, now, r.app, r.stepIdx); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE rollouts SET state = 'paused', reason = ?, step_idx = -1, step_from = 0, step_since = 0, updated_at = ?
		WHERE app = ?`, reason, now, r.app)
	return err
}

// startMove starts the move of replica rep to the rollout's target. It takes a
// surge on another node when one can take the replica, and otherwise changes
// the replica's generation in place.
func (s *Store) startMove(ctx context.Context, tx *sql.Tx, r rolloutRow, a spec.App, rep Assignment) error {
	now := s.now().UnixNano()
	nodes, err := listNodes(ctx, tx)
	if err != nil {
		return err
	}
	placed, err := queryAssignments(ctx, tx, `r.desired = 'running' AND r.node_id IS NOT NULL`)
	if err != nil {
		return err
	}
	all, err := queryAssignments(ctx, tx, `r.node_id IS NOT NULL`)
	if err != nil {
		return err
	}
	surges, err := querySurges(ctx, tx, `1`)
	if err != nil {
		return err
	}
	all = append(all, surges...)

	var candidates []Node
	for _, n := range nodes {
		if n.ID == rep.NodeID || now-n.LastLease.UnixNano() > int64(scheduler.DefaultLeaseTimeout) {
			continue
		}
		if hostPortsTaken(a, n.ID, all) {
			continue
		}
		candidates = append(candidates, n)
	}
	surgeNode := ""
	if len(candidates) > 0 {
		id, err := s.place(slices.Clone(candidates), slices.Clone(placed), a, rep.Index)
		if err == nil && slices.ContainsFunc(candidates, func(n Node) bool { return n.ID == id }) {
			surgeNode = id
		}
	}

	if surgeNode != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO surges (app, idx, node_id, generation, desired, updated_at)
			VALUES (?, ?, ?, ?, 'running', ?)
			ON CONFLICT (app, idx) DO UPDATE SET node_id = excluded.node_id, generation = excluded.generation,
				desired = 'running', updated_at = excluded.updated_at`,
			rep.App, rep.Index, surgeNode, r.target, now); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE replicas SET generation = ?, updated_at = ? WHERE app = ? AND idx = ?`,
		r.target, now, rep.App, rep.Index); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE rollouts SET step_idx = ?, step_from = ?, step_since = ?, updated_at = ? WHERE app = ?`,
		rep.Index, rep.Generation, now, now, rep.App)
	return err
}

// hostPortsTaken reports whether any assignment on the node publishes a host
// port that app publishes. Bind addresses are ignored, so this is stricter
// than the scheduler's check.
func hostPortsTaken(a spec.App, node string, all []Assignment) bool {
	if len(a.Ports) == 0 {
		return false
	}
	for _, x := range all {
		if x.NodeID != node {
			continue
		}
		for _, p := range x.Spec.Ports {
			for _, q := range a.Ports {
				if p.Host == q.Host && p.Protocol == q.Protocol {
					return true
				}
			}
		}
	}
	return false
}

// loadRollout reads the app's rollout. It is in progress, since
// AdvanceRollouts only selects those.
func loadRollout(ctx context.Context, tx *sql.Tx, app string) (rolloutRow, error) {
	var r rolloutRow
	err := tx.QueryRowContext(ctx, `SELECT app, target, state, reason, step_idx, step_from, step_since FROM rollouts WHERE app = ?`, app).
		Scan(&r.app, &r.target, &r.state, &r.reason, &r.stepIdx, &r.stepFrom, &r.stepSince)
	return r, err
}

// surgeOf returns the surge of one replica, if there is one.
func surgeOf(ctx context.Context, tx *sql.Tx, app string, idx int) (Assignment, bool, error) {
	as, err := querySurges(ctx, tx, `s.app = ? AND s.idx = ?`, app, idx)
	if err != nil || len(as) == 0 {
		return Assignment{}, false, err
	}
	return as[0], true, nil
}

// containerState reports whether a replica's container at generation gen on
// node is Ready, and whether it is crash-looping. A container that has not
// reported yet is neither.
func containerState(ctx context.Context, tx *sql.Tx, node, app string, idx, gen int) (ready, crash bool, err error) {
	var r, c int
	err = tx.QueryRowContext(ctx, `SELECT ready, crash_looping FROM container_status
		WHERE node_id = ? AND app = ? AND idx = ? AND generation = ? ORDER BY ready DESC LIMIT 1`,
		node, app, idx, gen).Scan(&r, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return r == 1, c == 1, err
}

// querySurges returns surges matching where, joined with their app's spec.
// Surge rows marked for removal are included, since their containers still run.
func querySurges(ctx context.Context, q querier, where string, args ...any) ([]Assignment, error) {
	rows, err := q.QueryContext(ctx, `SELECT s.app, s.idx, s.node_id, s.generation, s.desired, p.spec
		FROM surges s JOIN app_specs p ON p.app = s.app AND p.generation = s.generation WHERE `+where+` ORDER BY s.app, s.idx`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Assignment
	for rows.Next() {
		var x Assignment
		var specJSON string
		if err := rows.Scan(&x.App, &x.Index, &x.NodeID, &x.Generation, &x.Desired, &specJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(specJSON), &x.Spec); err != nil {
			return nil, fmt.Errorf("app %s: stored spec: %w", x.App, err)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
