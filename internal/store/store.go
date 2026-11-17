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
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ao/hivemind/internal/scheduler"
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
	// ErrConflict means the request is refused in the current state, such
	// as removing a node that still renews its lease.
	ErrConflict = errors.New("conflict")
	// ErrNodeRemoved means the node ID is on the denylist of removed nodes.
	ErrNodeRemoved = errors.New("node was removed from the cluster")
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
	// Origin names what deployed the app, such as "compose:shop" for the
	// compose project shop. It is empty for an app that hivemind deploy or
	// the API created, without an origin. See PutAppWith.
	Origin string
}

// PutOptions are what PutAppWith takes besides the spec.
type PutOptions struct {
	// Origin, when set, names what deploys the app, and is stored with it.
	// A put with an origin refuses an app that exists with another origin,
	// or with none, unless Replace is set. A put without one keeps the app's
	// origin.
	Origin  string
	Replace bool
}

// MaxOriginLen is the longest origin a put may name.
const MaxOriginLen = 128

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
	// Auth is the stored credential for the registry of Spec.Image, for a
	// replica that is to run. It is nil when there is none. Only
	// AssignmentsForNode sets it.
	Auth *RegistryAuth
	// specLen is the length of the stored spec, as the queries read it, or
	// 0 (assignmentBytes).
	specLen int
}

// Node is a machine that runs an agent.
type Node struct {
	ID        string
	Name      string
	CPU       int
	Memory    int64 // bytes
	LastLease time.Time
	// ReadySince is when the node last became ready: when it first
	// registered, or when its lease renewed after lapsing. Placement waits
	// MinReady after it. The cluster's own node is always ready since the
	// start of time.
	ReadySince time.Time
	// CertNotAfter is when the node's certificate expires, as the server
	// last recorded it. Zero means there is no record. CertIssuedAt is when
	// that certificate was issued, as recorded with it.
	CertNotAfter time.Time
	CertIssuedAt time.Time
	// Eligible is false for a node that takes no new replicas, after
	// SetNodeEligibility or DrainNode. UpsertNode ignores it: a node is
	// eligible when it first registers, and keeps its eligibility after.
	Eligible bool
	// Address is the node's private IP address, where routed containers
	// publish their ports and where routers reach them. Empty for a node
	// that reported none, which takes no routed replicas.
	Address string
	// Version is the hivemind version the node last reported, such as
	// v2.2.0. Empty for a node that reported none: a build older than 2.2.
	Version string
}

// MaxNodeVersionLen caps Node.Version.
const MaxNodeVersionLen = 64

// RoutePort is one container port of a routed container and the host port
// the engine published it on, on its node's Address.
type RoutePort struct {
	Port     int `json:"port"`
	HostPort int `json:"host_port"`
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
	// RoutePorts are the published ports of a routed container, sorted by
	// container port. Empty for a container of an app without routes.
	RoutePorts []RoutePort
	UpdatedAt  time.Time // set by the store
}

// PlaceFunc picks the node for one replica. nodes is every registered node,
// sorted by ID, with LastLease, ReadySince and Eligible set. placed is every
// assignment that holds a node or a surge, the app's other replicas
// included, since their host ports and capacity are in use. now is the
// store's clock. An error leaves the replica pending, with the error text
// as its reason, and so does the choice of a node that is not eligible.
type PlaceFunc func(nodes []Node, placed []Assignment, app spec.App, replica int, now time.Time) (string, error)

// FirstNode places every replica on the first eligible node by ID. It
// ignores capacity, ports and readiness. Tests use it to choose a node; the
// server uses the scheduler.
func FirstNode(nodes []Node, _ []Assignment, _ spec.App, _ int, _ time.Time) (string, error) {
	if len(nodes) == 0 {
		return "", errors.New("no nodes are registered")
	}
	for _, n := range nodes {
		if n.Eligible {
			return n.ID, nil
		}
	}
	return "", errors.New("every node is ineligible for new replicas")
}

