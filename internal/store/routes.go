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

// Route is one route in the routing table: a host, or a path prefix of one.
type Route struct {
	Host string
	// Path is the route's path prefix, or "" for the whole host.
	Path string
	App  string
	// Port is the container port the route names.
	Port int
	// Protocol is what the routers speak to the replicas: "" for HTTP/1.1,
	// or spec.RouteH2C. It is the app spec's.
	Protocol string
	// Endpoints are the address:port of each replica that can take traffic
	// now, sorted. Empty when none can.
	Endpoints []string
}

// Key is the route's host and path.
func (r Route) Key() spec.RouteKey { return spec.RouteKey{Host: r.Host, Path: r.Path} }

// RouteTable returns the routing table, sorted by host and then path, the
// whole host first. Every route that a live app has is in it, with or
// without endpoints.
//
// A route's protocol is in its app's spec, not in app_routes, so the store
// needs no new schema for it, and a build before protocols can still open
// the store. The spec is decoded only for an app whose stored JSON holds
// "protocol":"h2c", as encoding/json writes an h2c route; the routes of
// every other app are HTTP/1.1. A spec that holds that text elsewhere is
// decoded for nothing.
func (s *Store) RouteTable(ctx context.Context) ([]Route, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT h.host, h.path, h.app, h.port,
		CASE WHEN instr(a.spec, '"protocol":"h2c"') > 0 THEN a.spec ELSE '' END
		FROM app_routes h JOIN apps a ON a.name = h.app
		WHERE a.deleted = 0 ORDER BY h.host, h.path`)
	if err != nil {
		return nil, err
	}
	var out []Route
	apps := map[string]bool{}
	protocols := map[string]map[spec.RouteKey]string{}
	for rows.Next() {
		var r Route
		var specJSON string
		if err := rows.Scan(&r.Host, &r.Path, &r.App, &r.Port, &specJSON); err != nil {
			rows.Close()
			return nil, err
		}
		if specJSON != "" {
			byKey, ok := protocols[r.App]
			if !ok {
				var a spec.App
				if err := json.Unmarshal([]byte(specJSON), &a); err != nil {
					rows.Close()
					return nil, fmt.Errorf("app %s: stored spec: %w", r.App, err)
				}
				byKey = map[spec.RouteKey]string{}
				for _, sr := range a.Routes {
					byKey[sr.Key()] = sr.Protocol
				}
				protocols[r.App] = byKey
			}
			r.Protocol = byKey[r.Key()]
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

	// A node that is waiting for its first report after serve started (see
	// Waiting) keeps the endpoints it last reported for the whole start
	// grace, as ReconcileNodes keeps its replicas: its containers ran on
	// while the server was away, and its router served them. A stale
	// endpoint costs a router a dial before it tries the next replica; none
	// costs a 503. A node that was already lost before the start is not
	// brought back. The server's own node's reports from before the start
	// are left out: after a restore on a new machine those containers are
	// gone, and its agent reports again within a pass. The soak through the
	// routers saw 502s for a second after a restore, from the restored
	// machine's stale endpoints.
	now := s.now()
	cutoff := now.UnixNano() - int64(scheduler.DefaultLeaseTimeout)
	waitFrom, waitBefore := s.waitWindow(now)
	started := s.startedAt()
	rows, err = s.rdb.QueryContext(ctx, `SELECT c.app, c.route_ports, n.address FROM container_status c JOIN nodes n ON n.id = c.node_id
		WHERE c.ready = 1 AND c.route_ports != '' AND n.address != ''
		AND (n.last_lease >= ? OR (n.last_lease >= ? AND n.last_lease < ?))
		AND NOT (c.node_id = (SELECT node_id FROM cluster WHERE id = 1) AND c.updated_at < ?)
		AND (EXISTS (SELECT 1 FROM replicas r WHERE r.app = c.app AND r.idx = c.idx AND r.node_id = c.node_id
				AND r.generation = c.generation AND r.desired = 'running')
			OR EXISTS (SELECT 1 FROM surges g WHERE g.app = c.app AND g.idx = c.idx AND g.node_id = c.node_id
				AND g.generation = c.generation AND g.desired = 'running'))
		ORDER BY c.app, c.node_id, c.name`, cutoff, waitFrom, waitBefore, started)
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
