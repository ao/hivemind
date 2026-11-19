package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

const routeAddr = "10.0.0.5"

// newRoutedEnv is newEnv on a node with a private address, and an agent that
// publishes route ports there.
func newRoutedEnv(t *testing.T, bind string) *env {
	t.Helper()
	e := newEnv(t)
	if err := e.st.UpsertNode(ctxT(t), store.Node{ID: nodeID, Name: "one", Address: routeAddr}); err != nil {
		t.Fatal(err)
	}
	e.newRoutedAgent(bind)
	e.rt.images["docker.io/library/nginx:alpine"] = true
	return e
}

// newRoutedAgent replaces the agent with a new one that publishes route
// ports on bind, as a restart of hivemind does.
func (e *env) newRoutedAgent(bind string) {
	e.t.Helper()
	e.logs = &syncBuffer{}
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Clock: e.clock.Now, RouteBind: bind,
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		e.t.Fatal(err)
	}
	e.agent = a
}

func routedApp(replicas int, env string) spec.App {
	return spec.App{Name: "web", Image: "nginx:alpine", Replicas: replicas, Env: map[string]string{"V": env},
		Routes: []spec.Route{{Host: "www.example.com", Port: 80}, {Host: "api.example.com", Port: 8080}, {Host: "w2.example.com", Port: 80}}}
}

// routeBindings returns the route bindings a container was created with.
func routeBindings(c fakeContainer) []runtime.PortBinding {
	var out []runtime.PortBinding
	for _, p := range c.cfg.Ports {
		if p.Route {
			out = append(out, p)
		}
	}
	return out
}

// Each routed container publishes its route ports on host ports that the
// agent picks from the route range, as fixed bindings that its label
// records, and keeps them when it exits and is started again, and when
// every container on the node stops at once, as when the machine restarts.
// They were ephemeral before, and each start gave a container new ones: in
// the sixth soak the server's replicas came back on each other's ports, and
// routers with the old table sent one app's requests to another.
func TestRoutedContainersKeepTheirHostPorts(t *testing.T) {
	e := newRoutedEnv(t, routeAddr)
	e.put(routedApp(2, "1"))
	e.reconcile()

	run := e.running()
	if len(run) != 2 {
		t.Fatalf("running: %v", keysOf(run))
	}
	used := map[int]bool{}
	for name, c := range run {
		if c.Labels[runtime.LabelRouted] != "true" {
			t.Errorf("%s has no routed label: %v", name, c.Labels)
		}
		rb := routeBindings(c)
		if len(rb) != 2 || rb[0].ContainerPort != 80 || rb[1].ContainerPort != 8080 {
			t.Fatalf("%s route bindings: %+v", name, rb)
		}
		for _, p := range rb {
			if p.HostIP != routeAddr || !inRouteRange(p.HostPort) || used[p.HostPort] {
				t.Errorf("%s publishes %d on %s:%d; host ports used already: %v", name, p.ContainerPort, p.HostIP, p.HostPort, used)
			}
			used[p.HostPort] = true
		}
		if got, want := c.Labels[runtime.LabelRoutePorts], fmt.Sprintf("80:%d,8080:%d", rb[0].HostPort, rb[1].HostPort); got != want {
			t.Errorf("%s: label %s = %q, want %q", name, runtime.LabelRoutePorts, got, want)
		}
	}
	first := routePortsByName(e.status("web"))
	for name, rp := range first {
		rb := routeBindings(run[name])
		if len(rp) != 2 || rp[0] != (store.RoutePort{Port: 80, HostPort: rb[0].HostPort}) || rp[1] != (store.RoutePort{Port: 8080, HostPort: rb[1].HostPort}) {
			t.Fatalf("%s reported route ports %+v, created with %+v", name, rp, rb)
		}
	}
	// The next pass reads the same ports from the list.
	e.reconcile()
	if again := routePortsByName(e.status("web")); !equalPorts(first, again) {
		t.Fatalf("ports changed without a restart: %v then %v", first, again)
	}

	// A container that exits is started again, on the same ports.
	var id, name string
	for n, c := range e.running() {
		id, name = c.ID, n
		break
	}
	e.rt.exit(id, 1)
	e.clock.Advance(2 * time.Second)
	e.reconcile()
	if after := routePortsByName(e.status("web")); !equalPorts(first, after) {
		t.Fatalf("after %s was started again: %v, before %v", name, after, first)
	}
	if got := e.running()[name].ID; got != id {
		t.Fatalf("%s was replaced (%s), not started again (%s)", name, shortID(got), shortID(id))
	}

	// Every container stops, as when the node's machine restarts, and a new
	// agent starts them again, on the same ports.
	for _, c := range e.running() {
		e.rt.exit(c.ID, 0)
	}
	e.newRoutedAgent(routeAddr)
	e.clock.Advance(10 * time.Second)
	e.reconcile()
	if after := routePortsByName(e.status("web")); len(e.running()) != 2 || !equalPorts(first, after) {
		t.Fatalf("after every container stopped and a new agent started: %v running, ports %v, before %v", keysOf(e.running()), after, first)
	}
}

