package server

import "net/http"

// RouteView is one host of the routing table, as GET /v1/routes shows it.
type RouteView struct {
	Host string `json:"host"`
	App  string `json:"app"`
	// Port is the container port the route names.
	Port int `json:"port"`
	// Endpoints are the address:port of each replica that takes the host's
	// traffic now: Ready, assigned, and on a node with a current lease.
	Endpoints []string `json:"endpoints"`
}

// RouteList is the reply to GET /v1/routes.
type RouteList struct {
	Routes []RouteView `json:"routes"`
}

// listRoutes serves GET /v1/routes: the routing table every router serves.
func (s *Server) listRoutes(w http.ResponseWriter, r *http.Request) {
	rt, err := s.cfg.Store.RouteTable(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := RouteList{Routes: make([]RouteView, 0, len(rt))}
	for _, x := range rt {
		eps := x.Endpoints
		if eps == nil {
			eps = []string{}
		}
		out.Routes = append(out.Routes, RouteView{Host: x.Host, App: x.App, Port: x.Port, Endpoints: eps})
	}
	writeJSON(w, http.StatusOK, out)
}
