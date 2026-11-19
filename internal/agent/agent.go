// Package agent runs the containers assigned to one node. Each pass it fetches
// the node's assignments, compares them with the containers that carry
// hivemind labels, starts what is missing, hands what is extra to removals
// that stop and remove it in the background, and reports what it found.
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
	"sync/atomic"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/store"
)

// Defaults for Config.
const (
	DefaultInterval    = 2 * time.Second
	DefaultStopTimeout = 10 * time.Second
	// DefaultHeartbeat is how often Run renews the lease on its own, when
	// the Source can (LeaseRenewer). Three renewals fit in the 15 s lease.
	DefaultHeartbeat = 5 * time.Second
	// DefaultRouteDrain is how long a routed container that is no longer
	// assigned keeps running before the agent stops it. The routing table
	// drops it as soon as it is not assigned, and routers read the table
	// every second, so by then no router sends it new requests, and the
	// ones in flight have had time to finish. Load balancers call this a
	// deregistration delay.
	DefaultRouteDrain = 5 * time.Second
	// DefaultUnreachableWait is the longest wait between passes while the
	// server cannot be reached. The wait starts at the interval and doubles
	// with each pass that fails to reach it.
	DefaultUnreachableWait = 30 * time.Second
	// DefaultOutageLogEvery is how often Run logs, while the server cannot
	// be reached, that it still cannot, after the line for the first failed
	// pass.
	DefaultOutageLogEvery = time.Minute
)

// unreachable is what a Source's error has when its request did not reach
// the server, or got no reply: nodeapi.UnreachableError. The store, the
// server's own Source, has no such errors.
type unreachable interface {
	Unreachable() bool
}

// serverUnreachable reports whether err says the Source could not reach
// the server.
func serverUnreachable(err error) bool {
	var u unreachable
	return errors.As(err, &u) && u.Unreachable()
}

// LeaseRenewer is a Source that can renew the node's lease without a status
// report. Run renews it every Heartbeat, beside the passes, because a pass
// renews the lease only when it reports, at its end, and a pass can be slow:
// an engine or a server under load answers slowly, and a pass waits up to
// pullWait for a pull and up to removeWait for removals. Nomad clients
// heartbeat the same way, apart from the work they run.
type LeaseRenewer interface {
	RenewLease(ctx context.Context, nodeID string) error
}

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
	// Clock gives the time for restart backoff and health start periods.
	// Nil means time.Now. Tests set it to a fake clock.
	Clock func() time.Time
	// OnPass, if set, is called after each pass that Run makes, with the
	// wall time the pass took and its error. Metrics use it. A pass cut
	// short by the end of Run is not reported.
	OnPass func(d time.Duration, err error)
	// Heartbeat is how often Run renews the lease when Source is a
	// LeaseRenewer. Zero means DefaultHeartbeat.
	Heartbeat time.Duration
	// RouteBind is the node's private address. A routed app's containers
	// publish each route port on an ephemeral host port there. Empty means
	// the node runs no routed replica: one assigned to it fails to start.
	RouteBind string
	// RouteDrain is how long a routed container that is no longer assigned
	// keeps running before it is stopped. Zero means DefaultRouteDrain; a
	// negative value means no wait.
	RouteDrain time.Duration
	// UnreachableWait is the longest wait between passes while the Source
	// cannot reach the server. Zero means DefaultUnreachableWait.
	UnreachableWait time.Duration
	// OutageLogEvery is how often a server outage is logged after its first
	// line. Zero means DefaultOutageLogEvery.
	OutageLogEvery time.Duration
}

