package compose

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

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
		{"unknown key", web("x-hivemind: {routes: [], replicas: 2}"), "services.web.x-hivemind.replicas", "not supported: x-hivemind has routes and health only"},
		{"extension key inside", web("x-hivemind: {x-note: a}"), "services.web.x-hivemind.x-note", "x-hivemind has routes and health only"},
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

// x-hivemind takes health as an app spec has it, so a service can have the
// checks Compose cannot give: a TCP check, as routing.md recommends for a
// server that speaks only h2c, and an HTTP check. It is used in place of
// the healthcheck, and given values are interpolated.
func TestHivemindExtensionHealth(t *testing.T) {
	file := `services:
  cart:
    image: kong/grpcbin
    x-hivemind:
      routes: [{host: www.example.com, path: /shop.Cart, port: 9000, protocol: h2c}]
      health: {tcp: {port: 9000}, interval: 5s}
  front:
    image: nginx:1.27
    x-hivemind:
      health:
        http: {port: "${PORT}", path: /healthz}
        timeout: 1s
        retries: 5
        start_period: 30s
  probe:
    image: ghcr.io/x/grpc:1
    x-hivemind:
      health: {exec: [grpc_health_probe, "-addr=:50051"]}
  plain:
    image: nginx:1.27
    x-hivemind: {health: }
`
	res, err := Convert([]byte(file), Options{Lookup: env(map[string]string{"PORT": "8080"})})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*spec.Health{}
	for _, a := range res.Apps {
		got[a.Service] = a.Spec.Health
	}
	sec := func(n int) spec.Duration { return spec.Duration(time.Duration(n) * time.Second) }
	want := map[string]*spec.Health{
		"cart":  {TCP: &spec.TCPCheck{Port: 9000}, Interval: sec(5), Timeout: sec(2), Retries: 3},
		"front": {HTTP: &spec.HTTPCheck{Port: 8080, Path: "/healthz"}, Interval: sec(10), Timeout: sec(1), Retries: 5, StartPeriod: sec(30)},
		"probe": {Exec: []string{"grpc_health_probe", "-addr=:50051"}, Interval: sec(10), Timeout: sec(2), Retries: 3},
		"plain": nil,
	}
	if !reflect.DeepEqual(got, want) {
		for k := range want {
			if !reflect.DeepEqual(got[k], want[k]) {
				t.Errorf("%s: health %+v, want %+v", k, got[k], want[k])
			}
		}
	}
	// The converted spec is one deploy -f reads, with the check as given.
	out, err := Marshal(res.Apps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "health:\n  tcp:\n    port: 9000\n") {
		t.Errorf("the converted cart has no tcp check:\n%s", out)
	}
}