// A container whose host port something else took while it was stopped is
// not started without it: the start fails, the container is removed, and
// the replica's next container gets other ports. The ones it had are not
// handed out again soon.
func TestRoutePortTakenWhileStoppedGetsAnother(t *testing.T) {
	e := newRoutedEnv(t, routeAddr)
	e.put(routedApp(1, "1"))
	e.reconcile()
	name := ContainerName("web", 0, 1)
	c := e.running()[name]
	old := routeBindings(c)
	e.rt.exit(c.ID, 1)
	e.rt.hold(old[0].HostPort)
	e.clock.Advance(2 * time.Second)
	if err := e.agent.Reconcile(ctxT(t)); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("Reconcile: %v, want ErrConflict", err)
	}
	if snap := e.rt.snapshot(); len(snap) != 0 {
		t.Fatalf("the container that could not start is kept: %v", names(snap))
	}
	if st := e.status("web"); len(st) != 1 || st[0].State != "pending" || !strings.Contains(st[0].Error, "address already in use") {
		t.Fatalf("status after the refused start: %+v", st)
	}

	e.clock.Advance(launchBase)
	e.reconcile()
	c, ok := e.running()[name]
	if !ok {
		t.Fatalf("no new container: %v", keysOf(e.running()))
	}
	now := routeBindings(c)
	for _, p := range now {
		if !inRouteRange(p.HostPort) || p.HostPort == old[0].HostPort || p.HostPort == old[1].HostPort {
			t.Fatalf("the new container publishes %+v; the old one had %+v", now, old)
		}
	}
	rp := routePortsByName(e.status("web"))[name]
	if len(rp) != 2 || rp[0].HostPort != now[0].HostPort || rp[1].HostPort != now[1].HostPort {
		t.Fatalf("reported %+v, published %+v", rp, now)
	}
}

// The ports are handed out in turn round the range, skipping those held or
// freed less than routePortQuiet ago and those of the assigned apps' specs;
// with none left, the error says why.
func TestPickRoutePorts(t *testing.T) {
	e := newRoutedEnv(t, routeAddr)
	a := e.agent
	now := e.clock.Now()
	a.ports.seen = map[int]time.Time{
		RoutePortLast:  now.Add(-time.Minute),    // freed a minute ago
		RoutePortFirst: now.Add(-routePortQuiet), // freed long enough ago
	}
	a.ports.last = RoutePortLast - 1
	a.ports.spec = map[int]bool{RoutePortFirst + 1: true}
	if got, err := a.pickRoutePorts(2); err != nil || !slices.Equal(got, []int{RoutePortFirst, RoutePortFirst + 2}) {
		t.Fatalf("pick 2: %v, %v", got, err)
	}
	if got, err := a.pickRoutePorts(1); err != nil || !slices.Equal(got, []int{RoutePortFirst + 3}) {
		t.Fatalf("pick 1: %v, %v", got, err)
	}
	for p := RoutePortFirst; p <= RoutePortLast; p++ {
		a.ports.seen[p] = now
	}
	if got, err := a.pickRoutePorts(1); err == nil || !strings.Contains(err.Error(), "no host port is free for a routed replica") {
		t.Fatalf("with every port held: %v, %v", got, err)
	}
}

