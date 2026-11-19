package compose

import (
	"fmt"

	"github.com/ao/hivemind/internal/spec"
)

// extensionKey is the service extension that holds what an app spec has and
// Compose does not: routes, and the health checks Compose cannot give.
const extensionKey = "x-hivemind"

// hivemindExtension reads a service's x-hivemind: a mapping with routes, a
// list of routes as in an app spec, and health, a health check as in an app
// spec. Any other key in it is refused, an x- key too: the mapping is
// hivemind's, and a key it does not read is a mistake, not an extension. It
// returns the health check, and whether the mapping has a health key.
func (c *converter) hivemindExtension(path string, v *node, a *spec.App) (*spec.Health, bool) {
	switch v.kind {
	case nullKind:
		return nil, false
	case mapKind:
	default:
		c.add(path, "want a mapping, such as {routes: [{host: www.example.com, port: 80}]}")
		return nil, false
	}
	var h *spec.Health
	given := false
	for _, k := range v.keys {
		p := path + "." + k
		switch k {
		case "routes":
			a.Routes = c.routes(p, v.vals[k])
		case "health":
			h, given = c.health(p, v.vals[k]), v.vals[k].kind != nullKind
		default:
			c.refuse(p, "x-hivemind has routes and health only")
		}
	}
	return h, given
}

// routes reads a list of routes, each a mapping of host, port and the
// optional path and protocol, which the app spec checks.
func (c *converter) routes(path string, v *node) []spec.Route {
	switch v.kind {
	case nullKind:
		return nil
	case seqKind:
	default:
		c.add(path, "want a list of routes, each with host, port and, optionally, path and protocol")
		return nil
	}
	var out []spec.Route
	for i, it := range v.items {
		p := fmt.Sprintf("%s[%d]", path, i)
		if it.kind != mapKind {
			c.add(p, "want a mapping with host, port and, optionally, path and protocol")
			continue
		}
		var r spec.Route
		for _, k := range it.keys {
			val, kp := it.vals[k], p+"."+k
			switch k {
			case "host":
				r.Host, _ = c.str(kp, val)
			case "path":
				r.Path, _ = c.str(kp, val)
			case "protocol":
				r.Protocol, _ = c.str(kp, val)
			case "port":
				r.Port, _ = c.integer(kp, val)
			default:
				c.refuse(kp, "a route has host, port, path and protocol only")
			}
		}
		for _, k := range []string{"host", "port"} {
			if !present(it, k) {
				c.add(p+"."+k, "required")
			}
		}
		out = append(out, r)
	}
	return out
}

// health reads x-hivemind's health: a health check as in an app spec, one
// of http (a port and an optional path), tcp (a port) and exec (a command
// and its arguments, as a list), with the optional interval, timeout,
// retries and start_period. It gives a service the checks a healthcheck
// cannot: Compose has no TCP or HTTP check, and an image without a shell
// or a probe, as many gRPC servers are, can run no exec check. The app spec
// checks the values.
func (c *converter) health(path string, v *node) *spec.Health {
	switch v.kind {
	case nullKind:
		return nil
	case mapKind:
	default:
		c.add(path, "want a mapping, such as {tcp: {port: 50051}}")
		return nil
	}
	h := &spec.Health{}
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		switch k {
		case "http":
			h.HTTP = c.httpCheck(p, val)
		case "tcp":
			h.TCP = c.tcpCheck(p, val)
		case "exec":
			h.Exec = c.execCheck(p, val)
		case "interval":
			h.Interval = c.duration(p, val)
		case "timeout":
			h.Timeout = c.duration(p, val)
		case "start_period":
			h.StartPeriod = c.duration(p, val)
		case "retries":
			h.Retries, _ = c.integer(p, val)
		default:
			c.refuse(p, "a health check has http, tcp, exec, interval, timeout, retries and start_period only")
		}
	}
	return h
}

// httpCheck reads an http check: a port, and an optional path.
func (c *converter) httpCheck(path string, v *node) *spec.HTTPCheck {
	if v.kind != mapKind {
		c.add(path, "want a mapping with port and, optionally, path, such as {port: 8080, path: /healthz}")
		return nil
	}
	hc := &spec.HTTPCheck{}
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		switch k {
		case "port":
			hc.Port, _ = c.integer(p, val)
		case "path":
			hc.Path, _ = c.str(p, val)
		default:
			c.refuse(p, "an http check has port and path only")
		}
	}
	if !present(v, "port") {
		c.add(path+".port", "required")
	}
	return hc
}

// tcpCheck reads a tcp check: a port.
func (c *converter) tcpCheck(path string, v *node) *spec.TCPCheck {
	if v.kind != mapKind {
		c.add(path, "want a mapping with port, such as {port: 50051}")
		return nil
	}
	tc := &spec.TCPCheck{}
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		switch k {
		case "port":
			tc.Port, _ = c.integer(p, val)
		default:
			c.refuse(p, "a tcp check has port only")
		}
	}
	if !present(v, "port") {
		c.add(path+".port", "required")
	}
	return tc
}

// execCheck reads an exec check: the command and its arguments, as a list.
// It runs as it is, with no shell, as in an app spec.
func (c *converter) execCheck(path string, v *node) []string {
	if v.kind != seqKind || len(v.items) == 0 {
		c.add(path, "want a list: the command and its arguments, such as [grpc_health_probe, -addr=:50051]")
		return nil
	}
	out := make([]string, 0, len(v.items))
	for i, it := range v.items {
		if s, ok := c.str(fmt.Sprintf("%s[%d]", path, i), it); ok {
			out = append(out, s)
		}
	}
	return out
}
