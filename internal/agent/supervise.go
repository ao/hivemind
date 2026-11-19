package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/ao/hivemind/internal/health"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// Restart policy. A container that exits, or fails its health check, waits
// backoff(n) before its next restart, where n is the restarts it already had:
// 1 s, 2 s, 4 s and so on, up to 5 minutes. A container that has run for
// resetAfter since its last start, and passes its health check, starts over
// at 1 s.
//
// The waits are measured from the container's FinishedAt, which the engine
// keeps, so an agent that restarts in the middle of a wait still waits the
// rest of it. A FinishedAt later than the agent's clock, after the clock was
// set back, counts as now, so a wait is never longer than backoff(n). The
// restart count comes from the node's last status report in
// the store, read once at start-up. Together these mean a restarted agent
// never restarts a crashing container faster than the policy allows.
const (
	restartBase = time.Second
	restartCap  = 5 * time.Minute
	resetAfter  = 10 * time.Minute
)

// backoff returns how long a container that has been restarted restarts
// times waits before its next restart.
func backoff(restarts int) time.Duration {
	d := restartBase
	for range restarts {
		if d >= restartCap/2 {
			return restartCap
		}
		d *= 2
	}
	if d > restartCap {
		return restartCap
	}
	return d
}

// restartState is what the agent knows about one container's restarts. It is
// keyed by container name, and dropped when the container is no longer kept.
type restartState struct {
	restarts    int
	nextRestart time.Time // zero when no restart is scheduled
	// futureExit is the exit time, by the engine, of a container that
	// exited later than the agent's clock says it is now (the clock was set
	// back), and futureSeen is when the agent first saw it: the wait counts
	// from then, and stays put on later passes.
	futureExit time.Time
	futureSeen time.Time
	// healthFailure is why the agent last restarted the container for
	// failing its health check. The restarted container's monitor starts
	// with nothing to say, so its status keeps this until its own check
	// passes or fails.
	healthFailure string
}

// probe is the health monitor of one running container.
type probe struct {
	mon    *health.Monitor
	cancel context.CancelFunc
}

// statusReader is implemented by sources that can return the node's last
// status report. The store implements it. A source that does not is fine:
// restart counts then start from zero after an agent restart.
type statusReader interface {
	StatusForNode(ctx context.Context, nodeID string) ([]store.Status, error)
}

func (a *Agent) restartsOf(name string) *restartState {
	rs := a.restarts[name]
	if rs == nil {
		rs = &restartState{}
		a.restarts[name] = rs
	}
	return rs
}

// seedRestarts loads the restart counts from the node's last status report.
// It runs once per agent, before the first pass that creates or restarts
// anything. It also remembers which containers that report called healthy,
// by container ID: a restarted agent's new probe starts from that, rather
// than from "starting", so a container that kept running while the agent
// restarted (an upgrade of hivemind) stays ready, and in the routing table.
// Without it, every routed replica with a health check on the node left the
// table until its first check passed again, and an upgrade of every node at
// once answered 503 for a few seconds.
func (a *Agent) seedRestarts(ctx context.Context) error {
	sr, ok := a.cfg.Source.(statusReader)
	if !ok {
		return nil
	}
	reported, err := sr.StatusForNode(ctx, a.cfg.NodeID)
	if err != nil {
		return err
	}
	for _, x := range reported {
		if x.RestartCount > 0 {
			a.restartsOf(x.Name).restarts = x.RestartCount
		}
		if x.ContainerID != "" && x.State == "running" && x.Health == string(health.Healthy) && x.Ready {
			a.healthy[x.ContainerID] = true
		}
	}
	return nil
}

