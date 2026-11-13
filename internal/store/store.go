// Package store keeps the cluster's desired state, and the container status
// that nodes report, in a SQLite file. It uses modernc.org/sqlite, which is
// pure Go, so binaries stay static.
//
// Every method that changes state runs in one transaction, and the database
// runs in WAL mode with synchronous=FULL, so a process killed at any point
// leaves either the old state or the new one.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/spec"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Errors returned by the store. Match them with errors.Is.
var (
	// ErrNotFound means the app or node does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalid means the request was rejected before anything was written.
	ErrInvalid = errors.New("invalid")
	// ErrNewerSchema means the file was written by a newer hivemind.
	ErrNewerSchema = errors.New("store schema is newer than this build")
)

// Desired is what the server wants for one replica.
type Desired string

const (
	// Running means a container for the replica should run on its node.
	Running Desired = "running"
	// Removed means the replica's containers should be stopped and removed.
	// The row is deleted once its node reports no container for it.
	Removed Desired = "removed"
)

// App is a stored app spec.
type App struct {
	Spec spec.App
	// Generation starts at 1 and goes up by one each time the spec's
	// Template changes, or when an app is created again after a delete.
	Generation int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Replica is one copy of an app and where it is placed.
type Replica struct {
	App   string
	Index int
	// NodeID is empty while the replica is pending: no node could take it.
	NodeID string
	// Generation is the app generation the replica's container must have.
	Generation int
	Desired    Desired
	// PendingReason says why NodeID is empty.
	PendingReason string
}

// Assignment is a replica sent to its node, with the spec to run it from.
type Assignment struct {
	Replica
	Spec spec.App
}

// Node is a machine that runs an agent.
type Node struct {
	ID        string
	Name      string
	CPU       int
	Memory    int64 // bytes
	LastLease time.Time
}

// Status is one container as a node last reported it. A replica that has no
// container yet, because pulling or creating it failed, is reported with an
// empty ContainerID and the error.
type Status struct {
	NodeID      string // set by the store from the reporting node
	Name        string // container name, unique on the node
	ContainerID string
	App         string
	Replica     int
	Generation  int
	State       string // the engine's state, or "pending" or "unknown"
	Error       string
	// RestartCount is how many times the agent restarted this container
	// since it was created: after an exit, or after failing its health check.
	// It resets after 10 minutes of healthy running.
	RestartCount int
	// Health is "" when the replica has no health check, otherwise "starting",
	// "healthy" or "unhealthy".
	Health string
	// Ready is true when the container is running and passes its health
	// check, or has none. A rolling update waits for this.
	Ready bool
	// CrashLooping is true while a restart is scheduled after a failure.
	CrashLooping bool
	// NextRestart is when the scheduled restart is due. Zero when none is.
	NextRestart time.Time
	UpdatedAt   time.Time // set by the store
}

// PlaceFunc picks the node for one replica. nodes is every registered node,
// sorted by ID. placed is every running replica that already has a node,
// the app's other replicas included. An error leaves the replica pending,
// with the error text as its reason.
//
// The multi-node scheduler replaces this function; the store only calls it.
type PlaceFunc func(nodes []Node, placed []Assignment, app spec.App, replica int) (string, error)

// FirstNode places every replica on the first node by ID. On a single node
// that is the local node.
func FirstNode(nodes []Node, _ []Assignment, _ spec.App, _ int) (string, error) {
	if len(nodes) == 0 {
		return "", errors.New("no nodes are registered")
	}
	return nodes[0].ID, nil
}

// Option configures Open.
type Option func(*Store)

// WithPlacement sets the placement function. The default is FirstNode.
func WithPlacement(p PlaceFunc) Option { return func(s *Store) { s.place = p } }

// WithClock sets the time source, for tests.
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// Store is the desired-state database. It is safe for concurrent use.
type Store struct {
	db    *sql.DB
	place PlaceFunc
	now   func() time.Time
}

// Open opens or creates the store at path and applies any pending migrations.
// The file's directory is created with mode 0700 if missing, and tightened to
// 0700 if it is wider, so it should be a directory hivemind owns. The file is
// mode 0600: specs hold environment variables, which can be secrets.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("%w: store path %q: must be non-empty, without ? or #", ErrInvalid, path)
	}
	if err := prepareFile(path); err != nil {
		return nil, err
	}
	dsn := path + "?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// One connection: writes are serialised in the process, and a reader never
	// sees a half-applied change from this process. Other processes are kept
	// out by SQLite's locks and wait up to busy_timeout.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, place: FirstNode, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	if err := s.init(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return s, nil
}

func prepareFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating store directory: %w", err)
	}
	if fi, err := os.Stat(dir); err != nil {
		return err
	} else if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, fi.Mode().Perm()&0o700); err != nil {
			return fmt.Errorf("store directory %s: %w", dir, err)
		}
	}
	// Create the file before SQLite does, so it never exists with a wider
	// mode. SQLite gives the -wal and -shm files the main file's mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("store file: %w", err)
	}
	f.Close()
	return os.Chmod(path, 0o600)
}

func (s *Store) init(ctx context.Context) error {
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("journal mode is %q, want wal", mode)
	}
	var fk int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		return err
	}
	if fk != 1 {
		return errors.New("foreign keys are off")
	}
	return migrate(ctx, s.db, migrations)
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// tx runs fn in one IMMEDIATE transaction and commits it if fn returns nil.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// cleanSpec returns a normalised, validated deep copy of a.
func cleanSpec(a spec.App) (spec.App, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return spec.App{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var c spec.App
	if err := json.Unmarshal(b, &c); err != nil {
		return spec.App{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		return spec.App{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return c, nil
}

// PutApp creates the app, or replaces its spec. The generation goes up only
// when the spec's Template changes; a change to the replica count alone keeps
// it, and adds or removes replicas. Putting an app that was deleted creates
// it again with a higher generation, so its old containers are replaced.
func (s *Store) PutApp(ctx context.Context, a spec.App) (App, error) {
	c, err := cleanSpec(a)
	if err != nil {
		return App{}, err
	}
	specJSON, _ := json.Marshal(c)
	tmpl := c.Template()
	var out App
	err = s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		var gen int
		var oldTmpl string
		var deleted int
		var created int64
		err := tx.QueryRowContext(ctx, `SELECT generation, template, deleted, created_at FROM apps WHERE name = ?`, c.Name).
			Scan(&gen, &oldTmpl, &deleted, &created)
		// rolling is a template change to a live app: its replicas move to the
		// new generation through a rollout. recreate is an app put again after
		// a delete: its replicas are replaced at once, as before rollouts.
		rolling, recreate := false, false
		switch {
		case errors.Is(err, sql.ErrNoRows):
			gen, created = 1, now.UnixNano()
			_, err = tx.ExecContext(ctx, `INSERT INTO apps (name, spec, template, generation, replicas, deleted, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, 0, ?, ?)`, c.Name, string(specJSON), tmpl, gen, c.Replicas, created, now.UnixNano())
		case err != nil:
			return err
		default:
			if deleted == 1 {
				gen++
				created = now.UnixNano()
				recreate = true
			} else if oldTmpl != tmpl {
				gen++
				rolling = true
			}
			_, err = tx.ExecContext(ctx, `UPDATE apps SET spec = ?, template = ?, generation = ?, replicas = ?, deleted = 0,
				created_at = ?, updated_at = ? WHERE name = ?`, string(specJSON), tmpl, gen, c.Replicas, created, now.UnixNano(), c.Name)
		}
		if err != nil {
			return err
		}
		if err := s.syncReplicas(ctx, tx, c.Name, c.Replicas, gen, !recreate); err != nil {
			return err
		}
		switch {
		case rolling:
			if err := s.startRollout(ctx, tx, c.Name, gen); err != nil {
				return err
			}
		case recreate:
			if _, err := tx.ExecContext(ctx, `DELETE FROM rollouts WHERE app = ?`, c.Name); err != nil {
				return err
			}
		}
		if err := s.placePending(ctx, tx); err != nil {
			return err
		}
		out = App{Spec: c, Generation: gen, CreatedAt: time.Unix(0, created), UpdatedAt: now}
		return nil
	})
	return out, err
}

// syncReplicas makes replicas 0..n-1 of app desired running, and marks the
// rest for removal.
//
// With keepGen false, every replica moves to generation gen now. With keepGen
// true, a replica that is on a node keeps its generation: a rollout moves it
// later, one at a time. A replica with no node has nothing running, so it
// takes gen at once.
func (s *Store) syncReplicas(ctx context.Context, tx *sql.Tx, app string, n, gen int, keepGen bool) error {
	now := s.now().UnixNano()
	for i := range n {
		var err error
		if keepGen {
			_, err = tx.ExecContext(ctx, `INSERT INTO replicas (app, idx, node_id, generation, desired, pending_reason, updated_at)
				VALUES (?, ?, NULL, ?, 'running', '', ?)
				ON CONFLICT (app, idx) DO UPDATE SET
					generation = CASE WHEN replicas.node_id IS NULL THEN excluded.generation ELSE replicas.generation END,
					desired = 'running', updated_at = excluded.updated_at
				WHERE replicas.desired != 'running' OR (replicas.node_id IS NULL AND replicas.generation != excluded.generation)`,
				app, i, gen, now)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO replicas (app, idx, node_id, generation, desired, pending_reason, updated_at)
				VALUES (?, ?, NULL, ?, 'running', '', ?)
				ON CONFLICT (app, idx) DO UPDATE SET generation = excluded.generation, desired = 'running', updated_at = excluded.updated_at
				WHERE replicas.generation != excluded.generation OR replicas.desired != 'running'`,
				app, i, gen, now)
		}
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE replicas SET desired = 'removed', updated_at = ? WHERE app = ? AND idx >= ? AND desired = 'running'`,
		now, app, n); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE surges SET desired = 'removed', updated_at = ? WHERE app = ? AND idx >= ?`,
		now, app, n); err != nil {
		return err
	}
	// A replica that never reached a node has no container to remove.
	_, err := tx.ExecContext(ctx, `DELETE FROM replicas WHERE app = ? AND desired = 'removed' AND node_id IS NULL`, app)
	return err
}

// placePending gives a node to every running replica that has none.
func (s *Store) placePending(ctx context.Context, tx *sql.Tx) error {
	pending, err := queryAssignments(ctx, tx, `r.desired = 'running' AND r.node_id IS NULL`)
	if err != nil || len(pending) == 0 {
		return err
	}
	nodes, err := listNodes(ctx, tx)
	if err != nil {
		return err
	}
	placed, err := queryAssignments(ctx, tx, `r.desired = 'running' AND r.node_id IS NOT NULL`)
	if err != nil {
		return err
	}
	now := s.now().UnixNano()
	for _, p := range pending {
		id, perr := s.place(slices.Clone(nodes), slices.Clone(placed), p.Spec, p.Index)
		if perr == nil && !slices.ContainsFunc(nodes, func(n Node) bool { return n.ID == id }) {
			perr = fmt.Errorf("placement chose unknown node %q", id)
		}
		if perr != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE replicas SET pending_reason = ?, updated_at = ? WHERE app = ? AND idx = ?`,
				perr.Error(), now, p.App, p.Index); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE replicas SET node_id = ?, pending_reason = '', updated_at = ? WHERE app = ? AND idx = ?`,
			id, now, p.App, p.Index); err != nil {
			return err
		}
		p.NodeID, p.PendingReason = id, ""
		placed = append(placed, p)
	}
	return nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryAssignments returns the replicas of apps that are not deleted, or of
// every app when the where clause allows it, joined with their app's spec.
func queryAssignments(ctx context.Context, q querier, where string, args ...any) ([]Assignment, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.app, r.idx, COALESCE(r.node_id, ''), r.generation, r.desired, r.pending_reason, a.spec
		FROM replicas r JOIN apps a ON a.name = r.app WHERE `+where+` ORDER BY r.app, r.idx`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Assignment
	for rows.Next() {
		var a Assignment
		var specJSON string
		if err := rows.Scan(&a.App, &a.Index, &a.NodeID, &a.Generation, &a.Desired, &a.PendingReason, &specJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(specJSON), &a.Spec); err != nil {
			return nil, fmt.Errorf("app %s: stored spec: %w", a.App, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetApp returns one app. A deleted app is not found.
func (s *Store) GetApp(ctx context.Context, name string) (App, error) {
	apps, err := s.listApps(ctx, `deleted = 0 AND name = ?`, name)
	if err != nil {
		return App{}, err
	}
	if len(apps) == 0 {
		return App{}, fmt.Errorf("app %q: %w", name, ErrNotFound)
	}
	return apps[0], nil
}

// ListApps returns every app that is not deleted, sorted by name.
func (s *Store) ListApps(ctx context.Context) ([]App, error) {
	return s.listApps(ctx, `deleted = 0`)
}

func (s *Store) listApps(ctx context.Context, where string, args ...any) ([]App, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec, generation, created_at, updated_at FROM apps WHERE `+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		var specJSON string
		var created, updated int64
		if err := rows.Scan(&specJSON, &a.Generation, &created, &updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(specJSON), &a.Spec); err != nil {
			return nil, fmt.Errorf("stored spec: %w", err)
		}
		a.CreatedAt, a.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteApp marks the app deleted and its replicas for removal. The app's
// rows are deleted once its nodes report that no container is left.
func (s *Store) DeleteApp(ctx context.Context, name string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE apps SET deleted = 1, updated_at = ? WHERE name = ? AND deleted = 0`, s.now().UnixNano(), name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("app %q: %w", name, ErrNotFound)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rollouts WHERE app = ?`, name); err != nil {
			return err
		}
		if err := s.syncReplicas(ctx, tx, name, 0, 1, false); err != nil {
			return err
		}
		return purgeDeleted(ctx, tx)
	})
}

// Scale sets the app's replica count. The generation does not change.
func (s *Store) Scale(ctx context.Context, name string, replicas int) (App, error) {
	var out App
	err := s.tx(ctx, func(tx *sql.Tx) error {
		a, err := getAppTx(ctx, tx, name)
		if err != nil {
			return err
		}
		a.Spec.Replicas = replicas
		if err := a.Spec.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		specJSON, _ := json.Marshal(a.Spec)
		now := s.now()
		if _, err := tx.ExecContext(ctx, `UPDATE apps SET spec = ?, replicas = ?, updated_at = ? WHERE name = ?`,
			string(specJSON), replicas, now.UnixNano(), name); err != nil {
			return err
		}
		if err := s.syncReplicas(ctx, tx, name, replicas, a.Generation, true); err != nil {
			return err
		}
		if err := s.placePending(ctx, tx); err != nil {
			return err
		}
		a.UpdatedAt = now
		out = a
		return nil
	})
	return out, err
}

func getAppTx(ctx context.Context, tx *sql.Tx, name string) (App, error) {
	var a App
	var specJSON string
	var created, updated int64
	err := tx.QueryRowContext(ctx, `SELECT spec, generation, created_at, updated_at FROM apps WHERE name = ? AND deleted = 0`, name).
		Scan(&specJSON, &a.Generation, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, fmt.Errorf("app %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return App{}, err
	}
	if err := json.Unmarshal([]byte(specJSON), &a.Spec); err != nil {
		return App{}, fmt.Errorf("app %s: stored spec: %w", name, err)
	}
	a.CreatedAt, a.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
	return a, nil
}

// Replicas returns the app's replicas, those marked for removal included.
// An empty app returns the replicas of every app.
func (s *Store) Replicas(ctx context.Context, app string) ([]Replica, error) {
	where, args := `1`, []any{}
	if app != "" {
		where, args = `r.app = ?`, []any{app}
	}
	as, err := queryAssignments(ctx, s.db, where, args...)
	if err != nil {
		return nil, err
	}
	out := make([]Replica, len(as))
	for i, a := range as {
		out[i] = a.Replica
	}
	return out, nil
}

// AssignmentsForNode returns every replica placed on the node, running and
// marked for removal, with its app's spec. A surge on the node is included
// as an assignment for the same replica index, at the surge's generation.
func (s *Store) AssignmentsForNode(ctx context.Context, nodeID string) ([]Assignment, error) {
	out, err := queryAssignments(ctx, s.db, `r.node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	surges, err := querySurges(ctx, s.db, `s.node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	return append(out, surges...), nil
}

// UpsertNode registers a node or updates its name and capacity, renews its
// lease, and places pending replicas.
func (s *Store) UpsertNode(ctx context.Context, n Node) error {
	if n.ID == "" || n.CPU < 0 || n.Memory < 0 {
		return fmt.Errorf("%w: node needs an ID and non-negative capacity", ErrInvalid)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodes (id, name, cpu, memory, last_lease) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, cpu = excluded.cpu, memory = excluded.memory, last_lease = excluded.last_lease`,
			n.ID, n.Name, n.CPU, n.Memory, s.now().UnixNano()); err != nil {
			return err
		}
		return s.placePending(ctx, tx)
	})
}

// GetNode returns one node.
func (s *Store) GetNode(ctx context.Context, id string) (Node, error) {
	nodes, err := listNodes(ctx, s.db)
	if err != nil {
		return Node{}, err
	}
	for _, n := range nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return Node{}, fmt.Errorf("node %q: %w", id, ErrNotFound)
}

// ListNodes returns every node, sorted by ID.
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) { return listNodes(ctx, s.db) }

func listNodes(ctx context.Context, q querier) ([]Node, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name, cpu, memory, last_lease FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var lease int64
		if err := rows.Scan(&n.ID, &n.Name, &n.CPU, &n.Memory, &lease); err != nil {
			return nil, err
		}
		n.LastLease = time.Unix(0, lease)
		out = append(out, n)
	}
	return out, rows.Err()
}

// ReportStatus replaces everything the node reported before with st, and
// renews the node's lease. Replicas marked for removal that the node no
// longer reports a container for are then deleted, and so are deleted apps
// with no replicas left.
func (s *Store) ReportStatus(ctx context.Context, nodeID string, st []Status) error {
	seen := make(map[string]bool, len(st))
	for _, x := range st {
		if x.Name == "" || seen[x.Name] {
			return fmt.Errorf("%w: status needs a unique container name, got %q", ErrInvalid, x.Name)
		}
		seen[x.Name] = true
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now().UnixNano()
		res, err := tx.ExecContext(ctx, `UPDATE nodes SET last_lease = ? WHERE id = ?`, now, nodeID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("node %q: %w", nodeID, ErrNotFound)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM container_status WHERE node_id = ?`, nodeID); err != nil {
			return err
		}
		for _, x := range st {
			if x.RestartCount < 0 {
				return fmt.Errorf("%w: restart count must not be negative, got %d for %s", ErrInvalid, x.RestartCount, x.Name)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO container_status (node_id, name, app, idx, generation, container_id, state, error, updated_at,
				restart_count, health, ready, crash_looping, next_restart)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				nodeID, x.Name, x.App, x.Replica, x.Generation, x.ContainerID, x.State, x.Error, now,
				x.RestartCount, x.Health, x.Ready, x.CrashLooping, nanos(x.NextRestart)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM replicas WHERE desired = 'removed' AND node_id = ?
			AND NOT EXISTS (SELECT 1 FROM container_status c WHERE c.node_id = replicas.node_id AND c.app = replicas.app AND c.idx = replicas.idx)`,
			nodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM surges WHERE desired = 'removed' AND node_id = ?
			AND NOT EXISTS (SELECT 1 FROM container_status c WHERE c.node_id = surges.node_id AND c.app = surges.app
				AND c.idx = surges.idx AND c.generation = surges.generation)`,
			nodeID); err != nil {
			return err
		}
		return purgeDeleted(ctx, tx)
	})
}

// purgeDeleted deletes deleted apps that have no replicas or surges left.
func purgeDeleted(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE deleted = 1
		AND NOT EXISTS (SELECT 1 FROM replicas r WHERE r.app = apps.name)
		AND NOT EXISTS (SELECT 1 FROM surges s WHERE s.app = apps.name)`)
	return err
}

// ListStatus returns what nodes last reported for the app's containers, or
// for every container when app is empty, sorted by app, replica and name.
func (s *Store) ListStatus(ctx context.Context, app string) ([]Status, error) {
	where, args := `1`, []any{}
	if app != "" {
		where, args = `app = ?`, []any{app}
	}
	return queryStatus(ctx, s.db, where+` ORDER BY app, idx, name`, args...)
}

// StatusForNode returns what one node last reported, sorted by app, replica
// and name. An agent reads it at start-up to recover its restart counts.
func (s *Store) StatusForNode(ctx context.Context, nodeID string) ([]Status, error) {
	return queryStatus(ctx, s.db, `node_id = ? ORDER BY app, idx, name`, nodeID)
}

func queryStatus(ctx context.Context, q querier, where string, args ...any) ([]Status, error) {
	rows, err := q.QueryContext(ctx, `SELECT node_id, name, container_id, app, idx, generation, state, error, updated_at,
		restart_count, health, ready, crash_looping, next_restart
		FROM container_status WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Status
	for rows.Next() {
		var x Status
		var updated, next int64
		var ready, crash int
		if err := rows.Scan(&x.NodeID, &x.Name, &x.ContainerID, &x.App, &x.Replica, &x.Generation, &x.State, &x.Error, &updated,
			&x.RestartCount, &x.Health, &ready, &crash, &next); err != nil {
			return nil, err
		}
		x.UpdatedAt = time.Unix(0, updated)
		x.Ready, x.CrashLooping = ready == 1, crash == 1
		if next != 0 {
			x.NextRestart = time.Unix(0, next)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// nanos returns t as unix nanoseconds, or 0 for the zero time.
func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}
