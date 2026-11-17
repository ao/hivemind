package compose

import (
	"fmt"

	"github.com/ao/hivemind/internal/spec"
)

// extensionKey is the service extension that holds what an app spec has and
// Compose does not: routes.
const extensionKey = "x-hivemind"

// hivemindExtension reads a service's x-hivemind: a mapping with routes, a
// list of routes as in an app spec. Any other key in it is refused, an x-
// key too: the mapping is hivemind's, and a key it does not read is a
// mistake, not an extension.
func (c *converter) hivemindExtension(path string, v *node, a *spec.App) {
	switch v.kind {
	case nullKind:
		return
	case mapKind:
	default:
		c.add(path, "want a mapping, such as {routes: [{host: www.example.com, port: 80}]}")
		return
	}
	for _, k := range v.keys {
		p := path + "." + k
		switch k {
		case "routes":
			a.Routes = c.routes(p, v.vals[k])
		default:
			c.refuse(p, "x-hivemind has routes only")
		}
	}
}

// routes reads a list of routes, each a mapping of host, port and an
// optional path, which the app spec checks.
func (c *converter) routes(path string, v *node) []spec.Route {
	switch v.kind {
	case nullKind:
		return nil
	case seqKind:
	default:
		c.add(path, "want a list of routes, each with host, port and, optionally, path")
		return nil
	}
	var out []spec.Route
	for i, it := range v.items {
		p := fmt.Sprintf("%s[%d]", path, i)
		if it.kind != mapKind {
			c.add(p, "want a mapping with host, port and, optionally, path")
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
			case "port":
				r.Port, _ = c.integer(kp, val)
			default:
				c.refuse(kp, "a route has host, port and path only")
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