// A routed container does not take a host port that an app assigned to the
// node publishes, and a new agent goes on after the highest route port its
// node's containers hold, stopped ones included.
func TestRoutePortsSkipSpecPortsAndHeldOnes(t *testing.T) {
	e := newRoutedEnv(t, routeAddr)
	e.put(spec.App{Name: "fixed", Image: "nginx:alpine", Ports: []spec.Port{{Host: RoutePortFirst, Container: 80}}})
	e.put(routedApp(2, "1"))
	e.reconcile()
	held := map[int]bool{}
	top := 0
	for name, c := range e.running() {
		if name == ContainerName("fixed", 0, 1) {
			continue
		}
		for _, p := range routeBindings(c) {
			if p.HostPort == RoutePortFirst {
				t.Fatalf("%s took %d, the fixed app's host port", name, p.HostPort)
			}
			held[p.HostPort] = true
			top = max(top, p.HostPort)
		}
	}
	if len(held) != 4 {
		t.Fatalf("route host ports %v", held)
	}

	// Every container stops, a new agent starts, and the app gets a third
	// replica.
	for _, c := range e.running() {
		e.rt.exit(c.ID, 0)
	}
	e.newRoutedAgent(routeAddr)
	e.put(routedApp(3, "1"))
	e.clock.Advance(10 * time.Second)
	e.reconcile()
	run := e.running()
	if len(run) != 4 {
		t.Fatalf("running: %v", keysOf(run))
	}
	for name, c := range run {
		for _, p := range routeBindings(c) {
			if !held[p.HostPort] && p.HostPort <= top {
				t.Errorf("%s got %d, at or below %d, the highest port held when the agent started", name, p.HostPort, top)
			}
		}
	}
}

// A routed container that an earlier build made, with ephemeral route
// ports, runs on as it is, with the ports the engine gave it. When it exits
// it is replaced by a container with host ports of its own, not started
// again on new ephemeral ones; so is one that was created and never started.
func TestEphemeralRoutedContainerIsReplacedAtItsRestart(t *testing.T) {
	e := newRoutedEnv(t, routeAddr)
	e.put(routedApp(2, "1"))
	running := e.rt.addEphemeral("web", 0, 1, "running", 80, 8080)
	created := e.rt.addEphemeral("web", 1, 1, "created", 80, 8080)
	e.reconcile()

	name0, name1 := ContainerName("web", 0, 1), ContainerName("web", 1, 1)
	run := e.running()
	if len(run) != 2 || run[name0].ID != running || run[name1].ID == created {
		t.Fatalf("running %v: the running container is to be kept, the created one replaced", keysOf(run))
	}
	ports := routePortsByName(e.status("web"))
	if rp := ports[name0]; len(rp) != 2 || rp[0].HostPort < 32768 {
		t.Fatalf("the kept container reported %+v, not its ephemeral ports", rp)
	}
	if rb := routeBindings(run[name1]); len(rb) != 2 || !inRouteRange(rb[0].HostPort) || ports[name1][0].HostPort != rb[0].HostPort {
		t.Fatalf("the created one's replacement publishes %+v, reports %+v", rb, ports[name1])
	}

	e.rt.exit(running, 1)
	e.clock.Advance(2 * time.Second)
	e.reconcile()
	c, ok := e.running()[name0]
	if !ok || c.ID == running {
		t.Fatalf("the exited container was not replaced: %v", keysOf(e.running()))
	}
	rb := routeBindings(c)
	if len(rb) != 2 || !inRouteRange(rb[0].HostPort) || !inRouteRange(rb[1].HostPort) {
		t.Fatalf("its replacement publishes %+v", rb)
	}
	for _, s := range e.status("web") {
		if s.Name != name0 {
			continue
		}
		if s.ContainerID != c.ID || len(s.RoutePorts) != 2 || s.RoutePorts[0].HostPort != rb[0].HostPort || s.RestartCount != 1 {
			t.Fatalf("status %+v; the container is %s with %+v", s, shortID(c.ID), rb)
		}
	}
	snap := e.rt.snapshot()
	for _, x := range snap {
		if x.ID == running || x.ID == created {
			t.Fatalf("a container of the earlier build is left: %s", x.Name)
		}
	}
	if len(snap) != 2 {
		t.Fatalf("containers: %v", names(snap))
	}
	if !strings.Contains(e.logs.String(), "removed a routed container with ephemeral host ports") {
		t.Error("the replacement was not logged")
	}
}

