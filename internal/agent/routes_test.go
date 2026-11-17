package agent

import (
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
	e.logs = &syncBuffer{}
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Clock: e.clock.Now, RouteBind: bind,
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	e.agent = a
	e.rt.images["docker.io/library/nginx:alpine"] = true
	return e
}

func routedApp(replicas int, env string) spec.App {
	return spec.App{Name: "web", Image: "nginx:alpine", Replicas: replicas, Env: map[string]string{"V": env},
		Routes: []spec.Route{{Host: "www.example.com", Port: 80}, {Host: "api.example.com", Port: 8080}, {Host: "w2.example.com", Port: 80}}}
}

func TestRoutedContainersPublishEphemeralPorts(t *testing.T) {
	e := newRoutedEnv(t, routeAddr)
	e.put(routedApp(2, "1"))
	e.reconcile()

	run := e.running()
	if len(run) != 2 {
		t.Fatalf("running: %v", keysOf(run))
	}
	for name, c := range run {
		if c.Labels[runtime.LabelRouted] != "true" {
			t.Errorf("%s has no routed label: %v", name, c.Labels)
		}
		var eph []runtime.PortBinding
		for _, p := range c.cfg.Ports {
			if p.Ephemeral {
				eph = append(eph, p)
			}
		}
		if len(eph) != 2 || eph[0].ContainerPort != 80 || eph[1].ContainerPort != 8080 || eph[0].HostIP != routeAddr {
			t.Errorf("%s ephemeral bindings: %+v", name, eph)
		}
	}
	first := routePortsByName(e.status("web"))
	for name, rp := range first {
		if len(rp) != 2 || rp[0].Port != 80 || rp[1].Port != 8080 || rp[0].HostPort < 32768 {
			t.Fatalf("%s reported route ports %+v", name, rp)
		}
	}
	// The next pass reads the same ports from the list.
	e.reconcile()
	if again := routePortsByName(e.status("web")); !equalPorts(first, again) {
		t.Fatalf("ports changed without a restart: %v then %v", first, again)
	}

	// A container that exits and is restarted gets new ports, and reports them.
	var id, name string
	for n, c := range e.running() {
		id, name = c.ID, n
		break
	}
	e.rt.exit(id, 1)
	e.clock.Advance(2 * time.Second)
	e.reconcile()
	after := routePortsByName(e.status("web"))
	if slices.Equal(after[name], first[name]) || len(after[name]) != 2 {
		t.Fatalf("restarted container reported %v, before %v", after[name], first[name])
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
