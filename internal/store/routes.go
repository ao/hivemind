package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"

	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/spec"
)

// Routing.
//
// An app with routes publishes each route's container port on an ephemeral
// host port on its node's private address, and the agent reports the port it
// got. The routing table maps each route host to the address and port of
// every replica that can take traffic now: its container is Ready (running,
// and passing its health check, the signal a rollout waits for), it is the
// container the replica or its surge is assigned (so a copy a rollout has
// replaced leaves the table before its node stops it), and its node's lease
// is current. A routed replica needs a node that reported an address.

// Limits on what a report may say about route ports.
const (
	maxRoutePorts = 64
)

// errNoRoutableNode is the pending reason of a routed replica when no node
// has an address.
var errNoRoutableNode = errors.New("routed apps run only on nodes that report a private address (hivemind 2.2 or later), and no node does")

// routable returns, in a new slice, the nodes that can run a replica of a:
// every node for an app without routes, and the nodes with an address for
// one with routes.
func routable(nodes []Node, a spec.App) []Node {
	if len(a.Routes) == 0 {
		return slices.Clone(nodes)
	}
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Address != "" {
			out = append(out, n)
		}
	}
	return out
}

// nodeRoutable reports whether the node reported an address, and so can run
// a routed replica.
func nodeRoutable(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var addr string
	err := tx.QueryRowContext(ctx, `SELECT address FROM nodes WHERE id = ?`, id).Scan(&addr)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return addr != "", err
}

// releaseUnroutable takes the running replicas of a routed app off nodes that
// reported no address, so they are placed again where they can run. Without
// it, a recreated app would get its routes on the nodes of its old replicas.
func releaseUnroutable(ctx context.Context, tx *sql.Tx, a spec.App, now int64) error {
	if len(a.Routes) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE replicas SET node_id = NULL, pending_reason = '', updated_at = ?
		WHERE app = ? AND desired = 'running' AND node_id IN (SELECT id FROM nodes WHERE address = '')`, now, a.Name)
	return err
}

// checkNodeAddress checks a node address for the store: empty, or an IP
// address without a zone that is not a wildcard. The node API is stricter
// for workers: it also refuses loopback.
func checkNodeAddress(s string) error {
	if s == "" {
		return nil
	}
	ip, err := netip.ParseAddr(s)
	if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.String() != s {
		return fmt.Errorf("%w: node address %q must be an IP address in canonical form, not a wildcard", ErrInvalid, s)
	}
	return nil
}

func encodeRoutePorts(ps []RoutePort) (string, error) {
	if len(ps) == 0 {
		return "", nil
	}
	if len(ps) > maxRoutePorts {
		return "", fmt.Errorf("more than %d route ports", maxRoutePorts)
	}
	for _, p := range ps {
		if p.Port < 1 || p.Port > 65535 || p.HostPort < 1 || p.HostPort > 65535 {
			return "", fmt.Errorf("route port %d on host port %d is out of range", p.Port, p.HostPort)
		}
	}
	b, err := json.Marshal(ps)
	return string(b), err
}

func decodeRoutePorts(s string) ([]RoutePort, error) {
	if s == "" {
		return nil, nil
	}
	var out []RoutePort
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Route is one host in the routing table.
type Route struct {
	Host string
	App  string
	// Port is the container port the route names.
	Port int
	// Endpoints are the address:port of each replica that can take traffic
	// now, sorted. Empty when none can.
	Endpoints []string
}

// RouteTable returns the routing table, sorted by host. Every host that a
// live app routes is in it, with or without endpoints.
func (s *Store) RouteTable(ctx context.Context) ([]Route, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.host, h.app, h.port FROM app_hosts h JOIN apps a ON a.name = h.app
		WHERE a.deleted = 0 ORDER BY h.host`)
	if err != nil {
		return nil, err
	}
	var out []Route
	apps := map[string]bool{}
	for rows.Next() {
		var r Route
		if err := rows.Scan(&r.Host, &r.App, &r.Port); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		apps[r.App] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}

	cutoff := s.now().Add(-scheduler.DefaultLeaseTimeout).UnixNano()
	rows, err = s.db.QueryContext(ctx, `SELECT c.app, c.route_ports, n.address FROM container_status c JOIN nodes n ON n.id = c.node_id
		WHERE c.ready = 1 AND c.route_ports != '' AND n.address != '' AND n.last_lease >= ?
		AND (EXISTS (SELECT 1 FROM replicas r WHERE r.app = c.app AND r.idx = c.idx AND r.node_id = c.node_id
				AND r.generation = c.generation AND r.desired = 'running')
			OR EXISTS (SELECT 1 FROM surges g WHERE g.app = c.app AND g.idx = c.idx AND g.node_id = c.node_id
				AND g.generation = c.generation AND g.desired = 'running'))
		ORDER BY c.app, c.node_id, c.name`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct {
		app  string
		port int
	}
	endpoints := map[key][]string{}
	for rows.Next() {
		var app, rp, addr string
		if err := rows.Scan(&app, &rp, &addr); err != nil {
			return nil, err
		}
		if !apps[app] {
			continue
		}
		ports, err := decodeRoutePorts(rp)
		if err != nil {
			return nil, fmt.Errorf("app %s: stored route ports: %w", app, err)
		}
		for _, p := range ports {
			k := key{app, p.Port}
			endpoints[k] = append(endpoints[k], net.JoinHostPort(addr, strconv.Itoa(p.HostPort)))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		eps := endpoints[key{out[i].App, out[i].Port}]
		slices.Sort(eps)
		out[i].Endpoints = slices.Compact(slices.Clone(eps))
	}
	return out, nil
}
