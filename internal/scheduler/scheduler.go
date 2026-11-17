// Package scheduler decides which node runs each replica.
//
// Place is a pure function: it does no I/O, starts no goroutines and reads no
// clock. The caller passes in the nodes, the assignments that already exist,
// the replicas that still need a node, and the current time. The same input
// always gives the same output.
package scheduler

import (
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// Defaults for Options and LostNodes.
const (
	// DefaultMinReady is how long a node must have been ready before it gets
	// new replicas. It keeps a flapping node out of placement.
	DefaultMinReady = 30 * time.Second
	// DefaultLeaseTimeout is how long a node may go without renewing its
	// lease before it counts as lost.
	DefaultLeaseTimeout = 15 * time.Second
)

// maxCores bounds CPU figures so that conversion to millicores cannot overflow.
const maxCores = 1 << 20

// Node is a machine that can run replicas.
type Node struct {
	ID string
	// CPU is the number of cores the node offers.
	CPU float64
	// Memory is the number of bytes the node offers.
	Memory int64
	// Ready is false when the node's agent is not reporting.
	Ready bool
	// ReadySince is when the node last became ready.
	ReadySince time.Time
	// Ineligible is set on a node that an operator made ineligible, or that
	// is being drained. It gets no new replicas. Its existing assignments
	// stay, and count against its capacity and ports as on any node.
	Ineligible bool
	// Labels are not used by Place yet.
	Labels map[string]string
}

// Replica is one copy of an app that needs a node.
type Replica struct {
	App        string
	Index      int
	Generation int64
	Resources  spec.Resources
	// HostPorts are the host ports the replica publishes, written as
	// "addr:port/proto" ("127.0.0.1:80/tcp", "[::1]:53/udp"). An entry
	// without an address ("80/tcp") conflicts with every address on that
	// port. HostPortsOf builds this list from a spec.
	HostPorts []string
	// Volumes are the names of the replica's volumes. Place does not read
	// them; PinnedNode is what keeps a replica with its volumes.
	Volumes []string
	// PinnedNode, when set, is the only node the replica may run on.
	PinnedNode string
	// Bytes is what the replica's assignment takes in the list its node
	// reads. Options.MaxNodeBytes bounds the sum on a node.
	Bytes int64
}

// Assignment puts a replica on a node.
type Assignment struct {
	Replica Replica
	NodeID  string
	// Leaving is set on a copy that goes away without anyone acting: one
	// marked for removal, or a surge, which its rollout or drain promotes or
	// removes. It counts against its node like any other; only Blocked
	// reads the flag.
	Leaving bool
}

// Unplaced is a replica that Place could not put anywhere, and why.
type Unplaced struct {
	Replica Replica
	Reason  string
}

// Options tune Place. The zero value uses the defaults.
type Options struct {
	// MinReady is how long a node must have been ready before it gets new
	// replicas. Zero means DefaultMinReady; a negative value means no wait.
	MinReady time.Duration
	// MaxNodeBytes bounds the Bytes of the replicas on one node, existing
	// and new. Zero means no bound.
	MaxNodeBytes int64
}

// NodeLease is the last time a node's agent renewed its lease.
type NodeLease struct {
	NodeID    string
	RenewedAt time.Time
}

// LostNodes returns, sorted, the IDs of the nodes whose lease has not been
// renewed for more than timeout. A timeout of zero or less means
// DefaultLeaseTimeout. When a node has several leases, the latest counts.
func LostNodes(nodes []NodeLease, now time.Time, timeout time.Duration) []string {
	if timeout <= 0 {
		timeout = DefaultLeaseTimeout
	}
	latest := make(map[string]time.Time, len(nodes))
	for _, l := range nodes {
		if t, ok := latest[l.NodeID]; !ok || l.RenewedAt.After(t) {
			latest[l.NodeID] = l.RenewedAt
		}
	}
	var lost []string
	for id, t := range latest {
		if now.Sub(t) > timeout {
			lost = append(lost, id)
		}
	}
	sort.Strings(lost)
	return lost
}

// HostPortsOf returns the host ports an app publishes in the form
// Replica.HostPorts expects, sorted. Unlike spec.App.HostPorts it keeps the
// bind address. An empty bind means spec.DefaultBind and an empty protocol
// means tcp, as in spec.App.Normalize.
func HostPortsOf(app spec.App) []string {
	out := make([]string, 0, len(app.Ports))
	for _, p := range app.Ports {
		bind := p.Bind
		if bind == "" {
			bind = spec.DefaultBind
		}
		proto := strings.ToLower(p.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		key := strconv.Itoa(p.Host) + "/" + proto
		if a, err := netip.ParseAddr(bind); err == nil && p.Host >= 0 && p.Host <= 65535 {
			key = netip.AddrPortFrom(a.WithZone(""), uint16(p.Host)).String() + "/" + proto
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Place assigns pending replicas to nodes. It returns the new assignments and
// the replicas it could not place; together they hold every pending replica
// once. Existing assignments are never moved; they only count against their
// node's capacity and ports.
//
// Rules:
//   - A node gets a new replica only if it is Ready, is not Ineligible, and
//     has been ready for at least opts.MinReady.
//   - The CPU and memory of existing and new replicas on a node never exceed
//     the node's totals.
//   - No two replicas on a node publish conflicting host ports.
//   - With opts.MaxNodeBytes set, a replica goes to a node only if the
//     Bytes of the node's replicas, with it, stay within it.
//   - A replica with PinnedNode set goes to that node or nowhere.
//   - Among nodes that fit, a replica goes to the node with the fewest
//     replicas of its app, then the least loaded, then the lowest ID.
//
// Pinned replicas are placed first, then the rest ordered by app, index and
// generation. Node IDs should be unique; for a repeated ID the first node
// is used.
func Place(nodes []Node, existing []Assignment, pending []Replica, now time.Time, opts Options) (placed []Assignment, unplaced []Unplaced) {
	states, byID, why, minReady := loadNodes(nodes, existing, now, opts)

	order := make([]Replica, len(pending))
	copy(order, pending)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if (a.PinnedNode != "") != (b.PinnedNode != "") {
			return a.PinnedNode != ""
		}
		if a.App != b.App {
			return a.App < b.App
		}
		if a.Index != b.Index {
			return a.Index < b.Index
		}
		return a.Generation < b.Generation
	})

	for _, r := range order {
		s, reason := choose(r, states, byID, why, minReady, opts.MaxNodeBytes)
		if s == nil {
			unplaced = append(unplaced, Unplaced{Replica: r, Reason: reason})
			continue
		}
		cpu, mem, _ := request(r.Resources)
		s.add(r.App, cpu, mem, r.Bytes, heldPorts(r.HostPorts, r.App, false))
		placed = append(placed, Assignment{Replica: r, NodeID: s.id})
	}
	return placed, unplaced
}

// loadNodes is the nodes as Place starts from them: sorted by ID, a
// repeated ID dropped, each with its existing assignments counted. It also
// returns the nodes by ID, what kinds of ready node there are, and the
// minimum ready time that opts means.
func loadNodes(nodes []Node, existing []Assignment, now time.Time, opts Options) ([]*nodeState, map[string]*nodeState, reasons, time.Duration) {
	minReady := opts.MinReady
	if minReady == 0 {
		minReady = DefaultMinReady
	}

	byID := make(map[string]*nodeState, len(nodes))
	states := make([]*nodeState, 0, len(nodes))
	// why only chooses the reason given for a replica that no node takes.
	var why reasons
	for _, n := range nodes {
		if _, dup := byID[n.ID]; dup {
			continue
		}
		s := &nodeState{
			id:         n.ID,
			cpuCap:     capMillis(n.CPU),
			memCap:     max(n.Memory, 0),
			eligible:   n.Ready && !n.Ineligible && now.Sub(n.ReadySince) >= minReady,
			ineligible: n.Ineligible,
			perApp:     map[string]int{},
			maxBytes:   max(opts.MaxNodeBytes, 0),
		}
		switch {
		case n.Ready && n.Ineligible:
			why.closed = true
		case n.Ready:
			why.open = true
		}
		byID[n.ID] = s
		states = append(states, s)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].id < states[j].id })

	for _, a := range existing {
		s := byID[a.NodeID]
		if s == nil {
			continue
		}
		cpu, mem, ok := request(a.Replica.Resources)
		if !ok {
			// Count an unreadable request as filling the node.
			cpu, mem = s.cpuCap+1, s.memCap
		}
		s.add(a.Replica.App, cpu, mem, a.Replica.Bytes, heldPorts(a.Replica.HostPorts, a.Replica.App, a.Leaving))
	}
	return states, byID, why, minReady
}

// heldPorts parses the host ports of a copy of app on a node.
func heldPorts(list []string, app string, leaving bool) []hostPort {
	out := parsePorts(list)
	for i := range out {
		out[i].app, out[i].leaving = app, leaving
	}
	return out
}

// Block says more about a replica that Place could not put anywhere than
// its reason does.
type Block struct {
	// Holders are the apps whose copies publish a host port the replica
	// needs, on the eligible nodes where that port is what stops it: the
	// nodes its reason counts as having the port in use. Sorted; empty when
	// a port stops it nowhere.
	Holders []string
	// Lasting is set when nothing changes by itself to let the replica onto
	// any of the nodes: on each one it needs more CPU or memory than the
	// node has in all, or a host port that a copy which is not Leaving
	// holds. A node that is not ready, not eligible, or full only of what
	// is leaving may take it later, so it is false then, and with no nodes.
	Lasting bool
}

// Blocked explains why Place, given the same nodes, existing assignments,
// time and options, put the replica r nowhere. It changes nothing, and does
// not check that Place would in fact leave r unplaced.
func Blocked(nodes []Node, existing []Assignment, r Replica, now time.Time, opts Options) Block {
	states, byID, _, _ := loadNodes(nodes, existing, now, opts)
	cpu, mem, ok := request(r.Resources)
	if !ok {
		return Block{Lasting: len(states) > 0}
	}
	if r.PinnedNode != "" {
		s := byID[r.PinnedNode]
		if s == nil {
			return Block{}
		}
		states = []*nodeState{s}
	}
	ports := parsePorts(r.HostPorts)
	holders := map[string]bool{}
	lasting := len(states) > 0
	for _, s := range states {
		if f, _ := s.check(cpu, mem, r.Bytes, ports); s.eligible && f == portInUse {
			for _, p := range ports {
				for _, q := range s.ports {
					if p.conflicts(q) {
						holders[q.app] = true
					}
				}
			}
		}
		lasting = lasting && s.neverFits(cpu, mem, ports)
	}
	return Block{Holders: sortedKeys(holders), Lasting: lasting}
}

// neverFits reports whether a replica with the given request and host ports
// cannot go on s until something that is not leaving changes: it needs more
// than s has in all, or a port that such a copy holds.
func (s *nodeState) neverFits(cpu, mem int64, ports []hostPort) bool {
	if cpu > s.cpuCap || mem > s.memCap {
		return true
	}
	for _, p := range ports {
		for _, q := range s.ports {
			if !q.leaving && p.conflicts(q) {
				return true
			}
		}
	}
	return false
}

type nodeState struct {
	id         string
	cpuCap     int64 // millicores
	memCap     int64
	cpuUsed    int64
	memUsed    int64
	bytes      int64 // Replica.Bytes in use
	maxBytes   int64 // Options.MaxNodeBytes; 0 is no bound
	eligible   bool
	ineligible bool // Node.Ineligible
	total      int
	perApp     map[string]int
	ports      []hostPort
}

// reasons records which kinds of ready node exist, for the reason given when
// no node takes a replica.
type reasons struct {
	open   bool // a ready node that is not Ineligible
	closed bool // a ready node that is Ineligible
}

func (s *nodeState) add(app string, cpu, mem, bytes int64, ports []hostPort) {
	s.bytes = satAdd(s.bytes, max(bytes, 0))
	s.cpuUsed = satAdd(s.cpuUsed, cpu)
	s.memUsed = satAdd(s.memUsed, mem)
	s.total++
	s.perApp[app]++
	s.ports = append(s.ports, ports...)
}

// load is the larger of the CPU and memory fractions in use.
func (s *nodeState) load() float64 {
	return max(fraction(s.cpuUsed, s.cpuCap), fraction(s.memUsed, s.memCap))
}

func fraction(used, capacity int64) float64 {
	if capacity <= 0 {
		if used <= 0 {
			return 0
		}
		return math.Inf(1)
	}
	return float64(used) / float64(capacity)
}

// failure says why one node cannot take a replica.
type failure int

const (
	fits failure = iota
	noCPU
	noMemory
	portInUse
	noRoom // the node's assignments are at MaxNodeBytes
)

// check reports whether the replica fits on s, and if not, why. For a port
// conflict it also returns the replica's port that conflicts.
func (s *nodeState) check(cpu, mem, bytes int64, ports []hostPort) (failure, string) {
	if cpu > s.cpuCap-s.cpuUsed {
		return noCPU, ""
	}
	if mem > s.memCap-s.memUsed {
		return noMemory, ""
	}
	for _, p := range ports {
		for _, q := range s.ports {
			if p.conflicts(q) {
				return portInUse, p.raw
			}
		}
	}
	if s.maxBytes > 0 && bytes > s.maxBytes-s.bytes {
		return noRoom, ""
	}
	return fits, ""
}

func choose(r Replica, states []*nodeState, byID map[string]*nodeState, why reasons, minReady time.Duration, maxBytes int64) (*nodeState, string) {
	cpu, mem, ok := request(r.Resources)
	if !ok {
		return nil, fmt.Sprintf("invalid resources: cpu %v, memory %d", r.Resources.CPU, int64(r.Resources.Memory))
	}
	ports := parsePorts(r.HostPorts)

	if r.PinnedNode != "" {
		s := byID[r.PinnedNode]
		if s != nil && s.ineligible {
			return nil, fmt.Sprintf("volume pinned to node %s, which is ineligible", r.PinnedNode)
		}
		if s == nil || !s.eligible {
			return nil, fmt.Sprintf("volume pinned to node %s, which is unavailable", r.PinnedNode)
		}
		switch f, port := s.check(cpu, mem, r.Bytes, ports); f {
		case fits:
			return s, ""
		case noCPU:
			return nil, fmt.Sprintf("volume pinned to node %s, which does not have %s free", r.PinnedNode, cpuText(r.Resources.CPU))
		case noMemory:
			return nil, fmt.Sprintf("volume pinned to node %s, which does not have %s of memory free", r.PinnedNode, memText(mem))
		case noRoom:
			return nil, fmt.Sprintf("volume pinned to node %s, whose assignments would be more than the %s a node reads", r.PinnedNode, mibText(maxBytes))
		default:
			return nil, fmt.Sprintf("volume pinned to node %s, where host port %s is in use", r.PinnedNode, port)
		}
	}

	var best *nodeState
	eligible := 0
	counts := map[failure]int{}
	conflicting := map[string]bool{}
	for _, s := range states {
		if !s.eligible {
			continue
		}
		eligible++
		f, port := s.check(cpu, mem, r.Bytes, ports)
		if f != fits {
			counts[f]++
			if f == portInUse {
				conflicting[port] = true
			}
			continue
		}
		if best == nil || better(s, best, r.App) {
			best = s
		}
	}
	if best != nil {
		return best, ""
	}

	switch {
	case eligible == 0 && why.open && minReady > 0:
		return nil, fmt.Sprintf("no node has been ready for %s", minReady)
	case eligible == 0 && why.closed:
		return nil, "every ready node is ineligible for new replicas"
	case eligible == 0:
		return nil, "no node is ready"
	case counts[noCPU] == eligible:
		return nil, fmt.Sprintf("no node has %s free", cpuText(r.Resources.CPU))
	case counts[noMemory] == eligible:
		return nil, fmt.Sprintf("no node has %s of memory free", memText(mem))
	case counts[portInUse] == eligible:
		list := sortedKeys(conflicting)
		if len(list) == 1 {
			return nil, fmt.Sprintf("host port %s is in use on every eligible node", list[0])
		}
		return nil, fmt.Sprintf("host ports %s are in use on the eligible nodes", strings.Join(list, ", "))
	case counts[noRoom] == eligible:
		return nil, fmt.Sprintf("every eligible node's assignments would be more than the %s a node reads; "+
			"use fewer replicas, a smaller spec or more nodes", mibText(maxBytes))
	}
	var parts []string
	if n := counts[noCPU]; n > 0 {
		parts = append(parts, fmt.Sprintf("%s without %s free", nodesText(n), cpuText(r.Resources.CPU)))
	}
	if n := counts[noMemory]; n > 0 {
		parts = append(parts, fmt.Sprintf("%s without %s of memory free", nodesText(n), memText(mem)))
	}
	if n := counts[portInUse]; n > 0 {
		parts = append(parts, fmt.Sprintf("%s with host port %s in use", nodesText(n), strings.Join(sortedKeys(conflicting), ", ")))
	}
	if n := counts[noRoom]; n > 0 {
		parts = append(parts, fmt.Sprintf("%s whose assignments would be more than the %s a node reads", nodesText(n), mibText(maxBytes)))
	}
	return nil, "no eligible node fits: " + strings.Join(parts, "; ")
}

// better reports whether a is a better home than b for a replica of app.
func better(a, b *nodeState, app string) bool {
	if x, y := a.perApp[app], b.perApp[app]; x != y {
		return x < y
	}
	if x, y := a.load(), b.load(); x != y {
		return x < y
	}
	if a.total != b.total {
		return a.total < b.total
	}
	return a.id < b.id
}

// request converts a replica's resources to millicores and bytes. CPU is
// rounded up, so a node never holds more than its CPU figure.
func request(r spec.Resources) (cpu, mem int64, ok bool) {
	c := r.CPU
	if math.IsNaN(c) || c < 0 || c > maxCores || r.Memory < 0 {
		return 0, 0, false
	}
	return int64(math.Ceil(c*1000 - 1e-6)), int64(r.Memory), true
}

// capMillis converts a node's CPU to millicores, rounded down.
func capMillis(c float64) int64 {
	if math.IsNaN(c) || c <= 0 {
		return 0
	}
	if c > maxCores {
		c = maxCores
	}
	return int64(math.Floor(c*1000 + 1e-6))
}

func satAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func cpuText(c float64) string {
	s := strconv.FormatFloat(c, 'f', -1, 64)
	if c == 1 {
		return s + " CPU"
	}
	return s + " CPUs"
}

func memText(m int64) string { return spec.ByteSize(m).String() }

func nodesText(n int) string {
	if n == 1 {
		return "1 node"
	}
	return strconv.Itoa(n) + " nodes"
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mibText(n int64) string {
	if n%(1<<20) == 0 {
		return strconv.FormatInt(n>>20, 10) + " MiB"
	}
	return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
}
