package compose

import (
	"fmt"

	"github.com/ao/hivemind/internal/spec"
)

// extensionKey is the service extension that holds what an app spec has and
// Compose does not: routes, and the health checks Compose cannot give.
const extensionKey = "x-hivemind"

// extension is what a service's x-hivemind gives its app, besides its
// routes.
type extension struct {
	// health is its health check, and healthGiven whether it has a health
	// key with a value.
	health      *spec.Health
	healthGiven bool
	// routesRead and healthRead are true when every value in the routes,
	// and in the health check, could be read, though the file may have
	// other problems: the app spec's rules can then check them in the same
	// run (checkExtension).
	routesRead, healthRead bool
}

// hivemindExtension reads a service's x-hivemind: a mapping with routes, a
// list of routes as in an app spec, and health, a health check as in an app
// spec. Any other key in it is refused, an x- key too: the mapping is
// hivemind's, and a key it does not read is a mistake, not an extension.
func (c *converter) hivemindExtension(path string, v *node, a *spec.App) extension {
	switch v.kind {
	case nullKind:
		return extension{}
	case mapKind:
	default:
		c.add(path, "want a mapping, such as {routes: [{host: www.example.com, port: 80}]}")
		return extension{}
	}
	ext := extension{routesRead: true}
	for _, k := range v.keys {
		p := path + "." + k
		switch k {
		case "routes":
			a.Routes, ext.routesRead = c.routes(p, v.vals[k])
		case "health":
			ext.health, ext.healthRead = c.health(p, v.vals[k])
			ext.healthGiven = v.vals[k].kind != nullKind
		default:
			c.refuse(p, "x-hivemind has routes and health only")
		}
	}
	return ext
}

// checkExtension applies the app spec's rules for routes and health checks
// to what a service's x-hivemind gave, when the service has other problems:
// Validate does not run then, and a problem in a value that reads, such as
// protocol: grpc, would otherwise be reported only once the others were
// fixed. Values that could not be read are not checked, so a route whose
// port is not a number gets no second problem for it. routes and health say
// which to check.
func (c *converter) checkExtension(path string, a spec.App, routes, health bool) {
	x := spec.App{Routes: a.Routes, Health: a.Health}
	x.Normalize()
	var errs []error
	if routes {
		errs = append(errs, spec.ValidateRoutes(x.Routes)...)
	}
	if health {
		errs = append(errs, spec.ValidateHealth(x.Health)...)
	}
	for _, err := range errs {
		c.add(path, "%v", err)
	}
}

// routes reads a list of routes, each a mapping of host, port and the
// optional path and protocol, which the app spec checks. It reports whether
// every value in it could be read; a key it refuses does not count.
func (c *converter) routes(path string, v *node) ([]spec.Route, bool) {
	switch v.kind {
	case nullKind:
		return nil, true
	case seqKind:
	default:
		c.add(path, "want a list of routes, each with host, port and, optionally, path and protocol")
		return nil, false
	}
	var out []spec.Route
	read := true
	for i, it := range v.items {
		p := fmt.Sprintf("%s[%d]", path, i)
		if it.kind != mapKind {
			c.add(p, "want a mapping with host, port and, optionally, path and protocol")
			read = false
			continue
		}
		var r spec.Route
		ok := true
		for _, k := range it.keys {
			val, kp := it.vals[k], p+"."+k
			switch k {
			case "host":
				r.Host, ok = c.str(kp, val)
			case "path":
				r.Path, ok = c.str(kp, val)
			case "protocol":
				r.Protocol, ok = c.str(kp, val)
			case "port":
				r.Port, ok = c.integer(kp, val)
			default:
				c.refuse(kp, "a route has host, port, path and protocol only")
			}
			read = read && ok
			ok = true
		}
		for _, k := range []string{"host", "port"} {
			if !present(it, k) {
				c.add(p+"."+k, "required")
				read = false
			}
		}
		out = append(out, r)
	}
	return out, read
}

// health reads x-hivemind's health: a health check as in an app spec, one
// of http (a port and an optional path), tcp (a port) and exec (a command
// and its arguments, as a list), with the optional interval, timeout,
// retries and start_period. It gives a service the checks a healthcheck
// cannot: Compose has no TCP or HTTP check, and an image without a shell
// or a probe, as many gRPC servers are, can run no exec check. The app spec
// checks the values. It reports whether every value in it could be read; a
// key it refuses does not count.
func (c *converter) health(path string, v *node) (*spec.Health, bool) {
	switch v.kind {
	case nullKind:
		return nil, true
	case mapKind:
	default:
		c.add(path, "want a mapping, such as {tcp: {port: 50051}}")
		return nil, false
	}
	h := &spec.Health{}
	read := true
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		before, ok := len(c.problems), true
		switch k {
		case "http":
			h.HTTP, ok = c.httpCheck(p, val)
		case "tcp":
			h.TCP, ok = c.tcpCheck(p, val)
		case "exec":
			h.Exec, ok = c.execCheck(p, val)
		case "interval":
			h.Interval = c.duration(p, val)
		case "timeout":
			h.Timeout = c.duration(p, val)
		case "start_period":
			h.StartPeriod = c.duration(p, val)
		case "retries":
			h.Retries, ok = c.integer(p, val)
		default:
			c.refuse(p, "a health check has http, tcp, exec, interval, timeout, retries and start_period only")
			continue
		}
		// A duration says it did not read only in a problem.
		read = read && ok && len(c.problems) == before
	}
	return h, read
}

// httpCheck reads an http check: a port, and an optional path.
func (c *converter) httpCheck(path string, v *node) (*spec.HTTPCheck, bool) {
	if v.kind != mapKind {
		c.add(path, "want a mapping with port and, optionally, path, such as {port: 8080, path: /healthz}")
		return nil, false
	}
	hc := &spec.HTTPCheck{}
	read := true
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		ok := true
		switch k {
		case "port":
			hc.Port, ok = c.integer(p, val)
		case "path":
			hc.Path, ok = c.str(p, val)
		default:
			c.refuse(p, "an http check has port and path only")
		}
		read = read && ok
	}
	if !present(v, "port") {
		c.add(path+".port", "required")
		read = false
	}
	return hc, read
}

// tcpCheck reads a tcp check: a port.
func (c *converter) tcpCheck(path string, v *node) (*spec.TCPCheck, bool) {
	if v.kind != mapKind {
		c.add(path, "want a mapping with port, such as {port: 50051}")
		return nil, false
	}
	tc := &spec.TCPCheck{}
	read := true
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		switch k {
		case "port":
			var ok bool
			tc.Port, ok = c.integer(p, val)
			read = read && ok
		default:
			c.refuse(p, "a tcp check has port only")
		}
	}
	if !present(v, "port") {
		c.add(path+".port", "required")
		read = false
	}
	return tc, read
}

// execCheck reads an exec check: the command and its arguments, as a list.
// It runs as it is, with no shell, as in an app spec.
func (c *converter) execCheck(path string, v *node) ([]string, bool) {
	if v.kind != seqKind || len(v.items) == 0 {
		c.add(path, "want a list: the command and its arguments, such as [grpc_health_probe, -addr=:50051]")
		return nil, false
	}
	out := make([]string, 0, len(v.items))
	read := true
	for i, it := range v.items {
		s, ok := c.str(fmt.Sprintf("%s[%d]", path, i), it)
		if ok {
			out = append(out, s)
		}
		read = read && ok
	}
	return out, read
}
