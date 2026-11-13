// Package agent runs the containers assigned to one node. Each pass it fetches
// the node's assignments, compares them with the containers that carry
// hivemind labels, starts what is missing, stops and removes what is extra,
// and reports what it found.
//
// The agent keeps no state between passes. The labels on the containers are
// its record, so a restarted agent picks up where the last one stopped.
package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/store"
)

// Defaults for Config.
const (
	DefaultInterval    = 2 * time.Second
	DefaultStopTimeout = 10 * time.Second
)

// Source gives the agent its assignments and takes its status reports. The
// store implements it in-process; the node API client will implement it
// over the network.
type Source interface {
	AssignmentsForNode(ctx context.Context, nodeID string) ([]store.Assignment, error)
	ReportStatus(ctx context.Context, nodeID string, st []store.Status) error
}

// Config configures an Agent.
type Config struct {
	NodeID  string
	Runtime runtime.Runtime
	Source  Source
	// Interval is the time between passes. Zero means DefaultInterval.
	Interval time.Duration
	// StopTimeout is how long a container gets after SIGTERM before it is
	// killed. Zero means DefaultStopTimeout.
	StopTimeout time.Duration
	// Logger receives one line per action and per error. Nil means slog.Default().
	Logger *slog.Logger
}

// Agent reconciles one node.
type Agent struct {
	cfg     Config
	log     *slog.Logger
	trigger chan struct{}
	mu      sync.Mutex // one pass at a time
}

