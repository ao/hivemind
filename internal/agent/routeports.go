package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/store"
)

// Route host ports. A routed app's container publishes each route port on a
// host port of the node's private address that the agent picks when it
// creates the container, from RoutePortFirst to RoutePortLast, as a fixed
// binding, and records in its hivemind.route-ports label. The engine binds a
// fixed host port again at every start, so a replica has its ports for its
// container's whole life: when the container is started again, after it
// exits or after its machine restarts, a router that still has an old table
// reaches that replica, not another container that the port went to. Route
// ports were ephemeral before. In the sixth soak the server's machine
// restarted while its workers could not reach it; its replicas came back on
// each other's ports, and the workers' routers, serving their last table,
// sent one app's requests to another (a 200 from the wrong app) and gRPC
// calls to an HTTP/1.1 server (502).
//
// The range is below the kernel's ephemeral range (net.ipv4.ip_local_port_range,
// 32768 to 60999 by default), which the source ports of outgoing connections
// and Docker's own ephemeral host ports come from, so neither takes one of
// these ports while its container is stopped. Kubernetes (NodePorts) and
// Docker Swarm (published ports) use the same range for the same reason. A
// port that something else holds when the container starts makes the start
// fail with "port is already allocated" or "address already in use"; the
// agent then removes the container (startOrDiscard), and the next pass
// creates it on another port.
//
// A port is not handed out again until no container on the node has held it
// for routePortQuiet, and the ports are handed out in turn round the range,
// so a port freed by a removed container goes to another container only
// after the rest of the range has been used. A router whose table is that
// old would otherwise reach the new container. The record is in memory: an
// agent that starts knows the ports its containers hold, not the ones freed
// just before it started, and starts after the highest port its containers
// hold.
const (
	RoutePortFirst = 30000
	RoutePortLast  = 32767
	routePortQuiet = 10 * time.Minute
)

// routePortState is the agent's record of the host ports in the route range.
type routePortState struct {
	// seen is when each port was last held by a container on the node, by
	// its label or as published, or handed out to a new one.
	seen map[int]time.Time
	// last is the port handed out last, or the highest one held when the
	// agent started; 0 until the first pass that lists the containers.
	last int
	// spec is, for this pass, every host port in the spec of an app
	// assigned to the node: never handed out, so a routed container does
	// not take the port of an app that is to run there.
	spec map[int]bool
}

func inRouteRange(p int) bool { return p >= RoutePortFirst && p <= RoutePortLast }

// notePorts records the route range ports that the node's containers hold,
// stopped ones by their label, and the spec ports of the wanted
// assignments. Reconcile calls it after each list.
func (a *Agent) notePorts(containers []runtime.Container, want map[replicaKey]store.Assignment) {
	rp := &a.ports
	if rp.seen == nil {
		rp.seen = map[int]time.Time{}
	}
	now := a.cfg.Clock()
	top := 0
	hold := func(p int) {
		if inRouteRange(p) {
			rp.seen[p] = now
			top = max(top, p)
		}
	}
	for _, c := range containers {
		if hp, ok := runtime.RouteHostPorts(c.Labels); ok {
			for p := range maps.Values(hp) {
				hold(p)
			}
		}
		for _, p := range c.Ports {
			hold(p.HostPort)
		}
	}
	for p, t := range rp.seen {
		if now.Sub(t) >= routePortQuiet {
			delete(rp.seen, p)
		}
	}
	if rp.last == 0 {
		rp.last = max(top, RoutePortFirst-1)
	}
	rp.spec = map[int]bool{}
	for _, as := range want {
		for _, p := range as.Spec.Ports {
			rp.spec[p.Host] = true
		}
	}
}

// pickRoutePorts hands out n host ports for a new routed container: the next
// ones after the last handed out, round the range, that no container on the
// node has held for routePortQuiet and no assigned app's spec names.
func (a *Agent) pickRoutePorts(n int) ([]int, error) {
	rp := &a.ports
	if rp.seen == nil {
		rp.seen = map[int]time.Time{}
	}
	now := a.cfg.Clock()
	out := make([]int, 0, n)
	p := rp.last
	for range RoutePortLast - RoutePortFirst + 1 {
		if len(out) == n {
			break
		}
		p++
		if p < RoutePortFirst || p > RoutePortLast {
			p = RoutePortFirst
		}
		if t, ok := rp.seen[p]; (ok && now.Sub(t) < routePortQuiet) || rp.spec[p] {
			continue
		}
		out = append(out, p)
	}
	if len(out) < n {
		return nil, fmt.Errorf("no host port is free for a routed replica: every port from %d to %d on %s is in use, or was in the last %s",
			RoutePortFirst, RoutePortLast, a.cfg.RouteBind, routePortQuiet)
	}
	for _, p := range out {
		rp.seen[p] = now
	}
	rp.last = out[len(out)-1]
	return out, nil
}

// fixedRoutePorts reports whether c, a container of the routed replica as,
// has its route ports on host ports of its own: its label names a host port
// for each of the spec's route ports. A routed container made by an earlier
// build has ephemeral ports, which the engine picks again at each start; the
// agent replaces such a container rather than start it again.
func fixedRoutePorts(as store.Assignment, c runtime.Container) bool {
	hp, ok := runtime.RouteHostPorts(c.Labels)
	if !ok {
		return false
	}
	want := as.Spec.RoutePorts()
	if len(hp) != len(want) {
		return false
	}
	for _, p := range want {
		if _, ok := hp[p]; !ok {
			return false
		}
	}
	return true
}

// replaceForPorts removes c, a container of the routed replica as that an
// earlier build made with ephemeral route ports, and creates the replica's
// container again, with host ports of its own (launch). Starting c again
// would publish its route ports on new ephemeral ports, which a router with
// an old table could take for another replica's. Supervise calls it where
// it would start c again: after c exited, and when c failed its health
// check. The new container has the replica's name, so its restart count
// carries over.
func (a *Agent) replaceForPorts(ctx context.Context, as store.Assignment, c runtime.Container, st *store.Status, log *slog.Logger) error {
	a.stopProbe(c.Name)
	if c.Running() {
		if err := a.cfg.Runtime.Stop(ctx, c.ID, a.cfg.StopTimeout); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return a.fail(st, c.Name, err)
		}
	}
	if err := a.cfg.Runtime.Remove(ctx, c.ID, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return a.fail(st, c.Name, err)
	}
	log.Info("removed a routed container with ephemeral host ports, made by an earlier build, rather than start it again; its replacement gets host ports of its own")
	st.ContainerID, st.State, st.Ready, st.Health = "", "pending", false, ""
	return a.launch(ctx, as, st)
}
