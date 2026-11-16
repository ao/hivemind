package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/ao/hivemind/internal/spec"
)

// Ports bound to one address.
//
// A host port bound to a specific address, one that is not loopback and not a
// wildcard, can be published only on the machine that has that address. On
// any other machine Docker refuses the bind with "cannot assign requested
// address". So such a replica goes only to a node whose address is that
// address: a worker's private address, as it reported it, or the server's
// --advertise address. The server's own node, when serve runs without
// --advertise, reports 127.0.0.1. It has no workers then, so it takes a port
// bound to any address, as it always did.
//
// A replica that was placed on another node, before placement checked this or
// because the node's address changed, and whose container does not run there,
// is placed again by the sweep (placeMisplaced). One with a volume stays where
// its data is, and its reason says what to do.

// boundAddrs returns the specific addresses that a's ports bind, sorted,
// without repeats. An IPv4-mapped IPv6 address counts as the IPv4 address.
func boundAddrs(a spec.App) []netip.Addr {
	var out []netip.Addr
	for _, p := range a.Ports {
		ip, err := netip.ParseAddr(p.Bind)
		if err != nil {
			continue // empty: the default, 127.0.0.1
		}
		ip = ip.Unmap().WithZone("")
		if ip.IsLoopback() || ip.IsUnspecified() || slices.Contains(out, ip) {
			continue
		}
		out = append(out, ip)
	}
	slices.SortFunc(out, func(x, y netip.Addr) int { return x.Compare(y) })
	return out
}

// bindPhrase says which of a's host ports bind ip, such as "host port
// 10.0.0.5:5432/tcp binds address 10.0.0.5".
func bindPhrase(a spec.App, ip netip.Addr) string {
	var ports []string
	for _, p := range a.Ports {
		b, err := netip.ParseAddr(p.Bind)
		if err != nil || b.Unmap().WithZone("") != ip {
			continue
		}
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		ports = append(ports, netip.AddrPortFrom(ip, uint16(p.Host)).String()+"/"+proto)
	}
	if len(ports) == 1 {
		return fmt.Sprintf("host port %s binds address %s", ports[0], ip)
	}
	return fmt.Sprintf("host ports %s bind address %s", strings.Join(ports, ", "), ip)
}

// takesAddr reports whether node n can publish a port bound to ip: its
// address is ip, or it is the server's node without --advertise, the only
// node that reports a loopback address.
func takesAddr(n Node, ip netip.Addr) bool {
	a, err := netip.ParseAddr(n.Address)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a == ip || a.IsLoopback()
}

// addrMisfit says why node n cannot publish a's host ports, or returns ""
// when it can.
func addrMisfit(n Node, a spec.App) string {
	for _, ip := range boundAddrs(a) {
		if takesAddr(n, ip) {
			continue
		}
		has := "reported no address"
		if n.Address != "" {
			has = "has address " + n.Address
		}
		return fmt.Sprintf("%s, and node %s %s", bindPhrase(a, ip), n.ID, has)
	}
	return ""
}

// nodesFor returns, in a new slice, the nodes that can run a replica of a:
// for an app with routes, the nodes that reported an address; for an app
// with a port bound to a specific address, the node that has it. When some
// nodes are registered and none of them can, the error says why.
func nodesFor(nodes []Node, a spec.App) ([]Node, error) {
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if (len(a.Routes) > 0 && n.Address == "") || addrMisfit(n, a) != "" {
			continue
		}
		out = append(out, n)
	}
	if len(out) > 0 || len(nodes) == 0 {
		return out, nil
	}
	if len(a.Routes) > 0 && !slices.ContainsFunc(nodes, func(n Node) bool { return n.Address != "" }) {
		return nil, errNoRoutableNode
	}
	return nil, errors.New(noNodeHasAddrs(nodes, a))
}

// noNodeHasAddrs says that no node can publish a's host ports.
func noNodeHasAddrs(nodes []Node, a spec.App) string {
	addrs := boundAddrs(a)
	for _, ip := range addrs {
		if !slices.ContainsFunc(nodes, func(n Node) bool { return takesAddr(n, ip) }) {
			return fmt.Sprintf("no node has address %s, and %s; a node's address is the one it reaches the server from, "+
				"or the server's --advertise address (hivemind nodes --json lists them)", ip, bindPhrase(a, ip))
		}
	}
	return fmt.Sprintf("the host ports bind addresses %s, and no one node has all of them", addrList(addrs))
}

