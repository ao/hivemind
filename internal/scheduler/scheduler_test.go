package scheduler

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

var now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

const gi = 1 << 30

// node returns a node that has been ready for an hour.
func node(id string, cpu float64, mem int64) Node {
	return Node{ID: id, CPU: cpu, Memory: mem, Ready: true, ReadySince: now.Add(-time.Hour)}
}

func rep(app string, i int, cpu float64, mem int64, ports ...string) Replica {
	return Replica{App: app, Index: i, Generation: 1, Resources: spec.Resources{CPU: cpu, Memory: spec.ByteSize(mem)}, HostPorts: ports}
}

func on(r Replica, id string) Assignment { return Assignment{Replica: r, NodeID: id} }

// where maps each placed replica to its node, and each unplaced one to "-" + reason.
func where(placed []Assignment, unplaced []Unplaced) map[string]string {
	out := map[string]string{}
	for _, a := range placed {
		out[identity(a.Replica)] = a.NodeID
	}
	for _, u := range unplaced {
		out[identity(u.Replica)] = "-" + u.Reason
	}
	return out
}

func TestPlace(t *testing.T) {
	three := []Node{node("a", 4, 4*gi), node("b", 4, 4*gi), node("c", 4, 4*gi)}
	pinned := func(r Replica, id string) Replica { r.PinnedNode = id; r.Volumes = []string{"data"}; return r }

	tests := []struct {
		name     string
		nodes    []Node
		existing []Assignment
		pending  []Replica
		opts     Options
		want     map[string]string
	}{
		{
			name:    "spread one app over three nodes",
			nodes:   three,
			pending: []Replica{rep("web", 0, 1, gi), rep("web", 1, 1, gi), rep("web", 2, 1, gi)},
			want:    map[string]string{"web/0@1": "a", "web/1@1": "b", "web/2@1": "c"},
		},
		{
			name:     "spread counts existing replicas of the app",
			nodes:    three,
			existing: []Assignment{on(rep("web", 0, 0.1, 0), "a"), on(rep("web", 1, 0.1, 0), "b")},
			pending:  []Replica{rep("web", 2, 0.1, 0)},
			want:     map[string]string{"web/2@1": "c"},
		},
		{
			name:     "fewest replicas of the app beats least loaded",
			nodes:    three,
			existing: []Assignment{on(rep("web", 0, 0.1, 0), "a"), on(rep("web", 1, 0.1, 0), "b"), on(rep("db", 0, 3, 3*gi), "c")},
			pending:  []Replica{rep("web", 2, 0.5, 0)},
			want:     map[string]string{"web/2@1": "c"},
		},
		{
			name:     "least loaded when app counts tie",
			nodes:    three,
			existing: []Assignment{on(rep("db", 0, 2, 0), "a"), on(rep("db", 1, 1, 0), "c"), on(rep("cache", 0, 0, 3*gi), "b")},
			pending:  []Replica{rep("web", 0, 0.5, 0)},
			want:     map[string]string{"web/0@1": "c"},
		},
		{
			name:    "tie broken by node ID whatever the input order",
			nodes:   []Node{node("c", 4, gi), node("a", 4, gi), node("b", 4, gi)},
			pending: []Replica{rep("web", 0, 1, 0)},
			want:    map[string]string{"web/0@1": "a"},
		},
		{
			name:     "never exceed CPU",
			nodes:    []Node{node("a", 2, 8*gi), node("b", 2, 8*gi)},
			existing: []Assignment{on(rep("x", 0, 1.5, 0), "a"), on(rep("x", 1, 1, 0), "b")},
			pending:  []Replica{rep("web", 0, 1, 0), rep("web", 1, 1, 0)},
			want:     map[string]string{"web/0@1": "b", "web/1@1": "-no node has 1 CPU free"},
		},
		{
			name:    "no node has 2 CPUs free",
			nodes:   []Node{node("a", 1, gi), node("b", 1.5, gi)},
			pending: []Replica{rep("web", 0, 2, 0)},
			want:    map[string]string{"web/0@1": "-no node has 2 CPUs free"},
		},
		{
			name:    "fractional CPU fills a node exactly",
			nodes:   []Node{node("a", 0.3, gi)},
			pending: []Replica{rep("w", 0, 0.1, 0), rep("w", 1, 0.1, 0), rep("w", 2, 0.1, 0), rep("w", 3, 0.1, 0)},
			want:    map[string]string{"w/0@1": "a", "w/1@1": "a", "w/2@1": "a", "w/3@1": "-no node has 0.1 CPUs free"},
		},
		{
			name:    "never exceed memory",
			nodes:   []Node{node("a", 8, gi)},
			pending: []Replica{rep("web", 0, 0, 512<<20), rep("web", 1, 0, 512<<20), rep("web", 2, 0, 512<<20)},
			want:    map[string]string{"web/0@1": "a", "web/1@1": "a", "web/2@1": "-no node has 512Mi of memory free"},
		},
		{
			name:     "existing replicas over capacity leave no room",
			nodes:    []Node{node("a", 1, gi)},
			existing: []Assignment{on(rep("x", 0, 2, 0), "a")},
			pending:  []Replica{rep("web", 0, 0, 0)},
			want:     map[string]string{"web/0@1": "-no node has 0 CPUs free"},
		},
		{
			name:    "replicas with no request fit anywhere",
			nodes:   []Node{node("a", 0, 0)},
			pending: []Replica{rep("web", 0, 0, 0), rep("web", 1, 0, 0)},
			want:    map[string]string{"web/0@1": "a", "web/1@1": "a"},
		},
		{
			name:    "host port conflict moves the replica",
			nodes:   []Node{node("a", 4, gi), node("b", 4, gi)},
			pending: []Replica{rep("web", 0, 0, 0, "0.0.0.0:80/tcp"), rep("api", 0, 0, 0, "127.0.0.1:80/tcp")},
			want:    map[string]string{"api/0@1": "a", "web/0@1": "b"},
		},
		{
			name:    "host port in use on every eligible node",
			nodes:   []Node{node("a", 4, gi), node("b", 4, gi)},
			pending: []Replica{rep("web", 0, 0, 0, "0.0.0.0:80/tcp"), rep("web", 1, 0, 0, "0.0.0.0:80/tcp"), rep("web", 2, 0, 0, "0.0.0.0:80/tcp")},
			want:    map[string]string{"web/0@1": "a", "web/1@1": "b", "web/2@1": "-host port 0.0.0.0:80/tcp is in use on every eligible node"},
		},
		{
			name:    "different specific addresses share a node",
			nodes:   []Node{node("a", 4, gi)},
			pending: []Replica{rep("web", 0, 0, 0, "10.0.0.1:80/tcp"), rep("web", 1, 0, 0, "10.0.0.2:80/tcp")},
			want:    map[string]string{"web/0@1": "a", "web/1@1": "a"},
		},
		{
			name:    "same port on another protocol shares a node",
			nodes:   []Node{node("a", 4, gi)},
			pending: []Replica{rep("dns", 0, 0, 0, "0.0.0.0:53/tcp"), rep("dns", 1, 0, 0, "0.0.0.0:53/udp")},
			want:    map[string]string{"dns/0@1": "a", "dns/1@1": "a"},
		},
		{
			name:     "existing replica's port blocks a node",
			nodes:    []Node{node("a", 4, gi), node("b", 4, gi)},
			existing: []Assignment{on(rep("old", 0, 0, 0, "127.0.0.1:8080/tcp"), "a")},
			pending:  []Replica{rep("web", 0, 0, 0, "0.0.0.0:8080/tcp"), rep("web", 1, 0, 0, "0.0.0.0:8080/tcp")},
			want:     map[string]string{"web/0@1": "b", "web/1@1": "-host port 0.0.0.0:8080/tcp is in use on every eligible node"},
		},
		{
			name:     "mixed failures are listed",
			nodes:    []Node{node("a", 1, gi), node("b", 4, gi)},
			existing: []Assignment{on(rep("x", 0, 0, 0, "0.0.0.0:80/tcp"), "b")},
			pending:  []Replica{rep("y", 0, 2, 0, "0.0.0.0:80/tcp")},
			want:     map[string]string{"y/0@1": "-no eligible node fits: 1 node without 2 CPUs free; 1 node with host port 0.0.0.0:80/tcp in use"},
		},
		{
			name:    "pinned replica goes only to its node",
			nodes:   three,
			pending: []Replica{pinned(rep("db", 0, 1, gi), "c")},
			want:    map[string]string{"db/0@1": "c"},
		},
		{
			name:    "pinned replica is placed before others take its room",
			nodes:   []Node{node("a", 1, gi)},
			pending: []Replica{rep("a-web", 0, 1, 0), pinned(rep("z-db", 0, 1, 0), "a")},
			want:    map[string]string{"z-db/0@1": "a", "a-web/0@1": "-no node has 1 CPU free"},
		},
		{
			name: "pinned replica on a node that is not ready stays",
			nodes: []Node{node("a", 4, gi), {
				ID: "b", CPU: 4, Memory: gi, Ready: false, ReadySince: now.Add(-time.Hour),
			}},
			pending: []Replica{pinned(rep("db", 0, 1, 0), "b")},
			want:    map[string]string{"db/0@1": "-volume pinned to node b, which is unavailable"},
		},
		{
			name:    "pinned replica on a missing node stays",
			nodes:   three,
			pending: []Replica{pinned(rep("db", 0, 1, 0), "gone")},
			want:    map[string]string{"db/0@1": "-volume pinned to node gone, which is unavailable"},
		},
		{
			name:    "pinned replica on a node that just came back stays",
			nodes:   []Node{node("a", 4, gi), {ID: "b", CPU: 4, Memory: gi, Ready: true, ReadySince: now.Add(-10 * time.Second)}},
			pending: []Replica{pinned(rep("db", 0, 1, 0), "b")},
			want:    map[string]string{"db/0@1": "-volume pinned to node b, which is unavailable"},
		},
		{
			name:    "pinned replica that does not fit says so",
			nodes:   []Node{node("a", 1, gi)},
			pending: []Replica{pinned(rep("db", 0, 2, 0), "a")},
			want:    map[string]string{"db/0@1": "-volume pinned to node a, which does not have 2 CPUs free"},
		},
		{
			name:     "pinned replica with a port conflict says so",
			nodes:    []Node{node("a", 1, gi)},
			existing: []Assignment{on(rep("x", 0, 0, 0, "5432/tcp"), "a")},
			pending:  []Replica{pinned(rep("db", 0, 0, 0, "127.0.0.1:5432/tcp"), "a")},
			want:     map[string]string{"db/0@1": "-volume pinned to node a, where host port 127.0.0.1:5432/tcp is in use"},
		},
		{
			name:    "node that is not ready gets nothing",
			nodes:   []Node{node("a", 1, gi), {ID: "b", CPU: 8, Memory: 8 * gi, ReadySince: now.Add(-time.Hour)}},
			pending: []Replica{rep("web", 0, 1, 0), rep("web", 1, 1, 0)},
			want:    map[string]string{"web/0@1": "a", "web/1@1": "-no node has 1 CPU free"},
		},
		{
			name:    "flapping node waits 30s by default",
			nodes:   []Node{{ID: "a", CPU: 4, Memory: gi, Ready: true, ReadySince: now.Add(-29 * time.Second)}, {ID: "b", CPU: 4, Memory: gi, Ready: true, ReadySince: now.Add(-30 * time.Second)}},
			pending: []Replica{rep("web", 0, 0, 0), rep("web", 1, 0, 0)},
			want:    map[string]string{"web/0@1": "b", "web/1@1": "b"},
		},
		{
			name:    "no node ready long enough",
			nodes:   []Node{{ID: "a", CPU: 4, Memory: gi, Ready: true, ReadySince: now.Add(-time.Second)}},
			pending: []Replica{rep("web", 0, 0, 0)},
			want:    map[string]string{"web/0@1": "-no node has been ready for 30s"},
		},
		{
			name:    "custom MinReady",
			nodes:   []Node{{ID: "a", CPU: 4, Memory: gi, Ready: true, ReadySince: now.Add(-5 * time.Second)}},
			pending: []Replica{rep("web", 0, 0, 0)},
			opts:    Options{MinReady: 5 * time.Second},
			want:    map[string]string{"web/0@1": "a"},
		},
		{
			name:    "negative MinReady means no wait",
			nodes:   []Node{{ID: "a", CPU: 4, Memory: gi, Ready: true, ReadySince: now}},
			pending: []Replica{rep("web", 0, 0, 0)},
			opts:    Options{MinReady: -1},
			want:    map[string]string{"web/0@1": "a"},
		},
		{
			name:    "ReadySince in the future is not ready yet",
			nodes:   []Node{{ID: "a", CPU: 4, Memory: gi, Ready: true, ReadySince: now.Add(time.Hour)}},
			pending: []Replica{rep("web", 0, 0, 0)},
			opts:    Options{MinReady: -1},
			want:    map[string]string{"web/0@1": "-no node is ready"},
		},
		{
			name:    "no nodes",
			pending: []Replica{rep("web", 0, 0, 0)},
			want:    map[string]string{"web/0@1": "-no node is ready"},
		},
		{
			name:    "invalid resources",
			nodes:   three,
			pending: []Replica{rep("web", 0, math.NaN(), 0), rep("web", 1, 0, -1)},
			want:    map[string]string{"web/0@1": "-invalid resources: cpu NaN, memory 0", "web/1@1": "-invalid resources: cpu 0, memory -1"},
		},
		{
			name:    "repeated node ID uses the first",
			nodes:   []Node{node("a", 1, gi), node("a", 8, 8*gi)},
			pending: []Replica{rep("web", 0, 1, 0), rep("web", 1, 1, 0)},
			want:    map[string]string{"web/0@1": "a", "web/1@1": "-no node has 1 CPU free"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			placed, unplaced := checkInvariants(t, tt.nodes, tt.existing, tt.pending, now, tt.opts)
			if got := where(placed, unplaced); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got  %v\nwant %v", got, tt.want)
			}
		})
	}
}

