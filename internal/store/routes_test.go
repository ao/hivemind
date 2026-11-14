package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

func routed(name string, replicas int, hosts ...string) spec.App {
	a := spec.App{Name: name, Image: "nginx:alpine", Replicas: replicas, Env: map[string]string{"A": "1"}}
	for _, h := range hosts {
		a.Routes = append(a.Routes, spec.Route{Host: h, Port: 80})
	}
	return a
}

// renewAt registers a node with an address, or renews it.
func (e *rolloutEnv) renewAt(id, addr string) {
	e.t.Helper()
	if err := e.s.UpsertNode(e.ctx, Node{ID: id, Name: id, CPU: 4, Memory: 8 << 30, Address: addr}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *rolloutEnv) table() map[string][]string {
	e.t.Helper()
	rt, err := e.s.RouteTable(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string][]string{}
	for _, r := range rt {
		out[r.Host] = r.Endpoints
	}
	return out
}

// routedContainer is a Ready container that published port 80 on hostPort.
func routedContainer(app string, idx, gen, hostPort int) Status {
	c := container(app, idx, gen)
	c.RoutePorts = []RoutePort{{Port: 80, HostPort: hostPort}}
	return c
}

func TestRouteHostBelongsToOneApp(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	mustPut(t, s, routed("web", 1, "www.example.com", "web.example.com"))

	_, err := s.PutApp(ctx, routed("api", 1, "api.example.com", "www.example.com"))
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "www.example.com") || !strings.Contains(err.Error(), "app web") {
		t.Fatalf("clash: %v", err)
	}
	if _, err := s.GetApp(ctx, "api"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused put left the app behind: %v", err)
	}
	// The hosts are compared after normalisation.
	if _, err := s.PutApp(ctx, routed("api", 1, "WWW.Example.com.")); !errors.Is(err, ErrConflict) {
		t.Fatalf("clash in another case: %v", err)
	}
	// Putting the owner again with the same host is fine.
	mustPut(t, s, routed("web", 2, "www.example.com", "web.example.com"))
	// The owner gives the host up; another app can take it.
	mustPut(t, s, routed("web", 2, "web.example.com"))
	mustPut(t, s, routed("api", 1, "www.example.com"))
	// A deleted app frees its hosts at once, before its replicas are gone.
	if err := s.DeleteApp(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, routed("other", 1, "web.example.com"))
	rt, err := s.RouteTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, r := range rt {
		hosts = append(hosts, r.Host+"="+r.App)
	}
	if want := []string{"web.example.com=other", "www.example.com=api"}; !slices.Equal(hosts, want) {
		t.Fatalf("hosts %v, want %v", hosts, want)
	}
}

func TestRoutedReplicasNeedANodeAddress(t *testing.T) {
	e := newRolloutEnv(t, node1)
	e.put(routed("web", 1, "www.example.com"))
	reps := replicasOf(t, e.s, "web")
	if reps[0].NodeID != "" || !strings.Contains(reps[0].PendingReason, "private address") {
		t.Fatalf("placed on a node without an address: %+v", reps[0])
	}
	// An app without routes is placed as before.
	e.put(routed("plain", 1))
	if got := e.nodesOf("plain"); got[0] != node1 {
		t.Fatalf("an app without routes was not placed: %v", got)
	}
	// Registering with an address places the routed replica.
	e.renewAt(node1, "10.0.0.1")
	if got := e.nodesOf("web"); got[0] != node1 {
		t.Fatalf("not placed once the node has an address: %v", got)
	}
}

