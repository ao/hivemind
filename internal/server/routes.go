package server

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/spec"
)

// RouteView is one route of the routing table, as GET /v1/routes shows it.
type RouteView struct {
	Host string `json:"host"`
	// Path is the route's path prefix, or "/" for the whole host.
	Path string `json:"path"`
	App  string `json:"app"`
	// Port is the container port the route names.
	Port int `json:"port"`
	// Endpoints are the address:port of each replica that takes the
	// route's traffic now: Ready, assigned, and on a node with a current
	// lease.
	Endpoints []string `json:"endpoints"`
}

// RouteList is the reply to GET /v1/routes.
type RouteList struct {
	Routes []RouteView `json:"routes"`
	// OldRouters are the workers' routers that read the table in the last
	// minute without paths, being of a build before path routes. Each
	// answers 503 for the hosts it lists, which have path routes. Absent
	// when there are none.
	OldRouters []nodeapi.OldRouter `json:"old_routers,omitempty"`
}

// listRoutes serves GET /v1/routes: the routing table every router serves.
func (s *Server) listRoutes(w http.ResponseWriter, r *http.Request) {
	rt, err := s.cfg.Store.RouteTable(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := RouteList{Routes: make([]RouteView, 0, len(rt)), OldRouters: s.cfg.RouteReaders.Old(time.Now())}
	for _, x := range rt {
		eps := x.Endpoints
		if eps == nil {
			eps = []string{}
		}
		out.Routes = append(out.Routes, RouteView{Host: x.Host, Path: cmpOr(x.Path, "/"), App: x.App, Port: x.Port, Endpoints: eps})
	}
	writeJSON(w, http.StatusOK, out)
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// routeWarnings says, for an app with routes, which routers answer 503 for
// its hosts because they cannot read the path routes those hosts have.
func routeWarnings(a spec.App, old []nodeapi.OldRouter) []string {
	if len(a.Routes) == 0 || len(old) == 0 {
		return nil
	}
	hosts := a.RouteHosts()
	var out []string
	for _, o := range old {
		var held []string
		for _, h := range o.Hosts {
			if slices.Contains(hosts, h) {
				held = append(held, h)
			}
		}
		if len(held) == 0 {
			continue
		}
		have := "has"
		if len(held) > 1 {
			have = "have"
		}
		out = append(out, fmt.Sprintf("the router on node %s (%s) is of a build before path routes, and answers 503 for %s, which %s path routes; upgrade the node",
			o.Node, cmpOr(o.Version, "no version"), strings.Join(held, ", "), have))
	}
	return out
}
