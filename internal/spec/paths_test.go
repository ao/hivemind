package spec

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRoutePaths(t *testing.T) {
	long := "/" + strings.Repeat("a", MaxRoutePathLen-1)
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"/", ""},
		{"/api", "/api"},
		{"/api/", "/api"},
		{"/api/v2", "/api/v2"},
		{"/API/V2/", "/API/V2"},
		{"/a.b/c-d_e~f", "/a.b/c-d_e~f"},
		{"/x/!$&'()*+,=:@", "/x/!$&'()*+,=:@"},
		{"/...", "/..."},
		{"/.well-known", "/.well-known"},
		{long, long},
	} {
		got := NormalizePath(c.in)
		if got != c.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
		if err := ValidatePath(got); err != nil {
			t.Errorf("%q refused: %v", c.in, err)
		}
	}
	for _, c := range []struct{ in, msg string }{
		{"api", "must start with /"},
		{" /api", "must start with /"},
		{"//", "empty segment"},
		{"//api", "empty segment"},
		{"/api//v2", "empty segment"},
		{"/api//", "empty segment"},
		{"/.", "a . segment"},
		{"/..", "a .. segment"},
		{"/api/../admin", "a .. segment"},
		{"/api/./x", "a . segment"},
		{"/api/..", "a .. segment"},
		{"/api?x=1", `'?'`},
		{"/api#x", `'#'`},
		{"/api%2Fx", `'%'`},
		{"/%61pi", `'%'`},
		{"/api;v=1", `';'`},
		{"/api x", "a space"},
		{"/api\tx", "byte 0x09"},
		{"/api\x00", "byte 0x00"},
		{"/api\x7f", "byte 0x7f"},
		{`/api\x`, `'\\'`},
		{"/café", "byte 0xc3"},
		{"/a<b", `'<'`},
		{"/a\"b", `'"'`},
		{"/a[0]", `'['`},
		{"/a{b}", `'{'`},
		{"/a|b", `'|'`},
		{"/a^b", `'^'`},
		{"/a`b", "'`'"},
		{long + "a", "longer than 256"},
	} {
		err := ValidatePath(NormalizePath(c.in))
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%q: %v, want an error that says %s", c.in, err, c.msg)
		}
	}
}

func TestRoutesWithPaths(t *testing.T) {
	a := valid()
	a.Routes = []Route{
		{Host: "www.example.com", Port: 80},
		{Host: "www.example.com", Path: "/api/", Port: 8080},
		{Host: "WWW.example.com.", Path: "/admin", Port: 8081},
		{Host: "api.example.com", Path: "/", Port: 8080},
	}
	a.Normalize()
	if err := a.Validate(); err != nil {
		t.Fatalf("routes with paths refused: %v", err)
	}
	want := []Route{{"www.example.com", "", 80}, {"www.example.com", "/api", 8080}, {"www.example.com", "/admin", 8081},
		{"api.example.com", "", 8080}}
	for i, r := range a.Routes {
		if r != want[i] {
			t.Errorf("route %d normalised to %+v, want %+v", i, r, want[i])
		}
	}
	if got := a.RouteHosts(); len(got) != 2 || got[0] != "www.example.com" || got[1] != "api.example.com" {
		t.Errorf("RouteHosts = %v", got)
	}
	if !a.HasPathRoutes() {
		t.Error("HasPathRoutes is false")
	}
	if k := a.Routes[1].Key(); k.String() != "www.example.com/api" || a.Routes[0].Key().String() != "www.example.com" {
		t.Errorf("keys %v and %v", k, a.Routes[0].Key())
	}

	// One host and path twice in one app is refused, also when one of them
	// is written another way.
	for _, routes := range [][]Route{
		{{Host: "www.example.com", Path: "/api", Port: 80}, {Host: "www.example.com", Path: "/api/", Port: 81}},
		{{Host: "www.example.com", Port: 80}, {Host: "www.example.com", Path: "/", Port: 81}},
	} {
		b := valid()
		b.Routes = routes
		b.Normalize()
		if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "listed twice") {
			t.Errorf("%+v: %v", routes, err)
		}
	}
	b := valid()
	b.Routes = []Route{{Host: "www.example.com", Path: "/api", Port: 80}, {Host: "www.example.com", Path: "/api", Port: 80}}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), `host "www.example.com" with path "/api" listed twice`) {
		t.Errorf("a path twice: %v", err)
	}
	b.Routes = []Route{{Host: "www.example.com", Path: "/a b", Port: 80}}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "routes[0]: path") {
		t.Errorf("a bad path: %v", err)
	}
}

// A spec without paths, and one with the whole host written as "/", has the
// template it had before paths existed, so an upgrade starts no rollout.
func TestRoutePathKeepsOldTemplates(t *testing.T) {
	a := App{Name: "web", Image: "nginx:alpine", Routes: []Route{{Host: "www.example.com", Port: 80}}}
	a.Normalize()
	before := a.Template()
	if want := `"routes":[{"host":"www.example.com","port":80}]`; !strings.Contains(before, want) {
		t.Fatalf("a whole host route is not in the template as before paths: %s", before)
	}
	var b App
	if err := yaml.Unmarshal([]byte("name: web\nimage: nginx:alpine\nroutes:\n  - {host: www.example.com, path: /, port: 80}\n"), &b); err != nil {
		t.Fatal(err)
	}
	b.Normalize()
	if b.Template() != before {
		t.Fatalf("path / changed the template:\n%s\n%s", before, b.Template())
	}
	var c App
	if err := json.Unmarshal([]byte(`{"name":"web","image":"nginx:alpine","routes":[{"host":"www.example.com","path":"/api","port":80}]}`), &c); err != nil {
		t.Fatal(err)
	}
	c.Normalize()
	if err := c.Validate(); err != nil || c.Routes[0].Path != "/api" || c.Template() == before {
		t.Fatalf("a path from JSON: %+v, %v", c.Routes, err)
	}
}