// Agent reconciles one node.
type Agent struct {
	cfg     Config
	log     *slog.Logger
	trigger chan struct{}
	// backingOff is set from the end of a pass that could not reach the
	// server until the next pass starts, while Run waits out the backoff,
	// so ServerAnswered knows to end the wait.
	backingOff atomic.Bool

	// mu makes one pass at a time, and guards the fields below.
	mu       sync.Mutex
	restarts map[string]*restartState // by container name
	probes   map[string]*probe        // by container name, for running containers with a health check
	seeded   bool                     // restart counts have been read from the store
	healthy  map[string]bool          // by container ID: healthy in the node's last report, until its probe starts
	wakeAt   time.Time                // when the next scheduled restart or route drain is due, or zero
	unrouted map[string]time.Time     // by container name: when a routed container was first seen unassigned
	pulls    map[pullKey]*pullState   // failed pulls, for their backoff
	launches map[string]*launchState  // by container name: failed creates and starts, for their backoff
	removals map[string]*removal      // by container ID: removals in flight, and ended ones not yet reported
	slots    chan struct{}            // removeSlots tokens, one per removal running
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
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultHeartbeat
	}
	if cfg.UnreachableWait <= 0 {
		cfg.UnreachableWait = DefaultUnreachableWait
	}
	if cfg.OutageLogEvery <= 0 {
		cfg.OutageLogEvery = DefaultOutageLogEvery
	}
	switch {
	case cfg.RouteDrain == 0:
		cfg.RouteDrain = DefaultRouteDrain
	case cfg.RouteDrain < 0:
		cfg.RouteDrain = 0
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Agent{
		cfg:      cfg,
		log:      log.With("node", cfg.NodeID),
		trigger:  make(chan struct{}, 1),
		restarts: map[string]*restartState{},
		probes:   map[string]*probe{},
		healthy:  map[string]bool{},
		unrouted: map[string]time.Time{},
		pulls:    map[pullKey]*pullState{},
		launches: map[string]*launchState{},
		removals: map[string]*removal{},
		slots:    make(chan struct{}, removeSlots),
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

// ServerAnswered tells the agent that a call to the server got a reply: a
// lease renewal, a router's table read, a log poll, any call. While Run
// waits out the backoff of passes that could not reach the server, it ends
// the wait, so the next pass runs at once and the node's containers are
// reported as soon as the server can take the report, rather than up to
// UnreachableWait later. Otherwise it does nothing, so it is cheap to call
// on every reply. It does not block.
func (a *Agent) ServerAnswered() {
	if a.backingOff.Load() {
		a.Trigger()
	}
}

// outage is a run of passes that could not reach the server.
type outage struct {
	failures int       // passes in a row that could not reach it
	since    time.Time // when the first of them ended
	logged   time.Time // when the outage was last logged
}

// wait is the time before the next pass during the outage: interval after
// the first failed pass, doubling with each one after it, up to most, and
// never less than interval.
func (o outage) wait(interval, most time.Duration) time.Duration {
	d := interval
	for i := 1; i < o.failures && d < most; i++ {
		d *= 2
	}
	return max(interval, min(d, most))
}

// notePass keeps the outage up to date after a pass that ended with err, and
// logs it: the first pass that cannot reach the server, then one line every
// OutageLogEvery with the count, and one when the server answers again.
// Any other error of the pass is logged as it is.
func (a *Agent) notePass(o *outage, err error) {
	now := time.Now()
	down := err != nil && serverUnreachable(err)
	switch {
	case down && o.failures == 0:
		*o = outage{failures: 1, since: now, logged: now}
		a.log.Warn(fmt.Sprintf("cannot reach the server; passes back off, up to one every %s, until it answers", a.cfg.UnreachableWait), "err", err)
		return
	case down:
		o.failures++
		if now.Sub(o.logged) >= a.cfg.OutageLogEvery {
			o.logged = now
			a.log.Warn("the server is still unreachable", "for", now.Sub(o.since).Round(time.Second), "failed_passes", o.failures, "err", err)
		}
		return
	case o.failures > 0:
		a.log.Info("the server answers again", "after", now.Sub(o.since).Round(time.Second), "failed_passes", o.failures)
		*o = outage{}
	}
	if err != nil {
		a.log.Warn("reconcile pass failed", "err", err)
	}
}

// Run makes a pass at once, then one per interval and one per Trigger, until
// ctx is cancelled. Errors are logged and the next pass retries. While the
// Source cannot reach the server, the passes back off, from the interval
// doubling to UnreachableWait, and the outage is logged at its start, every
// OutageLogEvery, and at its end; the lease heartbeat keeps its own timer.
// The first call to the server that gets a reply after that, the heartbeat's
// or any other that reports it with ServerAnswered, ends the wait.
// It returns nil when ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	defer a.Close()
	if lr, ok := a.cfg.Source.(LeaseRenewer); ok {
		hbCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.heartbeat(hbCtx, lr)
		}()
		defer func() {
			stop()
			<-done
		}()
	}
	t := time.NewTicker(a.cfg.Interval)
	defer t.Stop()
	var down outage
	for {
		start := time.Now()
		err := a.Reconcile(ctx)
		if ctx.Err() == nil {
			wasDown := down.failures > 0
			a.notePass(&down, err)
			if wasDown && down.failures == 0 {
				t.Reset(a.cfg.Interval)
			}
		}
		// From the end of a pass that could not reach the server to the
		// start of the next, a reply to any other call ends the wait. A
		// reply that comes before the wait begins leaves a trigger for it.
		a.backingOff.Store(down.failures > 0)
		if a.cfg.OnPass != nil && ctx.Err() == nil {
			a.cfg.OnPass(time.Since(start), err)
		}
		// A container waiting out its backoff gets a pass when the wait ends,
		// not at the next tick.
		var due <-chan time.Time
		var timer *time.Timer
		if w := a.wake(); !w.IsZero() {
			timer = time.NewTimer(max(0, w.Sub(a.cfg.Clock())))
			due = timer.C
		}
		// While the server cannot be reached, the next pass waits out the
		// backoff instead of the tick, or until a call to the server gets a
		// reply (ServerAnswered).
		tick := t.C
		var backoff *time.Timer
		if down.failures > 0 {
			backoff = time.NewTimer(down.wait(a.cfg.Interval, a.cfg.UnreachableWait))
			tick = backoff.C
		}
		select {
		case <-ctx.Done():
		case <-tick:
		case <-a.trigger:
		case <-due:
		}
		a.backingOff.Store(false)
		if timer != nil {
			timer.Stop()
		}
		if backoff != nil {
			backoff.Stop()
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// heartbeat renews the lease every Heartbeat until ctx ends. It does not take
// a.mu, so it keeps going while a pass is slow. A failure is logged at debug
// level and tried again at the next tick: the passes report the same errors.
// A renewal that works ends a backed-off wait (ServerAnswered).
func (a *Agent) heartbeat(ctx context.Context, lr LeaseRenewer) {
	t := time.NewTicker(a.cfg.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := lr.RenewLease(ctx, a.cfg.NodeID)
		switch {
		case err == nil:
			a.ServerAnswered()
		case ctx.Err() == nil:
			a.log.Debug("lease renewal", "err", err)
		}
	}
}

// replicaKey is one copy of a replica: an app, a replica index and a
// generation. A replica and its rollout surge can be on one node at two
// generations, for a routed app, and are two keys.
type replicaKey struct {
	app   string
	index int
	gen   int
}

// Reconcile makes one pass. It returns every error it met, joined; the pass
// carries on past a failed replica, so one bad image does not hold up the
// rest. If the assignments can't be fetched it changes nothing, so
// containers keep running while the server is down.
func (a *Agent) Reconcile(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	// A pass that ends early, because the server or the daemon is down,
	// schedules no wake: the one from an earlier pass may be in the past,
	// and Run would pass again at once, over and over, against a server or
	// a daemon that is down. The next tick tries again. A full pass sets the
	// wake in forget.
	a.wakeAt = time.Time{}

	assigned, err := a.cfg.Source.AssignmentsForNode(ctx, a.cfg.NodeID)
	if err != nil {
		return fmt.Errorf("fetching assignments: %w", err)
	}
	want := map[replicaKey]store.Assignment{}
	for _, as := range assigned {
		if as.Desired == store.Running && as.NodeID == a.cfg.NodeID {
			want[replicaKey{as.App, as.Index, as.Generation}] = as
		}
	}
	wantKeys := sortedKeys(want)
	if !a.seeded {
		if err := a.seedRestarts(ctx); err != nil {
			a.log.Warn("reading restart counts", "err", err)
		} else {
			a.seeded = true
		}
	}

	containers, err := a.cfg.Runtime.List(ctx, map[string]string{runtime.LabelApp: ""})
	if err != nil {
		// Without the list nothing can be compared. Report every replica as
		// unknown, and leave the containers alone.
		err = fmt.Errorf("listing containers: %w", err)
		msg := err.Error()
		if errors.Is(err, runtime.ErrUnavailable) {
			msg = "the Docker daemon on this node is not answering, so the state of its containers is not known: " + err.Error()
		}
		st := make([]store.Status, 0, len(wantKeys))
		for _, k := range wantKeys {
			as := want[k]
			st = append(st, store.Status{Name: ContainerName(as.App, as.Index, as.Generation), App: as.App,
				Replica: as.Index, Generation: as.Generation, State: "unknown", Error: msg})
		}
		return errors.Join(err, a.report(ctx, st))
	}
	slices.SortFunc(containers, func(x, y runtime.Container) int { return cmp.Compare(x.Name, y.Name) })

	// Match containers to replicas. A container matches when its labels name
	// a wanted replica at the wanted generation; anything else is extra, and
	// so is one whose removal is in flight, as after a scale down that was
	// undone at once: it is gone soon, and its replica starts again then.
	keep := map[replicaKey]runtime.Container{}
	var extra []runtime.Container
	for _, c := range containers {
		k, ok := parseLabels(c.Labels)
		as, wanted := want[k]
		if _, removing := a.removals[c.ID]; removing || !ok || !wanted || c.State == "dead" || c.State == "removing" {
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

	// Hand the extras to removals (remove.go). A routed container that is
	// still running waits out RouteDrain first, and is reported, not Ready,
	// meanwhile. The removal of an extra that a wanted container waits for
	// skips the queue.
	now := a.cfg.Clock()
	draining := map[string]bool{}
	var removing []runtime.Container
	var blocks map[replicaKey][]blocker
	for _, c := range extra {
		k, _ := parseLabels(c.Labels)
		if c.Running() && c.Labels[runtime.LabelRouted] == "true" && a.cfg.RouteDrain > 0 && a.removals[c.ID] == nil {
			first, seen := a.unrouted[c.Name]
			if !seen {
				first = now
				a.unrouted[c.Name] = now
				a.log.Info("routed container left the routing table; stopping it after the drain delay",
					"container", shortID(c.ID), "name", c.Name, "delay", a.cfg.RouteDrain)
			}
			if now.Sub(first) < a.cfg.RouteDrain {
				draining[c.Name] = true
				status = append(status, store.Status{Name: c.Name, ContainerID: c.ID, App: c.Labels[runtime.LabelApp],
					Replica: k.index, Generation: k.gen, State: c.State})
				continue
			}
		}
		removing = append(removing, c)
	}
	urgent := map[string]bool{}
	for _, k := range wantKeys {
		if c, ok := keep[k]; ok && c.Running() {
			continue
		}
		if bs := blockersOf(want[k], removing); len(bs) > 0 {
			if blocks == nil {
				blocks = map[replicaKey][]blocker{}
			}
			blocks[k] = bs
			for _, b := range bs {
				urgent[b.c.ID] = true
			}
		}
	}
	var fresh []*removal
	for _, c := range removing {
		r := a.removals[c.ID]
		if r == nil {
			r = a.startRemoval(ctx, c, urgent[c.ID])
			fresh = append(fresh, r)
		} else if urgent[c.ID] {
			r.makeUrgent()
		}
		r.seen = true
	}
	waitUntil := time.Now().Add(removeWait)

	// Wanted containers that wait for nothing go first. The others wait for
	// their removals, up to removeWait, and are left pending if they are not
	// done by then.
	gone := func(c runtime.Container) bool {
		r := a.removals[c.ID]
		return r != nil && r.ended() && r.err == nil
	}
	order := make([]replicaKey, 0, len(wantKeys))
	var later []*removal
	for _, k := range wantKeys {
		if bs := blocks[k]; len(bs) > 0 {
			for _, b := range bs {
				later = append(later, a.removals[b.c.ID])
			}
			continue
		}
		order = append(order, k)
	}
	for _, k := range wantKeys {
		if len(blocks[k]) > 0 {
			order = append(order, k)
		}
	}
	waited := false
	created := map[string]bool{} // containers this pass created and started
	for _, k := range order {
		as := want[k]
		st := store.Status{Name: ContainerName(as.App, as.Index, as.Generation), App: as.App, Replica: as.Index, Generation: as.Generation}
		listed, ok := keep[k]
		if bs := blocks[k]; len(bs) > 0 {
			if !waited {
				waitRemovals(ctx, later, waitUntil)
				waited = true
			}
			var left []blocker
			for _, b := range bs {
				if !gone(b.c) {
					left = append(left, b)
				}
			}
			if len(left) > 0 {
				st.State, st.Error = "pending", waitingFor(left, a.removals).Error()
				if ok {
					st.ContainerID, st.State = listed.ID, listed.State
				}
				status = append(status, st)
				continue
			}
		}
		if ok {
			if err := a.supervise(ctx, as, listed, &st); err != nil {
				errs = append(errs, err)
			}
		} else if ls := a.launchWaiting(st.Name); ls != nil {
			// The last create failed less than its backoff ago.
			st.State, st.Error = "pending", ls.message()
		} else {
			id, state, err := a.create(ctx, as)
			st.ContainerID, st.State = id, state
			if state == "running" {
				afterStart(as, &st)
				delete(a.launches, st.Name)
				a.startProbe(ctx, as, id, st.Name)
				created[st.Name] = true
			}
			if err != nil {
				// A pull waiting out its backoff failed in an earlier
				// pass; the status keeps saying so, and the pass is fine.
				// A pull has its own backoff; any other failure to create
				// or start the container waits launchBackoff.
				var pe *pullError
				if !errors.As(err, &pe) || !pe.waiting {
					errs = append(errs, err)
				}
				st.Error = err.Error()
				if pe == nil && ctx.Err() == nil {
					st.Error = a.launchFailed(st.Name, err).message()
				}
			}
		}
		if len(as.Spec.Routes) > 0 && st.State == "running" && st.ContainerID != "" {
			if err := a.routePorts(ctx, as, listed, ok && listed.Running(), &st); err != nil {
				errs = append(errs, err)
			}
		}
		status = append(status, st)
	}

	// The removals: those that ended are done with, the others go on, and
	// their containers are reported until they are gone. The pass waits, up
	// to removeWait, for the ones it started, which most often stop at once;
	// one from an earlier pass has shown it is slow, and triggers a pass
	// when it ends.
	waitRemovals(ctx, fresh, waitUntil)
	leftRunning(fresh)
	named := make(map[string]bool, len(status))
	for _, st := range status {
		named[st.Name] = true
	}
	for _, c := range removing {
		st, err := a.collectRemoval(c, a.removals[c.ID])
		if err != nil {
			errs = append(errs, err)
		}
		// A wanted container that waits for this one's name is reported
		// already, as pending, with what it waits for.
		if st != nil && !named[st.Name] {
			status = append(status, *st)
		}
	}
	a.forgetRemovals()

	kept := make(map[string]bool, len(keep)+len(created))
	for _, c := range keep {
		kept[c.Name] = true
	}
	// The containers this pass started keep the monitors it started.
	for name := range created {
		kept[name] = true
	}
	for name := range a.unrouted {
		if !draining[name] {
			delete(a.unrouted, name)
		}
	}
	a.forget(kept)

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
// only when it is missing, so a replacement does not wait for the registry,
// with the assignment's registry credential if it has one, and a failed pull
// backs off (pull.go). It returns the container ID, the state the container
// is left in, and any error.
func (a *Agent) create(ctx context.Context, as store.Assignment) (string, string, error) {
	cfg := containerConfig(as, a.cfg.RouteBind)
	log := a.log.With("app", as.App, "replica", as.Index, "generation", as.Generation)
	if cfg.Routed && a.cfg.RouteBind == "" {
		return "", "pending", fmt.Errorf("%s: %w", cfg.Name, errNoRouteBind)
	}

	id, err := a.cfg.Runtime.Create(ctx, cfg)
	if errors.Is(err, runtime.ErrNotFound) {
		if perr := a.pull(ctx, as, cfg.Image, log); perr != nil {
			return "", "pending", fmt.Errorf("%s: %w", cfg.Name, perr)
		}
		id, err = a.cfg.Runtime.Create(ctx, cfg)
	}
	if errors.Is(err, runtime.ErrConflict) {
		// The name is taken. If the container there is this replica (it was
		// created after the list was read), use it.
		if c, ierr := a.cfg.Runtime.Inspect(ctx, cfg.Name); ierr == nil {
			if k, ok := parseLabels(c.Labels); ok && k == (replicaKey{as.App, as.Index, as.Generation}) {
				id, err = c.ID, nil
			}
		}
	}
	if err != nil {
		return "", "pending", fmt.Errorf("%s: %w", cfg.Name, err)
	}
	log.Info("created container", "container", shortID(id))
	if discarded, err := a.startOrDiscard(ctx, id); err != nil {
		if discarded {
			return "", "pending", fmt.Errorf("%s: %w", cfg.Name, err)
		}
		return id, "created", fmt.Errorf("%s: %w", cfg.Name, err)
	}
	log.Info("started container", "container", shortID(id))
	return id, "running", nil
}

// startOrDiscard starts a container. When the engine refuses with a conflict,
// such as "port is already allocated", the container is removed: after a
// failed start Docker can let a later start succeed without the published
// ports, and the replica would then look running while unreachable. The next
// pass creates a fresh container with the full port spec. discarded reports
// whether the container was removed.
func (a *Agent) startOrDiscard(ctx context.Context, id string) (discarded bool, err error) {
	err = a.cfg.Runtime.Start(ctx, id)
	if err == nil || !errors.Is(err, runtime.ErrConflict) {
		return false, err
	}
	if rerr := a.cfg.Runtime.Remove(ctx, id, true); rerr != nil && !errors.Is(rerr, runtime.ErrNotFound) {
		a.log.Warn("removing a container that failed to start", "container", shortID(id), "err", rerr)
		return false, err
	}
	return true, err
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
	if err := ctx.Err(); err != nil {
		// The agent is closing: the next one removes the container.
		return fmt.Errorf("removing %s: %w", c.Name, err)
	}
	if err := a.cfg.Runtime.Remove(ctx, c.ID, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return fmt.Errorf("removing %s: %w", c.Name, err)
	}
	log.Info("removed container", "app", c.Labels[runtime.LabelApp], "replica", c.Labels[runtime.LabelReplica],
		"generation", c.Labels[runtime.LabelGeneration])
	return nil
}

// errNoRouteBind is the error of a routed replica on a node that has no
// private address to publish its route ports on.
var errNoRouteBind = errors.New("this node has no private address, so it cannot run a routed replica")

// containerConfig is the container for one assignment. A routed app's route
// ports are published on ephemeral host ports on routeBind, beside any
// fixed host ports of the spec.
func containerConfig(as store.Assignment, routeBind string) runtime.ContainerConfig {
	cfg := runtime.ContainerConfig{
		Name:       ContainerName(as.App, as.Index, as.Generation),
		Image:      as.Spec.Image,
		Command:    as.Spec.Command,
		Env:        as.Spec.Env,
		CPU:        as.Spec.Resources.CPU,
		Memory:     int64(as.Spec.Resources.Memory),
		App:        as.App,
		Replica:    as.Index,
		Generation: as.Generation,
		Routed:     len(as.Spec.Routes) > 0,
	}
	for _, p := range as.Spec.Ports {
		cfg.Ports = append(cfg.Ports, runtime.PortBinding{HostIP: p.Bind, HostPort: p.Host, ContainerPort: p.Container, Protocol: p.Protocol})
	}
	for _, p := range as.Spec.RoutePorts() {
		cfg.Ports = append(cfg.Ports, runtime.PortBinding{HostIP: routeBind, ContainerPort: p, Protocol: "tcp", Ephemeral: true})
	}
	for _, v := range as.Spec.Volumes {
		cfg.Volumes = append(cfg.Volumes, runtime.VolumeMount{Name: v.Name, Path: v.Path})
	}
	return cfg
}

// routePorts fills in the host port of each route port of a running routed
// container. The ports come from the list when the container was running
// there already, and from an inspect when this pass started it, because the
// engine picks a new ephemeral port at each start.
func (a *Agent) routePorts(ctx context.Context, as store.Assignment, listed runtime.Container, useListed bool, st *store.Status) error {
	c := listed
	if !useListed {
		ci, err := a.cfg.Runtime.Inspect(ctx, st.ContainerID)
		if err != nil {
			return a.fail(st, st.Name, fmt.Errorf("reading its published ports: %w", err))
		}
		c = ci
	}
	st.RoutePorts = nil
	for _, p := range as.Spec.RoutePorts() {
		if hp := c.PublishedPort(a.cfg.RouteBind, p, "tcp"); hp > 0 {
			st.RoutePorts = append(st.RoutePorts, store.RoutePort{Port: p, HostPort: hp})
		}
	}
	return nil
}

// parseLabels reads a container's hivemind labels. ok is false when any of
// them is missing or malformed.
func parseLabels(l map[string]string) (k replicaKey, ok bool) {
	app := l[runtime.LabelApp]
	idx, err1 := strconv.Atoi(l[runtime.LabelReplica])
	gen, err2 := strconv.Atoi(l[runtime.LabelGeneration])
	if app == "" || err1 != nil || err2 != nil || idx < 0 || gen < 1 {
		return replicaKey{}, false
	}
	return replicaKey{app, idx, gen}, true
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
		return cmp.Or(cmp.Compare(x.app, y.app), cmp.Compare(x.index, y.index), cmp.Compare(x.gen, y.gen))
	})
	return keys
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