func TestRoutedReplicaNeedsARouteBind(t *testing.T) {
	e := newRoutedEnv(t, "")
	e.put(routedApp(1, "1"))
	if err := e.agent.Reconcile(ctxT(t)); err == nil || !strings.Contains(err.Error(), "no private address") {
		t.Fatalf("reconcile: %v", err)
	}
	if len(e.running()) != 0 {
		t.Fatal("started a routed container with nowhere to publish it")
	}
	sts := e.status("web")
	if len(sts) != 1 || sts[0].State != "pending" || !strings.Contains(sts[0].Error, "no private address") {
		t.Fatalf("status %+v", sts)
	}
}

// TestRoutedRolloutDrainsTheOldCopy follows a rollout of a routed app on one
// node: the new copy starts beside the old one, the rollout promotes it once
// it is Ready, and the old copy, no longer assigned, keeps running for the
// drain delay before it is stopped.
func TestRoutedRolloutDrainsTheOldCopy(t *testing.T) {
	e := newRoutedEnv(t, routeAddr)
	e.put(routedApp(1, "1"))
	e.reconcile()
	old := ContainerName("web", 0, 1)

	e.put(routedApp(1, "2"))
	if err := e.st.AdvanceRollouts(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	run := e.running()
	if _, ok := run[old]; !ok || len(run) != 2 {
		t.Fatalf("the surge did not start beside the old copy: %v", keysOf(run))
	}

	// The surge reported Ready in that pass, so the rollout promotes it.
	if err := e.st.AdvanceRollouts(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if _, ok := e.running()[old]; !ok {
		t.Fatal("the old copy stopped before its drain delay")
	}
	var reported *store.Status
	for _, s := range e.status("web") {
		if s.Name == old {
			reported = &s
		}
	}
	if reported == nil || reported.Ready || reported.State != "running" {
		t.Fatalf("the draining copy is reported as %+v", reported)
	}
	if w := e.agent.wake(); w.IsZero() || w.Sub(e.clock.Now()) > DefaultRouteDrain {
		t.Fatalf("no pass is due when the drain ends: wake %v, now %v", w, e.clock.Now())
	}

	e.clock.Advance(DefaultRouteDrain)
	e.reconcile()
	run = e.running()
	if _, ok := run[old]; ok || len(run) != 1 {
		t.Fatalf("after the drain delay: %v", keysOf(run))
	}
	if !strings.Contains(e.logs.String(), "left the routing table") {
		t.Error("the drain was not logged")
	}
}

// A container of an app without routes is removed at once, as before.
func TestUnroutedContainerIsRemovedAtOnce(t *testing.T) {
	e := newRoutedEnv(t, routeAddr)
	e.put(spec.App{Name: "web", Image: "nginx:alpine", Replicas: 2})
	e.reconcile()
	e.put(spec.App{Name: "web", Image: "nginx:alpine", Replicas: 1})
	e.reconcile()
	if run := e.running(); len(run) != 1 {
		t.Fatalf("running after scale down: %v", keysOf(run))
	}
}

func routePortsByName(sts []store.Status) map[string][]store.RoutePort {
	out := map[string][]store.RoutePort{}
	for _, s := range sts {
		out[s.Name] = s.RoutePorts
	}
	return out
}

func equalPorts(a, b map[string][]store.RoutePort) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

func keysOf(m map[string]fakeContainer) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// An image's own hivemind.routed=true label does not give the container of
// an app without routes the drain: hivemind sets the label on every
// container, and the container's label wins over the image's.
func TestImageLabelDoesNotMakeAContainerRouted(t *testing.T) {
	e := newRoutedEnv(t, routeAddr)
	e.rt.imageLabels = map[string]map[string]string{"docker.io/library/nginx:alpine": {runtime.LabelRouted: "true"}}
	e.put(spec.App{Name: "web", Image: "nginx:alpine", Replicas: 2})
	e.reconcile()
	for name, c := range e.running() {
		if c.Labels[runtime.LabelRouted] != "false" {
			t.Errorf("%s: label %s = %q, want false", name, runtime.LabelRouted, c.Labels[runtime.LabelRouted])
		}
	}
	e.put(spec.App{Name: "web", Image: "nginx:alpine", Replicas: 1})
	e.reconcile()
	if run := e.running(); len(run) != 1 {
		t.Fatalf("running after scale down: %v; the image's label gave the extra container the drain", keysOf(run))
	}
}