func TestHivemindExtensionHealthRefuses(t *testing.T) {
	for _, c := range []struct{ name, file, path, msg string }{
		{"both", web("healthcheck: {test: [CMD, x]}\nx-hivemind: {health: {tcp: {port: 80}}}"), "services.web.x-hivemind.health",
			"an app has one health check: give healthcheck or x-hivemind.health, not both"},
		{"both, healthcheck disabled", web("healthcheck: {disable: true}\nx-hivemind: {health: {tcp: {port: 80}}}"), "services.web.x-hivemind.health",
			"give healthcheck or x-hivemind.health, not both"},
		{"not a mapping", web("x-hivemind: {health: tcp}"), "services.web.x-hivemind.health", "want a mapping, such as {tcp: {port: 50051}}"},
		{"unknown key", web("x-hivemind: {health: {tcp: {port: 80}, test: [CMD, x]}}"), "services.web.x-hivemind.health.test",
			"not supported: a health check has http, tcp, exec, interval, timeout, retries and start_period only"},
		{"http without a port", web("x-hivemind: {health: {http: {path: /}}}"), "services.web.x-hivemind.health.http.port", "required"},
		{"http not a mapping", web("x-hivemind: {health: {http: 80}}"), "services.web.x-hivemind.health.http", "want a mapping with port"},
		{"http unknown key", web("x-hivemind: {health: {http: {port: 80, scheme: https}}}"), "services.web.x-hivemind.health.http.scheme",
			"not supported: an http check has port and path only"},
		{"tcp unknown key", web("x-hivemind: {health: {tcp: {port: 80, host: db}}}"), "services.web.x-hivemind.health.tcp.host", "not supported: a tcp check has port only"},
		{"tcp port not a number", web("x-hivemind: {health: {tcp: {port: grpc}}}"), "services.web.x-hivemind.health.tcp.port", "want a whole number"},
		{"exec a string", web("x-hivemind: {health: {exec: grpc_health_probe}}"), "services.web.x-hivemind.health.exec", "want a list: the command and its arguments"},
		{"exec empty", web("x-hivemind: {health: {exec: []}}"), "services.web.x-hivemind.health.exec", "want a list"},
		{"interval without a unit", web("x-hivemind: {health: {tcp: {port: 80}, interval: 5}}"), "services.web.x-hivemind.health.interval", "want a duration"},
		{"two checks", web("x-hivemind: {health: {tcp: {port: 80}, http: {port: 80}}}"), "services.web", "health: set exactly one of http, tcp and exec"},
		{"no check", web("x-hivemind: {health: {interval: 5s}}"), "services.web", "health: set exactly one of http, tcp and exec"},
		{"port out of range", web("x-hivemind: {health: {tcp: {port: 70000}}}"), "services.web", "health.tcp.port: must be between 1 and 65535"},
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

// One run lists every problem: a route's and a health check's values that
// the app spec refuses are reported with the service's other problems, not
// after them. In the fourth acceptance run protocol "grpc" was reported
// only once the unknown key beside it was fixed.
func TestConvertListsExtensionProblemsWithTheOthers(t *testing.T) {
	file := web(`restart: on-failure
x-hivemind:
  routes:
    - {host: www.example.com, path: /shop.Cart, port: 50051, protocol: grpc, tls: true}
    - {host: www.example.com, path: /shop.Cart/, port: 50051}
  health: {tcp: {port: 0}, interval: 500ms, timeout: 200ms}`)
	_, err := Convert([]byte(file), Options{Lookup: env(nil)})
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("converted: %v", err)
	}
	want := []Problem{
		{"services.web", `routes[0]: protocol "grpc": use http or h2c`},
		{"services.web", `routes[1]: host "www.example.com" with path "/shop.Cart" listed twice`},
		{"services.web", "health.tcp.port: must be between 1 and 65535"},
		{"services.web", "health.interval: must be between 1s and 1h"},
		{"services.web.restart", "not supported: hivemind restarts every replica that exits"},
		{"services.web.x-hivemind.routes[0].tls", "not supported: a route has host, port, path and protocol only"},
	}
	for _, w := range want {
		found := false
		for _, p := range ce.Problems {
			found = found || p.Path == w.Path && strings.HasPrefix(p.Msg, w.Msg)
		}
		if !found {
			t.Errorf("no problem %q at %s in:\n%v", w.Msg, w.Path, err)
		}
	}
	if len(ce.Problems) != len(want) {
		t.Errorf("%d problems, want %d:\n%v", len(ce.Problems), len(want), err)
	}

	// A value that could not be read is not checked again: a route whose
	// port is not a number has one problem for it, and the routes are left
	// to the next run; so is a health check whose interval is not a
	// duration.
	file = web(`restart: on-failure
x-hivemind:
  routes: [{host: www.example.com, port: http, protocol: grpc}]
  health: {tcp: {port: 0}, interval: 5}`)
	_, err = Convert([]byte(file), Options{Lookup: env(nil)})
	if !errors.As(err, &ce) {
		t.Fatalf("converted: %v", err)
	}
	if len(ce.Problems) != 3 || strings.Contains(err.Error(), "protocol") || strings.Contains(err.Error(), "health.tcp.port") {
		t.Fatalf("problems:\n%v", err)
	}
	// The health check of a service that also has a healthcheck is not
	// used, so it is not checked either.
	file = web(`healthcheck: {test: [CMD, x]}
x-hivemind: {health: {tcp: {port: 0}}}`)
	_, err = Convert([]byte(file), Options{Lookup: env(nil)})
	if !errors.As(err, &ce) || len(ce.Problems) != 1 || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("problems:\n%v", err)
	}
}
