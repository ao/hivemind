package spec

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A route's protocol is http, the default, or h2c, in any case. http is
// stored as "", so a spec that writes it has the template of one that does
// not, and an upgrade starts no rollout. Anything else is refused.
func TestRouteProtocols(t *testing.T) {
	plain := App{Name: "web", Image: "nginx:alpine", Routes: []Route{{Host: "www.example.com", Port: 80}}}
	plain.Normalize()
	for _, p := range []string{"http", "HTTP", " http "} {
		var a App
		if err := yaml.Unmarshal([]byte("name: web\nimage: nginx:alpine\nroutes:\n  - {host: www.example.com, port: 80, protocol: '"+p+"'}\n"), &a); err != nil {
			t.Fatal(err)
		}
		a.Normalize()
		if err := a.Validate(); err != nil {
			t.Fatalf("protocol %q refused: %v", p, err)
		}
		if a.Routes[0].Protocol != "" || a.Template() != plain.Template() || a.Routes[0].ProtocolName() != RouteHTTP {
			t.Fatalf("protocol %q: stored %q, template %s, want that of no protocol", p, a.Routes[0].Protocol, a.Template())
		}
	}
	if strings.Contains(plain.Template(), "protocol") {
		t.Fatalf("a route without a protocol has one in its template: %s", plain.Template())
	}

	a := valid()
	a.Routes = []Route{
		{Host: "grpc.example.com", Port: 50051, Protocol: "H2C"},
		{Host: "www.example.com", Port: 80},
		{Host: "www.example.com", Path: "/grpc.health.v1.Health", Port: 50051, Protocol: "h2c"},
	}
	a.Normalize()
	if err := a.Validate(); err != nil {
		t.Fatalf("h2c routes refused: %v", err)
	}
	for i, want := range []string{RouteH2C, "", RouteH2C} {
		if a.Routes[i].Protocol != want {
			t.Errorf("route %d: protocol %q, want %q", i, a.Routes[i].Protocol, want)
		}
	}
	if a.Routes[0].ProtocolName() != "h2c" || a.Routes[1].ProtocolName() != "http" {
		t.Errorf("names %q and %q", a.Routes[0].ProtocolName(), a.Routes[1].ProtocolName())
	}
	if !strings.Contains(a.Template(), `"protocol":"h2c"`) {
		t.Errorf("an h2c route is not in the template: %s", a.Template())
	}

	for _, p := range []string{"h2", "https", "grpc", "tcp", "http/1.1", "h2c "} {
		b := valid()
		b.Routes = []Route{{Host: "www.example.com", Port: 80, Protocol: p}}
		if p != "h2c " {
			b.Normalize()
		}
		if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "routes[0]: protocol") || !strings.Contains(err.Error(), "use http or h2c") {
			t.Errorf("protocol %q: %v", p, err)
		}
	}
	if err := ValidateRouteProtocol(""); err != nil {
		t.Error(err)
	}
	if err := ValidateRouteProtocol("http"); err == nil {
		t.Error("ValidateRouteProtocol takes http, which Normalize makes empty")
	}
}