func TestNodeAddressIsChecked(t *testing.T) {
	s := newT(t)
	for _, addr := range []string{"0.0.0.0", "::", "example.com", "10.0.0.1:80", "fe80::1%eth0", "010.0.0.1"} {
		if err := s.UpsertNode(ctxT(t), Node{ID: "node-x", Name: "x", Address: addr}); !errors.Is(err, ErrInvalid) {
			t.Errorf("address %q: %v", addr, err)
		}
	}
	for _, addr := range []string{"", "10.0.0.1", "127.0.0.1", "fd00::5"} {
		if err := s.UpsertNode(ctxT(t), Node{ID: "node-x", Name: "x", Address: addr}); err != nil {
			t.Errorf("address %q: %v", addr, err)
		}
		n, err := s.GetNode(ctxT(t), "node-x")
		if err != nil || n.Address != addr {
			t.Errorf("address %q stored as %q (%v)", addr, n.Address, err)
		}
	}
}

func TestRoutePortsInReports(t *testing.T) {
	e := newRolloutEnv(t)
	e.renewAt(node1, "10.0.0.1")
	e.put(routed("web", 1, "www.example.com"))
	st := routedContainer("web", 0, 1, 32768)
	st.RoutePorts = append(st.RoutePorts, RoutePort{Port: 8080, HostPort: 32769})
	e.report(node1, st)
	got, err := e.s.StatusForNode(e.ctx, node1)
	if err != nil || len(got) != 1 || !slices.Equal(got[0].RoutePorts, st.RoutePorts) {
		t.Fatalf("route ports read back as %+v (%v)", got, err)
	}
	for _, bad := range [][]RoutePort{{{Port: 0, HostPort: 1}}, {{Port: 80, HostPort: 70000}}, make([]RoutePort, maxRoutePorts+1)} {
		st.RoutePorts = bad
		if err := e.s.ReportStatus(e.ctx, node1, []Status{st}); !errors.Is(err, ErrInvalid) {
			t.Errorf("route ports %v: %v", len(bad), err)
		}
	}
}

