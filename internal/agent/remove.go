package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/store"
)

// Removals. A pass does not stop extra containers itself: it hands each one
// to a removal of its own, which stops and deletes it in the background, at
// most removeSlots at a time. A container that ignores SIGTERM takes the
// whole StopTimeout to stop, and a node with many of them, after a delete or
// a scale down, would otherwise make every replica it has to start wait for
// all of them, one after another. The next passes know the removals in
// flight: they do not start one twice, they report the container until it is
// gone, so the server keeps its row, and they report a removal's error.
//
// Order is kept where it matters. A wanted container that needs what an
// extra one holds is started only once that one is gone: a host port it
// publishes, the app's volumes, which every container of the app mounts, or
// its name. Those removals skip the queue. A pass waits for them up to
// removeWait, and reports the container pending meanwhile; a removal that
// ends triggers a pass.
const (
	removeSlots = 8
	removeWait  = 2 * time.Second
)

// removal is the stop and removal of one extra container, in flight or
// ended. It is keyed by container ID in Agent.removals.
type removal struct {
	c      runtime.Container
	done   chan struct{} // closed when the removal has ended; err is set by then
	err    error
	urgent chan struct{} // closed when the removal may skip the queue
	hurry  bool          // urgent is closed
	cancel context.CancelFunc
	seen   bool // the container was listed in this pass
	// late is set once the pass that started the removal has stopped
	// waiting for it. A removal that ends late triggers a pass, to report
	// it; one that ends in its own pass does not, so a removal that fails
	// at once is tried again at the next interval, not over and over.
	late atomic.Bool
}

func (r *removal) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// makeUrgent lets the removal start without waiting for a slot.
func (r *removal) makeUrgent() {
	if !r.hurry {
		r.hurry = true
		close(r.urgent)
	}
}

// startRemoval starts the removal of c in the background. An urgent one does
// not wait for a slot.
func (a *Agent) startRemoval(ctx context.Context, c runtime.Container, urgent bool) *removal {
	rctx, cancel := context.WithCancel(ctx)
	r := &removal{c: c, done: make(chan struct{}), urgent: make(chan struct{}), cancel: cancel}
	if urgent {
		r.makeUrgent()
	}
	a.removals[c.ID] = r
	go func() {
		r.err = a.removeQueued(rctx, r)
		cancel()
		close(r.done)
		if r.late.Load() {
			a.Trigger()
		}
	}()
	return r
}

// leftRunning marks the removals in rs that have not ended as late.
func leftRunning(rs []*removal) {
	for _, r := range rs {
		if !r.ended() {
			r.late.Store(true)
		}
	}
}

// removeQueued waits for a slot, unless the removal is urgent, and removes
// the container.
func (a *Agent) removeQueued(ctx context.Context, r *removal) error {
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	case <-r.urgent:
	case <-ctx.Done():
		return fmt.Errorf("removing %s: %w", r.c.Name, ctx.Err())
	}
	return a.remove(ctx, r.c)
}

// waitRemovals waits until every removal in rs has ended, ctx ends, or
// deadline passes. It reports whether they all ended.
func waitRemovals(ctx context.Context, rs []*removal, deadline time.Time) bool {
	t := time.NewTimer(max(0, time.Until(deadline)))
	defer t.Stop()
	for _, r := range rs {
		select {
		case <-r.done:
		case <-t.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// collectRemoval reports what the pass says about c, whose removal is r: the
// error of a removal that failed, which the next pass tries again, or the
// container, still there while the removal goes on. A removal that ended
// well reports nothing.
func (a *Agent) collectRemoval(c runtime.Container, r *removal) (st *store.Status, err error) {
	k, _ := parseLabels(c.Labels)
	st = &store.Status{Name: c.Name, ContainerID: c.ID, App: c.Labels[runtime.LabelApp], Replica: k.index, Generation: k.gen, State: c.State}
	if !r.ended() {
		return st, nil
	}
	delete(a.removals, c.ID)
	if r.err == nil {
		return nil, nil
	}
	// The container still exists: report it, so the server keeps a replica
	// marked for removal until it is really gone.
	st.Error = r.err.Error()
	return st, r.err
}

// forgetRemovals drops the removals that ended whose container was not
// listed in this pass, and clears the seen marks.
func (a *Agent) forgetRemovals() {
	for id, r := range a.removals {
		if !r.seen && r.ended() {
			delete(a.removals, id)
		}
		r.seen = false
	}
}

// cancelRemovals stops every removal in flight and waits for it to end. The
// containers stay; the next agent finds them and removes them.
func (a *Agent) cancelRemovals() {
	for _, r := range a.removals {
		r.cancel()
	}
	for id, r := range a.removals {
		<-r.done
		delete(a.removals, id)
	}
}

// blocker is what a wanted container waits for before it is started: an
// extra container that holds something it needs, and what that is.
type blocker struct {
	c    runtime.Container
	what string
}

// blockersOf returns the extra containers, of those in removing, that hold
// something the container for as needs: a host port it publishes, its name,
// or, for an app with volumes, the app's volumes.
func blockersOf(as store.Assignment, removing []runtime.Container) []blocker {
	name := ContainerName(as.App, as.Index, as.Generation)
	var out []blocker
	for _, c := range removing {
		switch {
		case c.Name == name:
			out = append(out, blocker{c, "its name"})
		case len(as.Spec.Volumes) > 0 && c.Labels[runtime.LabelApp] == as.App:
			out = append(out, blocker{c, "the app's volumes"})
		default:
			if p, ok := sharedHostPort(as, c); ok {
				out = append(out, blocker{c, "host port " + p})
			}
		}
	}
	return out
}

// sharedHostPort returns a host port, as "8080/tcp", that c has published and
// as publishes. Addresses are not compared, so this is stricter than the
// engine.
func sharedHostPort(as store.Assignment, c runtime.Container) (string, bool) {
	for _, p := range as.Spec.Ports {
		proto := strings.ToLower(p.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		for _, q := range c.Ports {
			qp := strings.ToLower(q.Protocol)
			if qp == "" {
				qp = "tcp"
			}
			if p.Host > 0 && q.HostPort == p.Host && qp == proto {
				return fmt.Sprintf("%d/%s", p.Host, proto), true
			}
		}
	}
	return "", false
}

// waitingFor is the error of a wanted container that waits for bs to be
// removed.
func waitingFor(bs []blocker, removals map[string]*removal) error {
	b := bs[0]
	msg := fmt.Sprintf("waiting for container %s, which holds %s, to be removed", b.c.Name, b.what)
	if r := removals[b.c.ID]; r != nil && r.ended() && r.err != nil {
		msg += ": " + r.err.Error()
	}
	if len(bs) > 1 {
		msg += fmt.Sprintf(" (and %d more)", len(bs)-1)
	}
	return errors.New(msg)
}