// Option configures Open.
type Option func(*Store)

// WithPlacement sets the placement function. The default is the scheduler.
func WithPlacement(p PlaceFunc) Option { return func(s *Store) { s.place = p } }

// WithClock sets the time source, for tests.
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// Now is the store's clock: the time it stamps leases and reports with, so
// the time to judge their age by.
func (s *Store) Now() time.Time { return s.now() }

// SetStarted records when serve started. The routing table then leaves out
// the server's own node's reports from before it, and counts other nodes'
// leases from it at the earliest (see RouteTable). serve calls it once.
func (s *Store) SetStarted(t time.Time) { s.started.Store(t.UnixNano()) }

// WithMinReady sets how long a node must have been ready before it gets new
// replicas. The default is scheduler.DefaultMinReady (30 s). Zero means no
// wait; tests that place replicas straight after registering a node use it.
func WithMinReady(d time.Duration) Option {
	return func(s *Store) {
		s.minReady = d
		s.minReadySet = true
	}
}

// WithLogger makes Open log one line for each schema migration it applies
// to an existing store. The default logs nothing.
func WithLogger(log *slog.Logger) Option { return func(s *Store) { s.log = log } }

// Store is the desired-state database. It is safe for concurrent use.
type Store struct {
	// db is the one connection every write and every transaction uses.
	db *sql.DB
	// rdb is a pool of read-only connections for the reads outside a
	// transaction, the API's GETs and the node API's reads among them. In
	// WAL mode they run beside the writer, and each sees the last commit.
	rdb   *sql.DB
	place PlaceFunc
	now   func() time.Time
	// started is when serve started, in Unix nanoseconds (SetStarted); 0
	// before it is set.
	started     atomic.Int64
	minReady    time.Duration
	minReadySet bool
	log         *slog.Logger

	// reportMu makes one status report at a time, and guards reported: the
	// digest of each node's last report this process stored. A report the
	// same as the last one renews the lease and does not rewrite its rows.
	reportMu sync.Mutex
	reported map[string][sha256.Size]byte

	// defaultPlace is true when place is the scheduler, which reads nothing
	// but its arguments, so a placement whose inputs did not change can be
	// skipped. placeMu guards placeKey, the inputs of the last placement
	// that placed nothing (placecache.go).
	defaultPlace bool
	placeMu      sync.Mutex
	placeKey     [sha256.Size]byte
	placeSet     bool

	// credSizes is the size of each stored registry credential in an
	// assignment, by host, or nil until it is read (assignbytes.go).
	// credMu guards it.
	credMu    sync.Mutex
	credSizes map[string]int
}

// readConns is the size of the read-only pool.
const readConns = 4

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
	// One connection for writes: they are serialised in the process. Other
	// processes are kept out by SQLite's locks and wait up to busy_timeout.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now, reported: map[string][sha256.Size]byte{}}
	for _, o := range opts {
		o(s)
	}
	if !s.minReadySet {
		s.minReady = scheduler.DefaultMinReady
	}
	if s.place == nil {
		s.place, s.defaultPlace = s.schedulerPlace, true
	}
	if err := s.init(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// Readers see only committed transactions, so never a half-applied
	// change, and a read that starts after a commit sees it. query_only
	// refuses any write on them.
	rdb, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=query_only(1)")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	rdb.SetMaxOpenConns(readConns)
	rdb.SetMaxIdleConns(readConns)
	s.rdb = rdb
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
	return migrateLogged(ctx, s.db, migrations, s.log)
}

// Close closes the database.
func (s *Store) Close() error { return errors.Join(s.rdb.Close(), s.db.Close()) }

// tx runs fn in one IMMEDIATE transaction and commits it if fn returns nil.
// The transaction has a txCache (placecache.go) while it is open.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	txCaches.Store(tx, &txCache{})
	defer txCaches.Delete(tx)
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
	return s.PutAppWith(ctx, a, PutOptions{})
}