func TestRouteTableFromReports(t *testing.T) {
	e := newRolloutEnv(t)
	e.renewAt(node1, "10.0.0.1")
	e.renewAt(nodeB, "10.0.0.2")
	e.put(routed("web", 2, "www.example.com", "web.example.com"))
	e.put(routed("api", 1, "api.example.com"))
	if got := e.nodesOf("web"); !slices.Equal(got, []string{node1, node1}) {
		t.Fatalf("placement %v", got)
	}

	// Nothing has reported: every host is in the table, with no endpoints.
	tab := e.table()
	if len(tab) != 3 || len(tab["www.example.com"]) != 0 {
		t.Fatalf("table before reports: %v", tab)
	}

	// Replica 0 is Ready, replica 1 is running but not Ready yet, and a
	// container of a replica index the app does not have is ignored.
	notReady := routedContainer("web", 1, 1, 32769)
	notReady.Ready = false
	stray := routedContainer("web", 7, 1, 32770)
	e.report(node1, routedContainer("web", 0, 1, 32768), notReady, stray, routedContainer("api", 0, 1, 40000))
	tab = e.table()
	if want := []string{"10.0.0.1:32768"}; !slices.Equal(tab["www.example.com"], want) || !slices.Equal(tab["web.example.com"], want) {
		t.Fatalf("one ready replica: %v", tab)
	}
	if want := []string{"10.0.0.1:40000"}; !slices.Equal(tab["api.example.com"], want) {
		t.Fatalf("api: %v", tab)
	}

	e.report(node1, routedContainer("web", 0, 1, 32768), routedContainer("web", 1, 1, 32769), routedContainer("api", 0, 1, 40000))
	if want := []string{"10.0.0.1:32768", "10.0.0.1:32769"}; !slices.Equal(e.table()["www.example.com"], want) {
		t.Fatalf("two ready replicas: %v", e.table())
	}

	// A route to a port the containers do not publish has no endpoints.
	a := routed("api", 1, "api.example.com")
	a.Routes[0].Port = 9000
	e.put(a)
	if eps := e.table()["api.example.com"]; len(eps) != 0 {
		t.Fatalf("route to an unpublished port: %v", eps)
	}

	// A node whose lease lapses leaves the table before the sweep runs.
	e.clock.Advance(16 * time.Second)
	if eps := e.table()["www.example.com"]; len(eps) != 0 {
		t.Fatalf("endpoints of a lost node: %v", eps)
	}
	e.renewAt(node1, "10.0.0.1")
	if eps := e.table()["www.example.com"]; len(eps) != 2 {
		t.Fatalf("endpoints after the lease renewed: %v", eps)
	}

	// A deleted app's hosts leave the table at once.
	if err := e.s.DeleteApp(e.ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.table()["www.example.com"]; ok {
		t.Fatalf("deleted app still routed: %v", e.table())
	}
}

// TestRoutedRolloutOnOneNode checks the order a rollout of a routed app on a
// single node keeps: the new copy runs beside the old one, is in the table
// once Ready, and only then does the old copy leave the table and lose its
// assignment.
func TestRoutedRolloutOnOneNode(t *testing.T) {
	e := newRolloutEnv(t)
	e.renewAt(node1, "10.0.0.1")
	e.put(routed("web", 1, "www.example.com"))
	e.report(node1, routedContainer("web", 0, 1, 32768))

	next := routed("web", 1, "www.example.com")
	next.Env["A"] = "2"
	e.put(next)
	e.advance()
	as, err := e.s.AssignmentsForNode(e.ctx, node1)
	if err != nil {
		t.Fatal(err)
	}
	var gens []int
	for _, a := range as {
		if a.App == "web" && a.Index == 0 && a.Desired == Running {
			gens = append(gens, a.Generation)
		}
	}
	slices.Sort(gens)
	if !slices.Equal(gens, []int{1, 2}) {
		t.Fatalf("replica 0 on node1 at generations %v, want the old and the surge [1 2]", gens)
	}
	if eps := e.table()["www.example.com"]; !slices.Equal(eps, []string{"10.0.0.1:32768"}) {
		t.Fatalf("table while the surge starts: %v", eps)
	}

	// The surge is Ready: both copies are in the table until the rollout
	// promotes it.
	e.report(node1, routedContainer("web", 0, 1, 32768), routedContainer("web", 0, 2, 32769))
	if eps := e.table()["www.example.com"]; !slices.Equal(eps, []string{"10.0.0.1:32768", "10.0.0.1:32769"}) {
		t.Fatalf("table with both copies Ready: %v", eps)
	}
	e.advance()
	if g, n := e.gens("web"), e.nodesOf("web"); !slices.Equal(g, []int{2}) || !slices.Equal(n, []string{node1}) {
		t.Fatalf("after promotion: gens %v nodes %v", g, n)
	}
	// The old copy still runs and reports, but is no longer assigned, so the
	// table drops it before its node stops it.
	if eps := e.table()["www.example.com"]; !slices.Equal(eps, []string{"10.0.0.1:32769"}) {
		t.Fatalf("table after promotion: %v", eps)
	}
	if ro := e.rollout("web"); ro.State != RolloutDone {
		e.advance()
		if ro = e.rollout("web"); ro.State != RolloutDone {
			t.Fatalf("rollout %+v", ro)
		}
	}
}

// A drain never surges onto the node it drains, routed app or not.
func TestDrainOfARoutedAppLeavesTheNode(t *testing.T) {
	e := newRolloutEnv(t)
	e.renewAt(node1, "10.0.0.1")
	e.put(routed("web", 1, "www.example.com"))
	e.report(node1, routedContainer("web", 0, 1, 32768))
	if _, err := e.s.DrainNode(e.ctx, node1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AdvanceDrains(e.ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	surges, _ := querySurges(e.ctx, e.s.db, `1`)
	if len(surges) != 0 {
		t.Fatalf("drain surged with no other node: %+v", surges)
	}
	e.renewAt(nodeB, "10.0.0.2")
	if _, err := e.s.AdvanceDrains(e.ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	surges, _ = querySurges(e.ctx, e.s.db, `1`)
	if len(surges) != 1 || surges[0].NodeID != nodeB {
		t.Fatalf("drain surge: %+v", surges)
	}
}
