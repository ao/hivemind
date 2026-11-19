package health

import (
	"context"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// State is what a Tracker reports for a replica.
type State string

const (
	// Starting means no probe has passed yet.
	Starting State = "starting"
	// Healthy means the last probe passed.
	Healthy State = "healthy"
	// Unhealthy means Retries probes failed in a row, after the start period.
	Unhealthy State = "unhealthy"
)

// Tracker turns probe results into a state. A replica becomes unhealthy after
// Retries consecutive failures. A failure during StartPeriod, measured from
// Started, is ignored: the replica is still starting up.
type Tracker struct {
	retries     int
	startPeriod time.Duration
	started     time.Time

	state          State
	failures       int
	unhealthySince time.Time
	lastErr        string
}

// NewTracker returns a tracker for a container started at started.
func NewTracker(h spec.Health, started time.Time) *Tracker {
	retries := h.Retries
	if retries < 1 {
		retries = 3
	}
	return &Tracker{retries: retries, startPeriod: h.StartPeriod.D(), started: started, state: Starting}
}

// Observe records one probe result at time now, nil when it passed.
func (t *Tracker) Observe(now time.Time, err error) State {
	if err == nil {
		t.state = Healthy
		t.failures = 0
		t.lastErr = ""
		t.unhealthySince = time.Time{}
		return t.state
	}
	// The failure is kept even when it does not count: it is why the
	// replica is not ready yet, which its status shows.
	t.lastErr = truncate(err.Error(), maxErrLen)
	if now.Sub(t.started) < t.startPeriod {
		// Still inside the start period: the failure does not count.
		return t.state
	}
	t.failures++
	if t.failures >= t.retries && t.state != Unhealthy {
		t.state = Unhealthy
		t.unhealthySince = now
	}
	return t.state
}

// State returns the current state.
func (t *Tracker) State() State { return t.state }

const maxErrLen = 200

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Result is a Monitor's latest view of its replica.
type Result struct {
	State State
	// Failures is the number of consecutive counted failures.
	Failures int
	// UnhealthySince is when the replica became unhealthy; zero otherwise.
	UnhealthySince time.Time
	// LastError is the last failure, truncated, one in the start period
	// too, which does not count. Empty after a pass.
	LastError string
	// CheckedAt is when the last probe finished; zero before the first.
	CheckedAt time.Time
}

// Monitor runs one replica's probe every interval and keeps the result. It is
// safe for concurrent use: Run writes, Result reads.
type Monitor struct {
	h       spec.Health
	target  Target
	rt      Execer
	now     func() time.Time
	started time.Time
	notify  func()
	mu      sync.Mutex // guards tr and checked
	tr      *Tracker
	checked time.Time
}

// Notify makes Run call f, outside the monitor's lock, after each check that
// changed the state. Call it before Run.
func (m *Monitor) Notify(f func()) { m.notify = f }

// NewMonitor returns a monitor for a container started at started. now is the
// clock the start period is measured with; nil means time.Now.
func NewMonitor(h spec.Health, target Target, rt Execer, started time.Time, now func() time.Time) *Monitor {
	if now == nil {
		now = time.Now
	}
	return &Monitor{h: h, target: target, rt: rt, now: now, started: started, tr: NewTracker(h, started)}
}

// Run probes until ctx is cancelled, every interval from one interval after
// the container started: a monitor started after that, as by an agent that
// restarted, or one that started many containers in one long pass, checks
// at once. Before, the first check was one interval after the monitor
// started, and in the second scale run a node that started 93 containers in
// one 21 s pass had its health-checked ones ready 9 s late. A probe that is
// cut short by the cancellation is not recorded.
func (m *Monitor) Run(ctx context.Context) {
	interval := m.h.Interval.D()
	if interval <= 0 {
		interval = 10 * time.Second
	}
	// A start time ahead of this machine's clock waits one interval at most.
	first := min(max(m.started.Add(interval).Sub(m.now()), 0), interval)
	wait := time.NewTimer(first)
	select {
	case <-ctx.Done():
		wait.Stop()
		return
	case <-wait.C:
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		err := Check(ctx, m.h, m.target, m.rt)
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		before := m.tr.state
		after := m.tr.Observe(m.now(), err)
		m.checked = m.now()
		m.mu.Unlock()
		if after != before && m.notify != nil {
			m.notify()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// AssumeHealthy starts the monitor as healthy rather than starting. An agent
// that restarted uses it for a container its last report called healthy,
// which has run on since. Failures from the next checks count as usual, so
// a container that broke meanwhile becomes unhealthy after Retries of them.
func (m *Monitor) AssumeHealthy() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tr.state == Starting && m.checked.IsZero() {
		m.tr.state = Healthy
	}
}

// Result returns the latest view.
func (m *Monitor) Result() Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Result{
		State:          m.tr.state,
		Failures:       m.tr.failures,
		UnhealthySince: m.tr.unhealthySince,
		LastError:      m.tr.lastErr,
		CheckedAt:      m.checked,
	}
}
