package store

import (
	"slices"
	"testing"

	"github.com/ao/hivemind/internal/spec"
)

// routeProtocols lists the table's routes as host+path=protocol.
func routeProtocols(t *testing.T, s *Store) []string {
	t.Helper()
	rt, err := s.RouteTable(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rt {
		out = append(out, r.Key().String()+"="+r.Protocol)
	}
	return out
}

// Each route's protocol comes from its app's spec: one app can have routes
// of both protocols, on one host or several, and an app whose spec has the
// text "protocol":"h2c" only in a value has HTTP/1.1 routes. A change of
// protocol shows at once, and so does its removal.
func TestRouteTableProtocols(t *testing.T) {
	s := newT(t)
	grpc := routed("grpc", 1)
	grpc.Routes = []spec.Route{
		{Host: "grpc.example.com", Port: 50051, Protocol: "H2C"},
		{Host: "www.example.com", Path: "/echo.Echo", Port: 50051, Protocol: "h2c"},
		{Host: "www.example.com", Path: "/status", Port: 8080, Protocol: "http"},
	}
	mustPut(t, s, grpc)
	web := routed("web", 1, "www.example.com")
	web.Env = map[string]string{"NOTE": `{"protocol":"h2c"}`, "PROTOCOL": "h2c"}
	mustPut(t, s, web)
	want := []string{"grpc.example.com=h2c", "www.example.com=", "www.example.com/echo.Echo=h2c", "www.example.com/status="}
	if got := routeProtocols(t, s); !slices.Equal(got, want) {
		t.Fatalf("protocols %v, want %v", got, want)
	}

	grpc.Routes[0].Protocol = ""
	grpc.Routes[2].Protocol = "h2c"
	mustPut(t, s, grpc)
	want = []string{"grpc.example.com=", "www.example.com=", "www.example.com/echo.Echo=h2c", "www.example.com/status=h2c"}
	if got := routeProtocols(t, s); !slices.Equal(got, want) {
		t.Fatalf("after the change: %v, want %v", got, want)
	}
	for i := range grpc.Routes {
		grpc.Routes[i].Protocol = ""
	}
	mustPut(t, s, grpc)
	want = []string{"grpc.example.com=", "www.example.com=", "www.example.com/echo.Echo=", "www.example.com/status="}
	if got := routeProtocols(t, s); !slices.Equal(got, want) {
		t.Fatalf("with no h2c route left: %v, want %v", got, want)
	}
}