// supervise moves one kept container to the state its replica wants, and
// fills in the status fields for it. It only returns an error for a failed
// engine call; a container waiting out its backoff is not an error.
func (a *Agent) supervise(ctx context.Context, as store.Assignment, c runtime.Container, st *store.Status) error {
	now := a.cfg.Clock()
	rs := a.restartsOf(c.Name)
	log := a.log.With("app", as.App, "replica", as.Index, "generation", as.Generation, "container", shortID(c.ID))
	st.Name, st.ContainerID, st.State = c.Name, c.ID, c.State
	defer func() { st.RestartCount = rs.restarts }()

	switch c.State {
	case "created":
		a.stopProbe(c.Name)
		if ls := a.launchWaiting(c.Name); ls != nil {
			st.Error = ls.message()
			return nil
		}
		if discarded, err := a.startOrDiscard(ctx, c.ID); err != nil {
			if discarded {
				st.ContainerID, st.State = "", "pending"
			}
			err = a.fail(st, c.Name, err)
			if ctx.Err() == nil {
				st.Error = a.launchFailed(c.Name, err).message()
			}
			return err
		}
		delete(a.launches, c.Name)
		log.Info("started container", "was", c.State)
		st.State = "running"
		afterStart(as, st)

	case "exited":
		a.stopProbe(c.Name)
		ci, err := a.cfg.Runtime.Inspect(ctx, c.ID)
		if err != nil {
			return a.fail(st, c.Name, err)
		}
		due := now
		if !ci.FinishedAt.IsZero() {
			finished := ci.FinishedAt
			if finished.After(now) {
				// The container stopped "in the future": the clock was set
				// back after it stopped, by NTP after a boot, by hand, or by
				// restoring a VM. Waiting from that time would hold the
				// restart for as long as the clock was ahead; wait from
				// when the agent first saw it instead.
				if !rs.futureExit.Equal(finished) {
					rs.futureExit, rs.futureSeen = finished, now
					log.Warn("the container stopped after this machine's clock says it is now; the clock was set back, so its restart waits from now",
						"finished_at", finished.UTC(), "now", now.UTC())
				}
				finished = rs.futureSeen
			}
			due = finished.Add(backoff(rs.restarts))
		}
		if now.Before(due) {
			a.schedule(rs, due, st)
			st.Error = exitReason(ci, as)
			return nil
		}
		if ls := a.launchWaiting(c.Name); ls != nil {
			st.Error = ls.message()
			return nil
		}
		if discarded, err := a.startOrDiscard(ctx, c.ID); err != nil {
			if discarded {
				st.ContainerID, st.State = "", "pending"
			}
			err = a.fail(st, c.Name, err)
			if ctx.Err() == nil {
				st.Error = a.launchFailed(c.Name, err).message()
			}
			return err
		}
		delete(a.launches, c.Name)
		rs.restarts++
		a.clearSchedule(rs, st)
		log.Warn("restarted exited container", "restarts", rs.restarts, "exit_code", ci.ExitCode, "oom_killed", ci.OOMKilled)
		st.State = "running"
		afterStart(as, st)

	case "running":
		if as.Spec.Health == nil {
			a.clearSchedule(rs, st)
			st.Ready = true
		} else {
			p, err := a.probeFor(ctx, c, *as.Spec.Health)
			if err != nil {
				return a.fail(st, c.Name, err)
			}
			res := p.mon.Result()
			st.Health, st.Ready = string(res.State), res.State == health.Healthy
			if res.State == health.Unhealthy {
				failed := fmt.Sprintf("failed its health check %d times in a row", res.Failures)
				if res.LastError != "" {
					failed += ": " + res.LastError
				}
				due := res.UnhealthySince.Add(backoff(rs.restarts))
				if now.Before(due) {
					a.schedule(rs, due, st)
					st.Error = failed
					return nil
				}
				if err := a.restartInPlace(ctx, c); err != nil {
					return a.fail(st, c.Name, err)
				}
				rs.restarts++
				a.clearSchedule(rs, st)
				st.Health, st.Ready = string(health.Starting), false
				rs.healthFailure = "restarted after it " + failed
				st.Error = rs.healthFailure
				log.Warn("restarted unhealthy container", "restarts", rs.restarts,
					"failures", res.Failures, "error", res.LastError)
				return nil
			}
			a.clearSchedule(rs, st)
			// A replica that is not ready says why: the last check's
			// failure, or, until the restarted container's own check
			// says more, the one it was restarted for. Without it, an
			// HTTP check on a server that speaks only h2c showed
			// "starting" and no reason for minutes (fourth acceptance
			// run), the reason only in this node's log.
			switch {
			case st.Ready:
				rs.healthFailure = ""
			case res.LastError != "":
				st.Error = "its last health check failed: " + res.LastError
			case rs.healthFailure != "":
				st.Error = rs.healthFailure
			}
		}

		// Reset the count once the container has run healthy for long enough.
		if rs.restarts > 0 && st.Ready {
			ci, err := a.cfg.Runtime.Inspect(ctx, c.ID)
			if err != nil {
				log.Warn("reading start time", "err", err)
			} else if !ci.StartedAt.IsZero() && now.Sub(ci.StartedAt) >= resetAfter {
				log.Info("restart count reset after healthy running", "was", rs.restarts)
				rs.restarts = 0
			}
		}
	}
	return nil
}

// exitReason says why an exited container stopped, for its status while it
// waits for its restart: its exit code, or that the kernel killed it for
// going over its memory limit.
func exitReason(ci runtime.Container, as store.Assignment) string {
	if ci.OOMKilled {
		if m := as.Spec.Resources.Memory; m > 0 {
			return fmt.Sprintf("killed for going over its memory limit of %s (out of memory)", m)
		}
		return "killed by the kernel for lack of memory (out of memory)"
	}
	return fmt.Sprintf("exited with code %d", ci.ExitCode)
}