// PutAppWith is PutApp with an origin. With o.Origin set, an app that exists
// with another origin, or with none, is refused with ErrConflict unless
// o.Replace is set, and nothing is written; the app then takes o.Origin.
// Without one, the app keeps the origin it has, and a new app has none. An
// app that was deleted counts as new.
func (s *Store) PutAppWith(ctx context.Context, a spec.App, o PutOptions) (App, error) {
	if err := checkOrigin(o.Origin); err != nil {
		return App{}, err
	}
	c, err := cleanSpec(a)
	if err != nil {
		return App{}, err
	}
	specJSON, _ := json.Marshal(c)
	tmpl := c.Template()
	var out App
	err = s.tx(ctx, func(tx *sql.Tx) error {
		before, berr := nodeBytes(ctx, tx, "", 0)
		if berr != nil {
			return berr
		}
		now := s.now()
		var gen int
		var oldTmpl, origin string
		var deleted int
		var created int64
		err := tx.QueryRowContext(ctx, `SELECT generation, template, deleted, created_at, origin FROM apps WHERE name = ?`, c.Name).
			Scan(&gen, &oldTmpl, &deleted, &created, &origin)
		// rolling is a template change to a live app: its replicas move to the
		// new generation through a rollout. recreate is an app put again after
		// a delete: its replicas are replaced at once, as before rollouts.
		rolling, recreate := false, false
		switch {
		case errors.Is(err, sql.ErrNoRows):
			gen, created, origin = 1, now.UnixNano(), o.Origin
			_, err = tx.ExecContext(ctx, `INSERT INTO apps (name, spec, template, generation, replicas, deleted, created_at, updated_at, origin)
				VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)`, c.Name, string(specJSON), tmpl, gen, c.Replicas, created, now.UnixNano(), origin)
		case err != nil:
			return err
		default:
			switch {
			case deleted == 1:
				gen++
				created = now.UnixNano()
				recreate = true
				origin = o.Origin
			case o.Origin != "" && origin != o.Origin && !o.Replace:
				return fmt.Errorf("%w: app %s was deployed %s, not from %s; hivemind compose deploy --replace "+
					"(replace=true in the API) updates it anyway", ErrConflict, c.Name, OriginText(origin), strings.TrimPrefix(OriginText(o.Origin), "from "))
			case o.Origin != "":
				origin = o.Origin
			}
			if !recreate && oldTmpl != tmpl {
				gen++
				rolling = true
			}
			_, err = tx.ExecContext(ctx, `UPDATE apps SET spec = ?, template = ?, generation = ?, replicas = ?, deleted = 0,
				created_at = ?, updated_at = ?, origin = ? WHERE name = ?`, string(specJSON), tmpl, gen, c.Replicas, created, now.UnixNano(), origin, c.Name)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_specs (app, generation, spec) VALUES (?, ?, ?)
			ON CONFLICT (app, generation) DO UPDATE SET spec = excluded.spec`, c.Name, gen, string(specJSON)); err != nil {
			return err
		}
		if err := s.syncReplicas(ctx, tx, c.Name, c.Replicas, gen, !recreate); err != nil {
			return err
		}
		if recreate {
			// A recreate replaces the replicas in place, at once; a routed
			// spec cannot go to a node that reported no address.
			if err := releaseUnroutable(ctx, tx, c, now.UnixNano()); err != nil {
				return err
			}
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
		if err := syncRoutes(ctx, tx, c); err != nil {
			return err
		}
		// Placement kept every new copy within MaxNodeAssignmentBytes; the
		// replicas the app has may get the new spec in place.
		after, err := nodeBytes(ctx, tx, c.Name, len(specJSON))
		if err != nil {
			return err
		}
		if err := checkNodeBytes(before, after, "app "+c.Name+"'s spec", "use fewer replicas on that node, a smaller spec, or more nodes"); err != nil {
			return err
		}
		out = App{Spec: c, Generation: gen, CreatedAt: time.Unix(0, created), UpdatedAt: now, Origin: origin}
		return nil
	})
	return out, err
}

// checkOrigin refuses an origin that is too long or has control
// characters.
func checkOrigin(o string) error {
	if len(o) > MaxOriginLen || strings.ContainsFunc(o, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%w: an origin is at most %d printable characters", ErrInvalid, MaxOriginLen)
	}
	return nil
}

// OriginText says where an app with origin o came from, for messages: "from
// compose project shop", or "with hivemind deploy or the API" for none.
func OriginText(o string) string {
	switch {
	case o == "":
		return "with hivemind deploy or the API"
	case strings.HasPrefix(o, "compose:"):
		return "from compose project " + strings.TrimPrefix(o, "compose:")
	}
	return fmt.Sprintf("from %q", o)
}

// syncRoutes gives the app its routes, and frees the routes it no longer
// has. A route, a host and a path, that another app has is ErrConflict,
// naming that app, and the whole put is rolled back. Apps may share a host
// with routes of different paths.
func syncRoutes(ctx context.Context, tx *sql.Tx, a spec.App) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_routes WHERE app = ?`, a.Name); err != nil {
		return err
	}
	for _, r := range a.Routes {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT app FROM app_routes WHERE host = ? AND path = ?`, r.Host, r.Path).Scan(&owner)
		switch {
		case err == nil && r.Path == "":
			return fmt.Errorf("%w: host %s is routed by app %s; remove it there first", ErrConflict, r.Host, owner)
		case err == nil:
			return fmt.Errorf("%w: host %s with path %s is routed by app %s; remove it there first", ErrConflict, r.Host, r.Path, owner)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_routes (host, path, app, port) VALUES (?, ?, ?, ?)`,
			r.Host, r.Path, a.Name, r.Port); err != nil {
			return err
		}
	}
	return nil
}

// syncReplicas makes replicas 0..n-1 of app desired running, and marks the
// rest for removal.
//
// With keepGen false, every replica moves to generation gen now. With keepGen
// true, a replica that is on a node keeps its generation: a rollout moves it
// later, one at a time. A replica with no node has nothing running, so it
// takes gen at once, and so does one marked for removal that is wanted
// again: its container is going, and a rollout that finished while it was
// marked would never move it.
func (s *Store) syncReplicas(ctx context.Context, tx *sql.Tx, app string, n, gen int, keepGen bool) error {
	now := s.now().UnixNano()
	for i := range n {
		var err error
		if keepGen {
			_, err = tx.ExecContext(ctx, `INSERT INTO replicas (app, idx, node_id, generation, desired, pending_reason, updated_at)
				VALUES (?, ?, NULL, ?, 'running', '', ?)
				ON CONFLICT (app, idx) DO UPDATE SET
					generation = CASE WHEN replicas.node_id IS NULL OR replicas.desired != 'running' THEN excluded.generation ELSE replicas.generation END,
					held = CASE WHEN replicas.node_id IS NULL AND replicas.generation != excluded.generation THEN 0 ELSE replicas.held END,
					desired = 'running', updated_at = excluded.updated_at
				WHERE replicas.desired != 'running' OR (replicas.node_id IS NULL AND replicas.generation != excluded.generation)`,
				app, i, gen, now)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO replicas (app, idx, node_id, generation, desired, pending_reason, updated_at)
				VALUES (?, ?, NULL, ?, 'running', '', ?)
				ON CONFLICT (app, idx) DO UPDATE SET generation = excluded.generation, desired = 'running', held = 0, updated_at = excluded.updated_at
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

// placePending gives a node to every running replica that has none, except
// held replicas: their volume was on a removed node. With the scheduler as
// the placement function, it does nothing when what it reads is the same as
// when it last ran and placed nothing (placecache.go).
func (s *Store) placePending(ctx context.Context, tx *sql.Tx) error {
	rows, err := queryPending(ctx, tx)
	if err != nil || len(rows) == 0 {
		return err
	}
	nodes, err := listNodes(ctx, tx)
	if err != nil {
		return err
	}
	at := s.now()
	var fp *[sha256.Size]byte
	reasons := make([]string, len(rows))
	if s.defaultPlace {
		f, err := placedPrint(ctx, tx)
		if err != nil {
			return err
		}
		fp = &f
		for i, r := range rows {
			reasons[i] = r.as.PendingReason
		}
		if s.placeSkip(s.placeInputs(nodes, rows, reasons, f, at)) {
			return nil
		}
	}
	// Every replica that still has a container on a node holds its host
	// ports and capacity, marked for removal or not, and so does every surge.
	placed, err := placedFrom(ctx, tx, fp)
	if err != nil {
		return err
	}
	now := at.UnixNano()
	placedAny := false
	for k, r := range rows {
		p := r.as
		var id string
		candidates, perr := nodesFor(nodes, p.Spec)
		if perr == nil {
			id, perr = s.place(candidates, slices.Clone(placed), p.Spec, p.Index, at)
			if perr != nil && len(candidates) > 0 && len(candidates) < len(nodes) && len(boundAddrs(p.Spec)) > 0 {
				// Placement saw only the nodes with the address, so its
				// reason, such as "no node is ready", is about those.
				perr = fmt.Errorf("%v (%s)", perr, onlyOn(candidates, p.Spec))
			}
		}
		if perr == nil {
			switch i := slices.IndexFunc(nodes, func(n Node) bool { return n.ID == id }); {
			case i < 0:
				perr = fmt.Errorf("placement chose unknown node %q", id)
			case !nodes[i].Eligible:
				perr = fmt.Errorf("placement chose node %s, which is ineligible for new replicas", id)
			}
		}
		if perr != nil {
			reasons[k] = perr.Error()
			if _, err := tx.ExecContext(ctx, `UPDATE replicas SET pending_reason = ?, updated_at = ? WHERE app = ? AND idx = ?`,
				reasons[k], now, p.App, p.Index); err != nil {
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
		placedAny = true
	}
	// Nothing was placed, so the rows are as they were but for the reasons
	// just written: the next run with the same inputs reaches the same end.
	if fp != nil && !placedAny {
		s.placeDone(s.placeInputs(nodes, rows, reasons, *fp, at))
	}
	return nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryAssignments returns the replicas of apps that are not deleted, or of
// every app when the where clause allows it, joined with their app's spec.
func queryAssignments(ctx context.Context, q querier, where string, args ...any) ([]Assignment, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.app, r.idx, COALESCE(r.node_id, ''), r.generation, r.desired, r.pending_reason, p.spec
		FROM replicas r JOIN app_specs p ON p.app = r.app AND p.generation = r.generation WHERE `+where+` ORDER BY r.app, r.idx`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	c := cacheOf(q)
	var out []Assignment
	for rows.Next() {
		var a Assignment
		var specJSON string
		if err := rows.Scan(&a.App, &a.Index, &a.NodeID, &a.Generation, &a.Desired, &a.PendingReason, &specJSON); err != nil {
			return nil, err
		}
		a.specLen = len(specJSON)
		if a.Spec, err = c.decode(a.App, a.Generation, specJSON); err != nil {
			return nil, err
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT spec, generation, created_at, updated_at, origin FROM apps WHERE `+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		var specJSON string
		var created, updated int64
		if err := rows.Scan(&specJSON, &a.Generation, &created, &updated, &a.Origin); err != nil {
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
	return s.DeleteAppWith(ctx, name, "")
}

// DeleteAppWith is DeleteApp with an origin. With origin set, an app that
// exists with another origin, or with none, is refused with ErrConflict,
// and nothing is changed, as PutAppWith refuses it without Replace.
func (s *Store) DeleteAppWith(ctx context.Context, name, origin string) error {
	if err := checkOrigin(origin); err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if origin != "" {
			var have string
			err := tx.QueryRowContext(ctx, `SELECT origin FROM apps WHERE name = ? AND deleted = 0`, name).Scan(&have)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("app %q: %w", name, ErrNotFound)
			case err != nil:
				return err
			case have != origin:
				return fmt.Errorf("%w: app %s was deployed %s, not from %s; hivemind compose delete --force deletes it anyway",
					ErrConflict, name, OriginText(have), strings.TrimPrefix(OriginText(origin), "from "))
			}
		}
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
		// A deleted app stops routing at once, and its routes are free for
		// another app.
		if _, err := tx.ExecContext(ctx, `DELETE FROM app_routes WHERE app = ?`, name); err != nil {
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
		if _, err := tx.ExecContext(ctx, `UPDATE app_specs SET spec = ? WHERE app = ? AND generation = ?`,
			string(specJSON), name, a.Generation); err != nil {
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
	err := tx.QueryRowContext(ctx, `SELECT spec, generation, created_at, updated_at, origin FROM apps WHERE name = ? AND deleted = 0`, name).
		Scan(&specJSON, &a.Generation, &created, &updated, &a.Origin)
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
	as, err := queryAssignments(ctx, s.rdb, where, args...)
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
// Each one that is to run carries the credential stored for its image's
// registry, if there is one, so the node can pull the image.
func (s *Store) AssignmentsForNode(ctx context.Context, nodeID string) ([]Assignment, error) {
	out, err := queryAssignments(ctx, s.rdb, `r.node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	surges, err := querySurges(ctx, s.rdb, `s.node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	out = append(out, surges...)
	if err := attachRegistryAuth(ctx, s.rdb, out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpsertNode registers a node or updates its name and capacity, renews its
// lease, and places pending replicas. A node that was removed is refused with
// ErrNodeRemoved, in the same transaction, so a request that raced the
// removal cannot bring the node back.
func (s *Store) UpsertNode(ctx context.Context, n Node) error {
	if n.ID == "" || n.CPU < 0 || n.Memory < 0 {
		return fmt.Errorf("%w: node needs an ID and non-negative capacity", ErrInvalid)
	}
	if err := checkNodeAddress(n.Address); err != nil {
		return err
	}
	if err := checkNodeVersion(n.Version); err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if removed, err := isRemoved(ctx, tx, n.ID); err != nil {
			return err
		} else if removed {
			return fmt.Errorf("node %q: %w", n.ID, ErrNodeRemoved)
		}
		now := s.now().UnixNano()
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodes (id, name, cpu, memory, last_lease, ready_since, address, version)
			VALUES (?, ?, ?, ?, ?, CASE WHEN ? = (SELECT node_id FROM cluster WHERE id = 1) THEN 0 ELSE ? END, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, cpu = excluded.cpu, memory = excluded.memory,
				address = excluded.address, version = excluded.version, ready_since = `+readySinceExpr+`, last_lease = excluded.last_lease`,
			n.ID, n.Name, n.CPU, n.Memory, now, n.ID, now, n.Address, n.Version, now-int64(scheduler.DefaultLeaseTimeout), now); err != nil {
			return err
		}
		return s.placePending(ctx, tx)
	})
}

// GetNode returns one node.
func (s *Store) GetNode(ctx context.Context, id string) (Node, error) {
	nodes, err := listNodes(ctx, s.rdb)
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
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) { return listNodes(ctx, s.rdb) }

func listNodes(ctx context.Context, q querier) ([]Node, error) {
	rows, err := q.QueryContext(ctx, `SELECT n.id, n.name, n.cpu, n.memory, n.last_lease, n.ready_since, coalesce(c.not_after, 0), coalesce(c.issued_at, 0), n.eligible, n.address, n.version
		FROM nodes n LEFT JOIN node_certs c ON c.id = n.id ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var lease, ready, certNA, certIssued int64
		var eligible int
		if err := rows.Scan(&n.ID, &n.Name, &n.CPU, &n.Memory, &lease, &ready, &certNA, &certIssued, &eligible, &n.Address, &n.Version); err != nil {
			return nil, err
		}
		n.Eligible = eligible == 1
		n.LastLease, n.ReadySince = time.Unix(0, lease), time.Unix(0, ready)
		if certNA != 0 {
			n.CertNotAfter, n.CertIssuedAt = time.Unix(0, certNA), time.Unix(0, certIssued)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetNodeVersion records the hivemind version a node reported, "" for none.
// A node that is not registered gets ErrNotFound.
func (s *Store) SetNodeVersion(ctx context.Context, id, version string) error {
	if err := checkNodeVersion(version); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE nodes SET version = ? WHERE id = ?`, version, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("node %q: %w", id, ErrNotFound)
	}
	return nil
}

// checkNodeVersion refuses a version that is too long or holds a character
// that is not printable ASCII.
func checkNodeVersion(v string) error {
	if len(v) > MaxNodeVersionLen || strings.IndexFunc(v, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
		return fmt.Errorf("%w: node version must be at most %d printable characters", ErrInvalid, MaxNodeVersionLen)
	}
	return nil
}

// RenewLease renews the node's lease and changes nothing else. An agent calls
// it on its own timer, so a reconcile pass that takes longer than the lease
// timeout, stopping containers or pulling an image, does not make the node
// lost. A node that was removed gets ErrNodeRemoved, and one that is not
// registered ErrNotFound.
func (s *Store) RenewLease(ctx context.Context, nodeID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return s.renewLease(ctx, tx, nodeID, s.now().UnixNano())
	})
}

// renewLease sets the node's lease to now, and restarts its readiness if the
// lease had lapsed.
func (s *Store) renewLease(ctx context.Context, tx *sql.Tx, nodeID string, now int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE nodes SET last_lease = ?,
		ready_since = `+readySinceExpr+` WHERE id = ?`,
		now, now-int64(scheduler.DefaultLeaseTimeout), now, nodeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if removed, err := isRemoved(ctx, tx, nodeID); err != nil {
			return err
		} else if removed {
			return fmt.Errorf("node %q: %w", nodeID, ErrNodeRemoved)
		}
		return fmt.Errorf("node %q: %w", nodeID, ErrNotFound)
	}
	return nil
}

// ReportStatus replaces everything the node reported before with st, and
// renews the node's lease. Replicas marked for removal that the node no
// longer reports a container for are then deleted, and so are deleted apps
// with no replicas left.
//
// A node reports every 2 s, and most reports are the same as the one before.
// Such a report renews the lease and stamps the node's rows with the time,
// and does not delete and insert them again. The digest of each node's last
// report is kept in memory, so the first report to a process always writes
// its rows.
func (s *Store) ReportStatus(ctx context.Context, nodeID string, st []Status) error {
	seen := make(map[string]bool, len(st))
	rps := make([]string, len(st))
	for i, x := range st {
		if x.Name == "" || seen[x.Name] {
			return fmt.Errorf("%w: status needs a unique container name, got %q", ErrInvalid, x.Name)
		}
		seen[x.Name] = true
		if x.RestartCount < 0 {
			return fmt.Errorf("%w: restart count must not be negative, got %d for %s", ErrInvalid, x.RestartCount, x.Name)
		}
		rp, err := encodeRoutePorts(x.RoutePorts)
		if err != nil {
			return fmt.Errorf("%w: container %s: %v", ErrInvalid, x.Name, err)
		}
		rps[i] = rp
	}
	sum := reportDigest(st, rps)
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	last, ok := s.reported[nodeID]
	same := ok && last == sum
	// Until this transaction commits, the stored rows are not known.
	delete(s.reported, nodeID)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now().UnixNano()
		if err := s.renewLease(ctx, tx, nodeID, now); err != nil {
			return err
		}
		rewrite := true
		if same {
			res, err := tx.ExecContext(ctx, `UPDATE container_status SET updated_at = ? WHERE node_id = ?`, now, nodeID)
			if err != nil {
				return err
			}
			// Rows deleted since, as by a node removal, are written again.
			n, err := res.RowsAffected()
			rewrite = err != nil || n != int64(len(st))
		}
		if rewrite {
			if err := writeStatus(ctx, tx, nodeID, now, st, rps); err != nil {
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
		if err := purgeDeleted(ctx, tx); err != nil {
			return err
		}
		// Purged replicas free their host ports and capacity, so a replica that
		// was pending on them can be placed now rather than at the next sweep.
		return s.placePending(ctx, tx)
	})
	if err == nil {
		s.reported[nodeID] = sum
	}
	return err
}

// writeStatus replaces the node's container rows with st, whose encoded
// route ports are rps. The INSERT is prepared once for all of them: the
// driver compiles each statement it runs, and compiling was a fifth of the
// server's CPU on the scale run.
func writeStatus(ctx context.Context, tx *sql.Tx, nodeID string, now int64, st []Status, rps []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM container_status WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	if len(st) == 0 {
		return nil
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO container_status (node_id, name, app, idx, generation, container_id, state, error, updated_at,
		restart_count, health, ready, crash_looping, next_restart, route_ports)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for i, x := range st {
		if _, err := ins.ExecContext(ctx, nodeID, x.Name, x.App, x.Replica, x.Generation, x.ContainerID, x.State, x.Error, now,
			x.RestartCount, x.Health, x.Ready, x.CrashLooping, nanos(x.NextRestart), rps[i]); err != nil {
			return err
		}
	}
	return nil
}

// reportDigest is the SHA-256 of a report's content, the same for the same
// containers in any order. NodeID and UpdatedAt, which the store sets, are
// left out.
func reportDigest(st []Status, rps []string) [sha256.Size]byte {
	idx := make([]int, len(st))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int { return strings.Compare(st[a].Name, st[b].Name) })
	h := sha256.New()
	var buf []byte
	str := func(s string) {
		buf = binary.AppendUvarint(buf[:0], uint64(len(s)))
		h.Write(buf)
		h.Write([]byte(s))
	}
	num := func(n int64) {
		buf = binary.AppendVarint(buf[:0], n)
		h.Write(buf)
	}
	flag := func(b bool) {
		if b {
			num(1)
		} else {
			num(0)
		}
	}
	for _, i := range idx {
		x := st[i]
		str(x.Name)
		str(x.ContainerID)
		str(x.App)
		num(int64(x.Replica))
		num(int64(x.Generation))
		str(x.State)
		str(x.Error)
		num(int64(x.RestartCount))
		str(x.Health)
		flag(x.Ready)
		flag(x.CrashLooping)
		num(nanos(x.NextRestart))
		str(rps[i])
	}
	var out [sha256.Size]byte
	h.Sum(out[:0])
	return out
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
	return queryStatus(ctx, s.rdb, where+` ORDER BY app, idx, name`, args...)
}

// StatusForNode returns what one node last reported, sorted by app, replica
// and name. An agent reads it at start-up to recover its restart counts.
func (s *Store) StatusForNode(ctx context.Context, nodeID string) ([]Status, error) {
	return queryStatus(ctx, s.rdb, `node_id = ? ORDER BY app, idx, name`, nodeID)
}

func queryStatus(ctx context.Context, q querier, where string, args ...any) ([]Status, error) {
	rows, err := q.QueryContext(ctx, `SELECT node_id, name, container_id, app, idx, generation, state, error, updated_at,
		restart_count, health, ready, crash_looping, next_restart, route_ports
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
		var rp string
		if err := rows.Scan(&x.NodeID, &x.Name, &x.ContainerID, &x.App, &x.Replica, &x.Generation, &x.State, &x.Error, &updated,
			&x.RestartCount, &x.Health, &ready, &crash, &next, &rp); err != nil {
			return nil, err
		}
		if x.RoutePorts, err = decodeRoutePorts(rp); err != nil {
			return nil, fmt.Errorf("container %s: stored route ports: %w", x.Name, err)
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
