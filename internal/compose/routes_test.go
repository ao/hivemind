package compose

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/spec"
)

func TestHivemindExtensionRoutes(t *testing.T) {
	file := `x-routes: &api-routes
  - {host: "${HOST}", path: /api/, port: 8080}
  - {host: "${HOST}", path: /grpc.Echo, port: 50051, protocol: H2C}
services:
  web:
    image: nginx:1.27
    x-hivemind:
      routes:
        - host: ${HOST}
          port: 80
  api:
    image: ghcr.io/x/api:2
    x-hivemind:
      routes: *api-routes
  plain:
    image: nginx:1.27
    x-hivemind:
`
	res, err := Convert([]byte(file), Options{Lookup: env(map[string]string{"HOST": "WWW.Example.com"})})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]spec.Route{}
	for _, a := range res.Apps {
		got[a.Service] = a.Spec.Routes
	}
	want := map[string][]spec.Route{
		"web":   {{Host: "www.example.com", Port: 80}},
		"api":   {{Host: "www.example.com", Path: "/api", Port: 8080}, {Host: "www.example.com", Path: "/grpc.Echo", Port: 50051, Protocol: "h2c"}},
		"plain": nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes %+v, want %+v", got, want)
	}
}

func TestHivemindExtensionRefuses(t *testing.T) {
	for _, c := range []struct{ name, file, path, msg string }{
		{"top level", "x-hivemind: {routes: []}\n" + web(""), "x-hivemind", "x-hivemind goes on a service"},
		{"scalar", web("x-hivemind: yes"), "services.web.x-hivemind", "want a mapping"},
		{"unknown key", web("x-hivemind: {routes: [], replicas: 2}"), "services.web.x-hivemind.replicas", "not supported: x-hivemind has routes only"},
		{"extension key inside", web("x-hivemind: {x-note: a}"), "services.web.x-hivemind.x-note", "x-hivemind has routes only"},
		{"routes not a list", web("x-hivemind: {routes: {host: a.example.com}}"), "services.web.x-hivemind.routes", "want a list of routes"},
		{"route not a mapping", web("x-hivemind: {routes: [www.example.com]}"), "services.web.x-hivemind.routes[0]", "want a mapping"},
		{"unknown route key", web("x-hivemind: {routes: [{host: a.example.com, port: 80, strip_prefix: true}]}"), "services.web.x-hivemind.routes[0].strip_prefix",
			"not supported: a route has host, port, path and protocol only"},
		{"no host", web("x-hivemind: {routes: [{port: 80}]}"), "services.web.x-hivemind.routes[0].host", "required"},
		{"no port", web("x-hivemind: {routes: [{host: a.example.com}]}"), "services.web.x-hivemind.routes[0].port", "required"},
		{"port not a number", web("x-hivemind: {routes: [{host: a.example.com, port: http}]}"), "services.web.x-hivemind.routes[0].port", "want a whole number"},
		{"bad path", web("x-hivemind: {routes: [{host: a.example.com, port: 80, path: /a/../b}]}"), "services.web", "routes[0]: path \"/a/../b\" has a .. segment"},
		{"bad protocol", web("x-hivemind: {routes: [{host: a.example.com, port: 80, protocol: h2}]}"), "services.web", `routes[0]: protocol "h2": use http or h2c`},
		{"protocol not a string", web("x-hivemind: {routes: [{host: a.example.com, port: 80, protocol: [h2c]}]}"), "services.web.x-hivemind.routes[0].protocol", "want a string"},
		{"path twice", web("x-hivemind: {routes: [{host: a.example.com, port: 80, path: /a}, {host: a.example.com, port: 81, path: /a/}]}"), "services.web",
			`routes[1]: host "a.example.com" with path "/a" listed twice`},
		{"unset variable", web("x-hivemind: {routes: [{host: '${UNSET}', port: 80}]}"), "services.web.x-hivemind.routes[0].host", "variable UNSET is not set"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := Convert([]byte(c.file), Options{Lookup: env(nil)})
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("converted: %+v, %v", res, err)
			}
			for _, p := range ce.Problems {
				if p.Path == c.path && strings.Contains(p.Msg, c.msg) {
					return
				}
			}
			t.Fatalf("no problem at %q containing %q in:\n%v", c.path, c.msg, err)
		})
	}
}