// New returns an agent. It does nothing until Run or Reconcile is called.
func New(cfg Config) (*Agent, error) {
	if cfg.NodeID == "" || cfg.Runtime == nil || cfg.Source == nil {
		return nil, errors.New("agent: NodeID, Runtime and Source are required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = DefaultStopTimeout
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Agent{
		cfg:     cfg,
		log:     log.With("node", cfg.NodeID),
		trigger: make(chan struct{}, 1),
	}, nil
}

// ContainerName is the name of the container for one replica generation.
// Names are deterministic, so an agent that stops between create and start
// finds the created container next pass instead of making a second one.
func ContainerName(app string, replica, generation int) string {
	return fmt.Sprintf("hivemind-%s-%d-g%d", app, replica, generation)
}

// Trigger asks Run for a pass now rather than at the next tick. It does not
// block; triggers that arrive during a pass are merged into one.
func (a *Agent) Trigger() {
	select {
	case a.trigger <- struct{}{}:
	default:
	}
}

// Run makes a pass at once, then one per interval and one per Trigger, until
// ctx is cancelled. Errors are logged and the next pass retries. It returns
// nil when ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	t := time.NewTicker(a.cfg.Interval)
	defer t.Stop()
	for {
		if err := a.Reconcile(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("reconcile pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-a.trigger:
		}
	}
}

type replicaKey struct {
	app   string
	index int
}

// Reconcile makes one pass. It returns every error it met, joined; the pass
// carries on past a failed replica, so one bad image does not hold up the
// rest. If the assignments can't be fetched it changes nothing, so
// containers keep running while the server is down.
func (a *Agent) Reconcile(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	assigned, err := a.cfg.Source.AssignmentsForNode(ctx, a.cfg.NodeID)
	if err != nil {
		return fmt.Errorf("fetching assignments: %w", err)
	}
	want := map[replicaKey]store.Assignment{}
	for _, as := range assigned {
		if as.Desired == store.Running && as.NodeID == a.cfg.NodeID {
			want[replicaKey{as.App, as.Index}] = as
		}
	}
	wantKeys := sortedKeys(want)

	containers, err := a.cfg.Runtime.List(ctx, map[string]string{runtime.LabelApp: ""})
	if err != nil {
		// Without the list nothing can be compared. Report every replica as
		// unknown, and leave the containers alone.
		err = fmt.Errorf("listing containers: %w", err)
		st := make([]store.Status, 0, len(wantKeys))
		for _, k := range wantKeys {
			as := want[k]
			st = append(st, store.Status{Name: ContainerName(as.App, as.Index, as.Generation), App: as.App,
				Replica: as.Index, Generation: as.Generation, State: "unknown", Error: err.Error()})
		}
		return errors.Join(err, a.report(ctx, st))
	}
	slices.SortFunc(containers, func(x, y runtime.Container) int { return cmp.Compare(x.Name, y.Name) })

	// Match containers to replicas. A container matches when its labels name
	// a wanted replica at the wanted generation; anything else is extra.
	keep := map[replicaKey]runtime.Container{}
	var extra []runtime.Container
	for _, c := range containers {
		k, gen, ok := parseLabels(c.Labels)
		as, wanted := want[k]
		if !ok || !wanted || gen != as.Generation || c.State == "dead" || c.State == "removing" {
			extra = append(extra, c)
			continue
		}
		if prev, dup := keep[k]; dup {
			if better(c, prev, ContainerName(as.App, as.Index, as.Generation)) {
				extra = append(extra, prev)
				keep[k] = c
			} else {
				extra = append(extra, c)
			}
			continue
		}
		keep[k] = c
	}

	var errs []error
	var status []store.Status

	// Remove extras first, so their host ports are free for replacements.
	for _, c := range extra {
		if err := a.remove(ctx, c); err != nil {
			errs = append(errs, err)
			// The container still exists: report it, so the server keeps a
			// replica marked for removal until it is really gone.
			k, gen, _ := parseLabels(c.Labels)
			status = append(status, store.Status{Name: c.Name, ContainerID: c.ID, App: c.Labels[runtime.LabelApp],
				Replica: k.index, Generation: gen, State: c.State, Error: err.Error()})
		}
	}

	for _, k := range wantKeys {
		as := want[k]
		st := store.Status{Name: ContainerName(as.App, as.Index, as.Generation), App: as.App, Replica: as.Index, Generation: as.Generation}
		if c, ok := keep[k]; ok {
			st.Name, st.ContainerID, st.State = c.Name, c.ID, c.State
			if c.State == "created" || c.State == "exited" {
				if err := a.cfg.Runtime.Start(ctx, c.ID); err != nil {
					err = fmt.Errorf("%s: %w", c.Name, err)
					errs = append(errs, err)
					st.Error = err.Error()
				} else {
					a.log.Info("started container", "app", as.App, "replica", as.Index, "generation", as.Generation, "container", shortID(c.ID), "was", c.State)
					st.State = "running"
				}
			}
		} else {
			id, state, err := a.create(ctx, as)
			st.ContainerID, st.State = id, state
			if err != nil {
				errs = append(errs, err)
				st.Error = err.Error()
			}
		}
		status = append(status, st)
	}

	if err := a.report(ctx, status); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (a *Agent) report(ctx context.Context, st []store.Status) error {
	if err := a.cfg.Source.ReportStatus(ctx, a.cfg.NodeID, st); err != nil {
		return fmt.Errorf("reporting status: %w", err)
	}
	return nil
}

// create makes and starts the container for one replica. The image is pulled
// only when it is missing, so a replacement does not wait for the registry.
// It returns the container ID, the state the container is left in, and any
// error.
func (a *Agent) create(ctx context.Context, as store.Assignment) (string, string, error) {
	cfg := containerConfig(as)
	log := a.log.With("app", as.App, "replica", as.Index, "generation", as.Generation)

	id, err := a.cfg.Runtime.Create(ctx, cfg)
	if errors.Is(err, runtime.ErrNotFound) {
		log.Info("pulling image", "image", cfg.Image)
		if perr := a.cfg.Runtime.Pull(ctx, cfg.Image); perr != nil {
			return "", "pending", fmt.Errorf("%s: %w", cfg.Name, perr)
		}
		id, err = a.cfg.Runtime.Create(ctx, cfg)
	}
	if errors.Is(err, runtime.ErrConflict) {
		// The name is taken. If the container there is this replica (it was
		// created after the list was read), use it.
		if c, ierr := a.cfg.Runtime.Inspect(ctx, cfg.Name); ierr == nil {
			if k, gen, ok := parseLabels(c.Labels); ok && k == (replicaKey{as.App, as.Index}) && gen == as.Generation {
				id, err = c.ID, nil
			}
		}
	}
	if err != nil {
		return "", "pending", fmt.Errorf("%s: %w", cfg.Name, err)
	}
	log.Info("created container", "container", shortID(id))
	if err := a.cfg.Runtime.Start(ctx, id); err != nil {
		return id, "created", fmt.Errorf("%s: %w", cfg.Name, err)
	}
	log.Info("started container", "container", shortID(id))
	return id, "running", nil
}

// remove stops a container and deletes it. A container that is already gone
// is not an error.
func (a *Agent) remove(ctx context.Context, c runtime.Container) error {
	log := a.log.With("container", shortID(c.ID), "name", c.Name)
	if c.State == "running" || c.State == "restarting" || c.State == "paused" {
		if err := a.cfg.Runtime.Stop(ctx, c.ID, a.cfg.StopTimeout); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			// Remove with force below kills it anyway.
			log.Warn("stopping extra container", "err", err)
		}
	}
	if err := a.cfg.Runtime.Remove(ctx, c.ID, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return fmt.Errorf("removing %s: %w", c.Name, err)
	}
	log.Info("removed container", "app", c.Labels[runtime.LabelApp], "replica", c.Labels[runtime.LabelReplica],
		"generation", c.Labels[runtime.LabelGeneration])
	return nil
}

func containerConfig(as store.Assignment) runtime.ContainerConfig {
	cfg := runtime.ContainerConfig{
		Name:       ContainerName(as.App, as.Index, as.Generation),
		Image:      as.Spec.Image,
		Command:    as.Spec.Command,
		Env:        as.Spec.Env,
		App:        as.App,
		Replica:    as.Index,
		Generation: as.Generation,
	}
	for _, p := range as.Spec.Ports {
		cfg.Ports = append(cfg.Ports, runtime.PortBinding{HostIP: p.Bind, HostPort: p.Host, ContainerPort: p.Container, Protocol: p.Protocol})
	}
	for _, v := range as.Spec.Volumes {
		cfg.Volumes = append(cfg.Volumes, runtime.VolumeMount{Name: v.Name, Path: v.Path})
	}
	return cfg
}

// parseLabels reads a container's hivemind labels. ok is false when any of
// them is missing or malformed.
func parseLabels(l map[string]string) (k replicaKey, gen int, ok bool) {
	app := l[runtime.LabelApp]
	idx, err1 := strconv.Atoi(l[runtime.LabelReplica])
	gen, err2 := strconv.Atoi(l[runtime.LabelGeneration])
	if app == "" || err1 != nil || err2 != nil || idx < 0 || gen < 1 {
		return replicaKey{}, 0, false
	}
	return replicaKey{app, idx}, gen, true
}

// better reports whether c should be kept over prev, of two containers for
// the same replica generation: a running one, then the one with the expected
// name.
func better(c, prev runtime.Container, name string) bool {
	if c.Running() != prev.Running() {
		return c.Running()
	}
	return c.Name == name && prev.Name != name
}

func sortedKeys(m map[replicaKey]store.Assignment) []replicaKey {
	keys := make([]replicaKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y replicaKey) int {
		return cmp.Or(cmp.Compare(x.app, y.app), cmp.Compare(x.index, y.index))
	})
	return keys
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