// onlyOn says which nodes can run a replica of a whose host ports bind a
// specific address, for a reason that placement gave on those nodes alone.
func onlyOn(candidates []Node, a spec.App) string {
	ids := make([]string, 0, len(candidates))
	for _, n := range candidates {
		ids = append(ids, n.ID)
	}
	addrs := boundAddrs(a)
	what := bindPhrase(a, addrs[0])
	if len(addrs) > 1 {
		what = "its host ports bind addresses " + addrList(addrs)
	}
	if len(ids) == 1 {
		return fmt.Sprintf("%s, which only node %s has", what, ids[0])
	}
	return fmt.Sprintf("%s, which only nodes %s have", what, strings.Join(ids, ", "))
}

func addrList(addrs []netip.Addr) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// nodeMisfit says why the node id cannot run a replica of a, or returns ""
// when it can: an app with routes needs a node that reported an address, and
// a port bound to an address needs the node that has it. An unknown node is
// "".
func nodeMisfit(ctx context.Context, tx *sql.Tx, id string, a spec.App) (string, error) {
	var addr string
	err := tx.QueryRowContext(ctx, `SELECT address FROM nodes WHERE id = ?`, id).Scan(&addr)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(a.Routes) > 0 && addr == "" {
		return fmt.Sprintf("the app has routes, and node %s reported no address", id), nil
	}
	return addrMisfit(Node{ID: id, Address: addr}, a), nil
}

// placeMisplaced finds the running replicas whose host ports bind an
// address their node does not have. Each was placed before placement checked
// addresses, or its node's address changed since; either way its container
// cannot start there. A replica without a volume gives up the node and is
// placed again, on the node that has the address if one does. A replica with
// a volume stays, because its data is on that node, and its reason says what
// to do about it.
//
// A replica whose container runs on its node at its generation is left alone:
// the machine has the address, though the node reported another. So is a
// replica with a surge, which a rollout or a drain is moving.
func (s *Store) placeMisplaced(ctx context.Context, tx *sql.Tx) error {
	reps, err := queryAssignments(ctx, tx, `r.desired = 'running' AND r.node_id IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM surges g WHERE g.app = r.app AND g.idx = r.idx)
		AND NOT EXISTS (SELECT 1 FROM container_status c WHERE c.node_id = r.node_id AND c.app = r.app
			AND c.idx = r.idx AND c.generation = r.generation AND c.state = 'running')`)
	if err != nil {
		return err
	}
	var nodes map[string]Node
	now := s.now().UnixNano()
	for _, r := range reps {
		if len(boundAddrs(r.Spec)) == 0 {
			continue
		}
		if nodes == nil {
			list, err := listNodes(ctx, tx)
			if err != nil {
				return err
			}
			nodes = make(map[string]Node, len(list))
			for _, n := range list {
				nodes[n.ID] = n
			}
		}
		n, ok := nodes[r.NodeID]
		if !ok {
			continue
		}
		misfit := addrMisfit(n, r.Spec)
		if misfit == "" {
			continue
		}
		if len(r.Spec.Volumes) == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE replicas SET node_id = NULL, pending_reason = '', updated_at = ? WHERE app = ? AND idx = ?`,
				now, r.App, r.Index); err != nil {
				return err
			}
			continue
		}
		fix := "deploy the app with the port on the node's address"
		if n.Address != "" {
			fix = "deploy the app with the port on " + n.Address
		}
		reason := misfit + "; its volume is on that node, so it stays there. To run it, " + fix +
			", or delete the app and deploy it again to start it, with a new volume, on the node that has the address"
		if _, err := tx.ExecContext(ctx, `UPDATE replicas SET pending_reason = ?, updated_at = ? WHERE app = ? AND idx = ? AND pending_reason != ?`,
			reason, now, r.App, r.Index, reason); err != nil {
			return err
		}
	}
	return nil
}
