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
// rest of it. The restart count comes from the node's last status report in
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
// anything.
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
		if discarded, err := a.startOrDiscard(ctx, c.ID); err != nil {
			if discarded {
				st.ContainerID, st.State = "", "pending"
			}
			return a.fail(st, c.Name, err)
		}
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
			due = ci.FinishedAt.Add(backoff(rs.restarts))
		}
		if now.Before(due) {
			a.schedule(rs, due, st)
			return nil
		}
		if discarded, err := a.startOrDiscard(ctx, c.ID); err != nil {
			if discarded {
				st.ContainerID, st.State = "", "pending"
			}
			return a.fail(st, c.Name, err)
		}
		rs.restarts++
		a.clearSchedule(rs, st)
		log.Warn("restarted exited container", "restarts", rs.restarts, "exit_code", ci.ExitCode)
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
				due := res.UnhealthySince.Add(backoff(rs.restarts))
				if now.Before(due) {
					a.schedule(rs, due, st)
					return nil
				}
				if err := a.restartInPlace(ctx, c); err != nil {
					return a.fail(st, c.Name, err)
				}
				rs.restarts++
				a.clearSchedule(rs, st)
				st.Health, st.Ready = string(health.Starting), false
				log.Warn("restarted unhealthy container", "restarts", rs.restarts,
					"failures", res.Failures, "error", res.LastError)
				return nil
			}
			a.clearSchedule(rs, st)
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
}

// wake returns when a scheduled restart is next due, or zero if none is.
func (a *Agent) wake() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.wakeAt
}

// Close stops the health probes. Run calls it when it returns. A closed agent
// can still reconcile, and starts new probes as it needs them.
func (a *Agent) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for name := range a.probes {
		a.stopProbe(name)
	}
}
