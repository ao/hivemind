package scheduler

import (
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"
)

// checkInvariants runs Place twice on the same input and fails the test if the
// results differ or break a rule that must hold for every input.
func checkInvariants(t *testing.T, nodes []Node, existing []Assignment, pending []Replica, now time.Time, opts Options) ([]Assignment, []Unplaced) {
	t.Helper()
	placed, unplaced := Place(nodes, existing, pending, now, opts)
	placed2, unplaced2 := Place(nodes, existing, pending, now, opts)
	// Compare printed forms: reflect.DeepEqual treats NaN as unequal to itself.
	if fmt.Sprintf("%#v %#v", placed, unplaced) != fmt.Sprintf("%#v %#v", placed2, unplaced2) {
		t.Fatalf("two runs on the same input differ:\n%v %v\n%v %v", placed, unplaced, placed2, unplaced2)
	}

	if len(placed)+len(unplaced) != len(pending) {
		t.Fatalf("%d placed + %d unplaced != %d pending", len(placed), len(unplaced), len(pending))
	}
	want := map[string]int{}
	for _, r := range pending {
		want[identity(r)]++
	}
	for _, a := range placed {
		want[identity(a.Replica)]--
	}
	for _, u := range unplaced {
		want[identity(u.Replica)]--
		if u.Reason == "" {
			t.Fatalf("unplaced %s has no reason", identity(u.Replica))
		}
	}
	for k, n := range want {
		if n != 0 {
			t.Fatalf("replica %s: output count off by %d", k, n)
		}
	}

	minReady := opts.MinReady
	if minReady == 0 {
		minReady = DefaultMinReady
	}
	first := map[string]Node{}
	for _, n := range nodes {
		if _, ok := first[n.ID]; !ok {
			first[n.ID] = n
		}
	}

	type usage struct {
		cpu   float64
		mem   big.Int
		ports []hostPort
	}
	used := map[string]*usage{}
	get := func(id string) *usage {
		if used[id] == nil {
			used[id] = &usage{}
		}
		return used[id]
	}
	for _, a := range existing {
		if _, ok := first[a.NodeID]; !ok {
			continue
		}
		u := get(a.NodeID)
		u.cpu += a.Replica.Resources.CPU
		u.mem.Add(&u.mem, big.NewInt(int64(a.Replica.Resources.Memory)))
		u.ports = append(u.ports, parsePorts(a.Replica.HostPorts)...)
	}

	touched := map[string]bool{}
	for _, a := range placed {
		n, ok := first[a.NodeID]
		if !ok {
			t.Fatalf("%s placed on unknown node %q", identity(a.Replica), a.NodeID)
		}
		if !n.Ready || now.Sub(n.ReadySince) < minReady {
			t.Fatalf("%s placed on node %s, which is not ready or not ready for %s", identity(a.Replica), n.ID, minReady)
		}
		if a.Replica.PinnedNode != "" && a.Replica.PinnedNode != a.NodeID {
			t.Fatalf("%s is pinned to %s but placed on %s", identity(a.Replica), a.Replica.PinnedNode, a.NodeID)
		}
		u := get(a.NodeID)
		for _, p := range parsePorts(a.Replica.HostPorts) {
			for _, q := range u.ports {
				if p.conflicts(q) {
					t.Fatalf("%s on %s: host port %s conflicts with %s already there", identity(a.Replica), a.NodeID, p.raw, q.raw)
				}
			}
		}
		u.cpu += a.Replica.Resources.CPU
		u.mem.Add(&u.mem, big.NewInt(int64(a.Replica.Resources.Memory)))
		u.ports = append(u.ports, parsePorts(a.Replica.HostPorts)...)
		touched[a.NodeID] = true
	}
	for id := range touched {
		n, u := first[id], used[id]
		cpuCap := n.CPU
		if math.IsNaN(cpuCap) || cpuCap < 0 {
			cpuCap = 0
		}
		if u.cpu > cpuCap+1e-6 {
			t.Fatalf("node %s: %v CPUs in use, has %v", id, u.cpu, n.CPU)
		}
		if u.mem.Cmp(big.NewInt(max(n.Memory, 0))) > 0 {
			t.Fatalf("node %s: %s bytes in use, has %d", id, u.mem.String(), n.Memory)
		}
	}
	return placed, unplaced
}

func identity(r Replica) string {
	return fmt.Sprintf("%s/%d@%d", r.App, r.Index, r.Generation)
}