// TestAcceptance is PLAN Task 9's acceptance, at the unit level.
func TestAcceptance(t *testing.T) {
	nodes := []Node{node("n1", 2, 2*gi), node("n2", 2, 2*gi), node("n3", 2, 2*gi)}
	app := spec.App{Name: "web", Ports: []spec.Port{{Bind: "0.0.0.0", Host: 8080, Container: 80}}}
	var pending []Replica
	for i := range 3 {
		pending = append(pending, Replica{App: "web", Index: i, Generation: 1, HostPorts: HostPortsOf(app), Resources: spec.Resources{CPU: 0.5, Memory: 128 << 20}})
	}
	placed, unplaced := checkInvariants(t, nodes, nil, pending, now, Options{})
	if len(unplaced) != 0 || len(placed) != 3 {
		t.Fatalf("placed %v, unplaced %v", placed, unplaced)
	}
	seen := map[string]bool{}
	for _, a := range placed {
		seen[a.NodeID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("3 replicas landed on %d nodes: %v", len(seen), placed)
	}

	fourth := Replica{App: "web", Index: 3, Generation: 1, HostPorts: HostPortsOf(app), Resources: spec.Resources{CPU: 0.5}}
	placed2, unplaced2 := checkInvariants(t, nodes, placed, []Replica{fourth}, now, Options{})
	if len(placed2) != 0 || len(unplaced2) != 1 {
		t.Fatalf("4th replica: placed %v, unplaced %v", placed2, unplaced2)
	}
	if want := "host port 0.0.0.0:8080/tcp is in use on every eligible node"; unplaced2[0].Reason != want {
		t.Fatalf("reason %q, want %q", unplaced2[0].Reason, want)
	}
}

func TestPlaceDoesNotModifyInput(t *testing.T) {
	nodes := []Node{node("b", 4, gi), node("a", 4, gi)}
	pending := []Replica{rep("z", 0, 1, 0, "80/tcp"), rep("a", 0, 1, 0)}
	nodesCopy := append([]Node(nil), nodes...)
	pendingCopy := append([]Replica(nil), pending...)
	Place(nodes, nil, pending, now, Options{})
	if !reflect.DeepEqual(nodes, nodesCopy) || !reflect.DeepEqual(pending, pendingCopy) {
		t.Fatal("Place reordered or changed its input")
	}
}

func TestPortConflicts(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1:80/tcp", "0.0.0.0:80/tcp", true},
		{"0.0.0.0:80/tcp", "127.0.0.1:80/tcp", true},
		{"10.0.0.1:80/tcp", "10.0.0.2:80/tcp", false},
		{"10.0.0.1:80/tcp", "10.0.0.1:80/tcp", true},
		{"10.0.0.1:80/tcp", "10.0.0.1:80/udp", false},
		{"10.0.0.1:80/tcp", "10.0.0.1:81/tcp", false},
		{"10.0.0.1:80/tcp", "10.0.0.1:80/TCP", true},
		{"80/tcp", "10.0.0.1:80/tcp", true},
		{"80/tcp", "[::1]:80/tcp", true},
		{"80/tcp", "81/tcp", false},
		{"[::]:80/tcp", "10.0.0.1:80/tcp", true},
		{"[::]:80/tcp", "[::1]:80/tcp", true},
		{"0.0.0.0:80/tcp", "[::1]:80/tcp", false},
		{"0.0.0.0:80/tcp", "[::]:80/tcp", true},
		{"[::1]:80/tcp", "[::2]:80/tcp", false},
		{"[::ffff:10.0.0.1]:80/tcp", "10.0.0.1:80/tcp", true},
		{"[::ffff:10.0.0.1]:80/tcp", "0.0.0.0:80/tcp", true},
		{"garbage", "garbage", true},
		{"garbage", "80/tcp", false},
		{"", "", true},
		{"99999/tcp", "80/tcp", false},
	}
	for _, tt := range tests {
		if got := parsePort(tt.a).conflicts(parsePort(tt.b)); got != tt.want {
			t.Errorf("%q vs %q: got %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestHostPortsOf(t *testing.T) {
	app := spec.App{Ports: []spec.Port{
		{Host: 8080, Container: 80},
		{Bind: "0.0.0.0", Host: 53, Container: 53, Protocol: "UDP"},
		{Bind: "::1", Host: 443, Container: 443, Protocol: "tcp"},
	}}
	want := []string{"0.0.0.0:53/udp", "127.0.0.1:8080/tcp", "[::1]:443/tcp"}
	if got := HostPortsOf(app); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Every entry must parse as a real address, not fall back to opaque.
	for _, s := range want {
		if parsePort(s).opaque {
			t.Fatalf("%q did not parse", s)
		}
	}
}

func TestLostNodes(t *testing.T) {
	leases := []NodeLease{
		{NodeID: "fresh", RenewedAt: now.Add(-5 * time.Second)},
		{NodeID: "edge", RenewedAt: now.Add(-15 * time.Second)},
		{NodeID: "lost", RenewedAt: now.Add(-16 * time.Second)},
		{NodeID: "also-lost", RenewedAt: now.Add(-time.Hour)},
		{NodeID: "renewed", RenewedAt: now.Add(-time.Hour)},
		{NodeID: "renewed", RenewedAt: now.Add(-time.Second)},
		{NodeID: "never", RenewedAt: time.Time{}},
	}
	if got, want := LostNodes(leases, now, 0), []string{"also-lost", "lost", "never"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("default timeout: got %v, want %v", got, want)
	}
	if got, want := LostNodes(leases, now, time.Minute), []string{"also-lost", "never"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("1m timeout: got %v, want %v", got, want)
	}
	if got := LostNodes(nil, now, 0); len(got) != 0 {
		t.Fatalf("no leases: got %v", got)
	}
}

// randomCluster builds a cluster from r. Node IDs and replica identities are
// unique, so the result must not depend on input order either.
func randomCluster(r *rand.Rand) (nodes []Node, existing []Assignment, pending []Replica, opts Options) {
	ports := []string{"0.0.0.0:80/tcp", "127.0.0.1:80/tcp", "10.0.0.1:80/tcp", "10.0.0.2:80/tcp", "[::]:80/tcp", "[::1]:80/tcp", "80/tcp", "0.0.0.0:443/tcp", "0.0.0.0:53/udp", "53/tcp"}
	pickPorts := func() []string {
		var out []string
		for range r.Intn(3) {
			out = append(out, ports[r.Intn(len(ports))])
		}
		return out
	}
	cpus := []float64{0, 0.1, 0.25, 0.3, 0.5, 1, 1.5, 2, 4}
	nn := r.Intn(7)
	for i := range nn {
		n := Node{
			ID:         fmt.Sprintf("n%02d", i),
			CPU:        float64(r.Intn(9)) * 0.5,
			Memory:     int64(r.Intn(9)) * (256 << 20),
			Ready:      r.Intn(5) != 0,
			ReadySince: now.Add(-time.Duration(r.Intn(90)) * time.Second),
		}
		nodes = append(nodes, n)
	}
	nodeID := func() string {
		if nn == 0 || r.Intn(8) == 0 {
			return "missing"
		}
		return fmt.Sprintf("n%02d", r.Intn(nn))
	}
	apps := []string{"web", "api", "db", "cache"}
	idx := 0
	mk := func() Replica {
		idx++
		rp := Replica{
			App:        apps[r.Intn(len(apps))],
			Index:      idx,
			Generation: int64(r.Intn(3)),
			Resources:  spec.Resources{CPU: cpus[r.Intn(len(cpus))], Memory: spec.ByteSize(r.Intn(5)) * (128 << 20)},
			HostPorts:  pickPorts(),
		}
		if r.Intn(6) == 0 {
			rp.PinnedNode = nodeID()
			rp.Volumes = []string{"data"}
		}
		return rp
	}
	for range r.Intn(10) {
		existing = append(existing, on(mk(), nodeID()))
	}
	for range r.Intn(15) {
		pending = append(pending, mk())
	}
	switch r.Intn(3) {
	case 1:
		opts.MinReady = time.Duration(r.Intn(60)) * time.Second
	case 2:
		opts.MinReady = -1
	}
	return nodes, existing, pending, opts
}

func TestPlaceProperties(t *testing.T) {
	const runs = 5000
	r := rand.New(rand.NewSource(9))
	for i := range runs {
		nodes, existing, pending, opts := randomCluster(r)
		ok := t.Run(fmt.Sprint(i), func(t *testing.T) {
			placed, unplaced := checkInvariants(t, nodes, existing, pending, now, opts)

			// Shuffled input gives the same answer.
			sn := append([]Node(nil), nodes...)
			se := append([]Assignment(nil), existing...)
			sp := append([]Replica(nil), pending...)
			r.Shuffle(len(sn), func(i, j int) { sn[i], sn[j] = sn[j], sn[i] })
			r.Shuffle(len(se), func(i, j int) { se[i], se[j] = se[j], se[i] })
			r.Shuffle(len(sp), func(i, j int) { sp[i], sp[j] = sp[j], sp[i] })
			p2, u2 := Place(sn, se, sp, now, opts)
			if !reflect.DeepEqual(placed, p2) || !reflect.DeepEqual(unplaced, u2) {
				t.Fatalf("shuffled input changed the result")
			}

			// A pinned replica whose node is unavailable gets the fixed reason.
			for _, u := range unplaced {
				if u.Replica.PinnedNode != "" && !strings.HasPrefix(u.Reason, "volume pinned to node "+u.Replica.PinnedNode) {
					t.Fatalf("pinned replica unplaced with reason %q", u.Reason)
				}
			}
		})
		if !ok {
			t.Logf("cluster: nodes %+v\nexisting %+v\npending %+v\nopts %+v", nodes, existing, pending, opts)
			return
		}
	}
}

// TestSpreadIsEven checks that with room everywhere, replicas of one app
// differ by at most one per node.
func TestSpreadIsEven(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for range 500 {
		nn := 1 + r.Intn(6)
		var nodes []Node
		for i := range nn {
			nodes = append(nodes, node(fmt.Sprintf("n%d", i), 100, 100*gi))
		}
		var pending []Replica
		for i := range r.Intn(40) {
			pending = append(pending, rep("web", i, 0.5, 64<<20))
		}
		placed, _ := checkInvariants(t, nodes, nil, pending, now, Options{})
		count := map[string]int{}
		for _, n := range nodes {
			count[n.ID] = 0
		}
		for _, a := range placed {
			count[a.NodeID]++
		}
		var c []int
		for _, v := range count {
			c = append(c, v)
		}
		sort.Ints(c)
		if c[len(c)-1]-c[0] > 1 {
			t.Fatalf("uneven spread over %d nodes: %v", nn, count)
		}
	}
}