// afterStart sets the readiness of a container that has just been started. A
// replica without a health check is ready as soon as it runs. With one, it is
// "starting" and not ready until a probe passes.
func afterStart(as store.Assignment, st *store.Status) {
	st.Ready = as.Spec.Health == nil
	st.Health = ""
	if as.Spec.Health != nil {
		st.Health = string(health.Starting)
	}
}

// schedule records that the next restart of a failed container is due at due.
func (a *Agent) schedule(rs *restartState, due time.Time, st *store.Status) {
	rs.nextRestart = due
	st.CrashLooping, st.NextRestart = true, due
}

func (a *Agent) clearSchedule(rs *restartState, st *store.Status) {
	rs.nextRestart = time.Time{}
	st.CrashLooping, st.NextRestart = false, time.Time{}
}

// fail records err in the status of one container and returns it.
func (a *Agent) fail(st *store.Status, name string, err error) error {
	err = fmt.Errorf("%s: %w", name, err)
	st.Error = err.Error()
	return err
}

// restartInPlace stops a running container and starts it again. The probe
// monitor is dropped, because the container's start time changes.
func (a *Agent) restartInPlace(ctx context.Context, c runtime.Container) error {
	if err := a.cfg.Runtime.Stop(ctx, c.ID, a.cfg.StopTimeout); err != nil {
		return err
	}
	if _, err := a.startOrDiscard(ctx, c.ID); err != nil {
		return err
	}
	a.stopProbe(c.Name)
	return nil
}

// probeFor returns the running container's health monitor, starting one if
// there is none. The probes run in their own goroutines, so a slow or
// hanging check never holds up a pass.
func (a *Agent) probeFor(ctx context.Context, c runtime.Container, h spec.Health) (*probe, error) {
	if p := a.probes[c.Name]; p != nil {
		return p, nil
	}
	ci, err := a.cfg.Runtime.Inspect(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithCancel(context.Background())
	mon := health.NewMonitor(h, health.Target{ContainerID: c.ID, IP: ci.IPAddress}, a.cfg.Runtime, ci.StartedAt, a.cfg.Clock)
	if a.healthy[c.ID] {
		// The same container, healthy in the last report before this agent
		// started: its next check decides, as it would have.
		mon.AssumeHealthy()
	}
	delete(a.healthy, c.ID)
	p := &probe{mon: mon, cancel: cancel}
	a.probes[c.Name] = p
	go mon.Run(pctx)
	return p, nil
}

// stopProbe stops the monitor of a container, if it has one.
func (a *Agent) stopProbe(name string) {
	if p := a.probes[name]; p != nil {
		p.cancel()
		delete(a.probes, name)
	}
}

// forget drops the restart state and probes of containers that are not kept
// this pass, and records when the next scheduled restart is due.
func (a *Agent) forget(kept map[string]bool) {
	// Only the first pass after a start can use the last report's health;
	// later passes have their own probes.
	clear(a.healthy)
	for name := range a.restarts {
		if !kept[name] {
			delete(a.restarts, name)
		}
	}
	for name := range a.probes {
		if !kept[name] {
			a.stopProbe(name)
		}
	}
	a.wakeAt = time.Time{}
	for _, rs := range a.restarts {
		if !rs.nextRestart.IsZero() && (a.wakeAt.IsZero() || rs.nextRestart.Before(a.wakeAt)) {
			a.wakeAt = rs.nextRestart
		}
	}
	// A routed container waiting out its drain is stopped when the wait ends.
	for _, first := range a.unrouted {
		if due := first.Add(a.cfg.RouteDrain); a.wakeAt.IsZero() || due.Before(a.wakeAt) {
			a.wakeAt = due
		}
	}
	// A pull waiting out its backoff is tried when the wait ends, and so is
	// a create or a start.
	for _, due := range []time.Time{a.forgetPulls(), a.forgetLaunches()} {
		if !due.IsZero() && (a.wakeAt.IsZero() || due.Before(a.wakeAt)) {
			a.wakeAt = due
		}
	}
}

// wake returns when a scheduled restart is next due, or zero if none is.
func (a *Agent) wake() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.wakeAt
}

// Close stops the health probes and the pulls in flight, and the removals in
// flight, which it waits for. Run calls it when it returns. A closed agent
// can still reconcile, and starts new probes, pulls and removals as it needs
// them.
func (a *Agent) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for name := range a.probes {
		a.stopProbe(name)
	}
	a.cancelPulls()
	a.cancelRemovals()
}
