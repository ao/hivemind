package store

import (
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// routedEnv is a store with a fake clock, two nodes with addresses, and one
// routed app with a replica on each, reported Ready.
type routedEnv struct {
	*rolloutEnv
}

const (
	nodeRA = "node-ra"
	nodeRB = "node-rb"
)

func newRoutedEnv(t *testing.T) *routedEnv {
	t.Helper()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"), WithClock(clock.Now), WithMinReady(0))
	e := &routedEnv{&rolloutEnv{t: t, s: s, clock: clock, ctx: ctxT(t)}}
	for i, id := range []string{nodeRA, nodeRB} {
		if err := s.UpsertNode(e.ctx, Node{ID: id, Name: id, CPU: 4, Memory: 8 << 30, Address: []string{"10.0.0.1", "10.0.0.2"}[i]}); err != nil {
			t.Fatal(err)
		}
	}
	e.put(spec.App{Name: "web", Image: "nginx", Replicas: 2, Routes: []spec.Route{{Host: "www.example.com", Port: 80}}})
	for _, id := range []string{nodeRA, nodeRB} {
		e.report(id, e.routed(id, true)...)
	}
	return e
}

// routed is what node reports for its replicas of web, each Ready or not.
func (e *routedEnv) routed(node string, ready bool) []Status {
	e.t.Helper()
	as, err := e.s.AssignmentsForNode(e.ctx, node)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []Status
	for _, a := range as {
		st := container(a.App, a.Index, a.Generation)
		st.Ready = ready
		st.RoutePorts = []RoutePort{{Port: 80, HostPort: 32768 + a.Index}}
		out = append(out, st)
	}
	return out
}

// ep is the endpoint of node's replica of web: one on each node.
func (e *routedEnv) ep(node string) string {
	e.t.Helper()
	sts := e.routed(node, true)
	if len(sts) != 1 {
		e.t.Fatalf("%s has %d replicas of web, want 1", node, len(sts))
	}
	addr := map[string]string{nodeRA: "10.0.0.1", nodeRB: "10.0.0.2"}[node]
	return net.JoinHostPort(addr, strconv.Itoa(sts[0].RoutePorts[0].HostPort))
}

// endpoints reads the table and returns the endpoints of www.example.com,
// and whether the read was answered from the kept table.
func (e *routedEnv) endpoints() ([]string, bool) {
	e.t.Helper()
	e.s.routeMu.Lock()
	kept := e.s.routeCache
	e.s.routeMu.Unlock()
	rt, err := e.s.RouteTable(e.ctx)
	if err != nil || len(rt) != 1 {
		e.t.Fatalf("table: %+v, %v", rt, err)
	}
	e.s.routeMu.Lock()
	hit := kept != nil && e.s.routeCache == kept
	e.s.routeMu.Unlock()
	return rt[0].Endpoints, hit
}

func (e *routedEnv) want(step string, eps []string, hit bool) {
	e.t.Helper()
	got, h := e.endpoints()
	if !slices.Equal(got, eps) {
		e.t.Fatalf("%s: endpoints %v, want %v", step, got, eps)
	}
	if h != hit {
		e.t.Fatalf("%s: answered from the kept table %v, want %v", step, h, hit)
	}
}

// The routing table is read once and kept while nothing it reads changes: a
// lease renewal of a node whose lease is current, and a report the same as
// the node's last, keep it. A changed ready flag, a lease that lapses with no
// write at all, the renewal that brings that node back, and a removal each
// reach the next read.
func TestRouteTableIsKeptUntilWhatItReadsChanges(t *testing.T) {
	e := newRoutedEnv(t)
	ra0, rb1 := e.ep(nodeRA), e.ep(nodeRB)
	both := []string{ra0, rb1}
	if got, _ := e.endpoints(); !slices.Equal(got, both) {
		t.Fatalf("first read: %v, want %v", got, both)
	}
	e.want("a second read", both, true)

	e.clock.Advance(time.Second)
	if err := e.s.RenewLease(e.ctx, nodeRA); err != nil {
		t.Fatal(err)
	}
	e.want("after a lease renewal", both, true)
	e.report(nodeRA, e.routed(nodeRA, true)...)
	e.want("after a report the same as the last", both, true)

	e.report(nodeRA, e.routed(nodeRA, false)...)
	e.want("after node-ra's replica is not ready", []string{rb1}, false)
	e.want("read again", []string{rb1}, true)
	e.report(nodeRA, e.routed(nodeRA, true)...)
	e.want("after it is ready again", both, false)

	// node-ra's lease lapses while node-rb renews: no write changes what
	// the table reads of node-ra, only the time does.
	for range 4 {
		e.clock.Advance(4 * time.Second)
		if err := e.s.RenewLease(e.ctx, nodeRB); err != nil {
			t.Fatal(err)
		}
	}
	e.want("after node-ra's lease lapsed", []string{rb1}, false)
	if err := e.s.RenewLease(e.ctx, nodeRA); err != nil {
		t.Fatal(err)
	}
	e.want("after node-ra renewed its lapsed lease", both, false)

	if _, err := e.s.RemoveNode(e.ctx, nodeRA, true); err != nil {
		t.Fatal(err)
	}
	if removed, err := e.s.IsNodeRemoved(e.ctx, nodeRA); err != nil || !removed {
		t.Fatalf("node-ra after its removal: removed %v, %v", removed, err)
	}
	e.want("after node-ra was removed", []string{rb1}, false)
}

// A caller that changes the table it got does not change the kept one.
func TestRouteTableKeptIsNotTheCallers(t *testing.T) {
	e := newRoutedEnv(t)
	rt, err := e.s.RouteTable(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	rt[0].Endpoints[0] = "changed"
	rt[0].Host = "changed"
	again, err := e.s.RouteTable(e.ctx)
	if err != nil || again[0].Host != "www.example.com" || again[0].Endpoints[0] == "changed" {
		t.Fatalf("the kept table changed with the caller's: %+v, %v", again, err)
	}
}

// The denylist of removed nodes, which the store keeps in memory, is read
// again when the store opens.
func TestRemovedNodesAreKnownAfterAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "hivemind.db")
	s, err := Open(ctxT(t), path, WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxT(t)
	for _, id := range []string{"node-1", "node-2"} {
		if err := s.UpsertNode(ctx, Node{ID: id, Name: id, CPU: 1, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RemoveNode(ctx, "node-2", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openT(t, path, WithMinReady(0))
	for id, want := range map[string]bool{"node-1": false, "node-2": true, "node-3": false} {
		if got, err := s.IsNodeRemoved(ctx, id); err != nil || got != want {
			t.Errorf("%s: removed %v, %v; want %v", id, got, err, want)
		}
	}
	if err := s.UpsertNode(ctx, Node{ID: "node-2", Name: "node-2", CPU: 1, Memory: 1 << 30}); err == nil {
		t.Fatal("a removed node registered again after a reopen")
	}
}
