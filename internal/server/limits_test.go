package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// limitCase is one limit of the app spec: at sets a valid base spec to the
// limit, past to one over it. past must be refused with code, and a message
// that contains word.
type limitCase struct {
	name string
	at   func(*spec.App)
	past func(*spec.App)
	code int
	word string
}

func repeat(n int) string { return strings.Repeat("x", n) }

func limitCases() []limitCase {
	args := func(n, size int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = repeat(size)
		}
		return out
	}
	env := func(n, valueLen int) map[string]string {
		out := map[string]string{}
		for i := range n {
			out[fmt.Sprintf("E%03d", i)] = repeat(valueLen)
		}
		return out
	}
	ports := func(n int) []spec.Port {
		var out []spec.Port
		for i := range n {
			out = append(out, spec.Port{Host: 20000 + i, Container: 80})
		}
		return out
	}
	volumes := func(n int) []spec.Volume {
		var out []spec.Volume
		for i := range n {
			out = append(out, spec.Volume{Name: fmt.Sprintf("v%d", i), Path: fmt.Sprintf("/data/v%d", i)})
		}
		return out
	}
	routes := func(n int) []spec.Route {
		var out []spec.Route
		for i := range n {
			out = append(out, spec.Route{Host: fmt.Sprintf("r%d.example.com", i), Port: 8080})
		}
		return out
	}
	// A host name of n characters: labels of 63, 63, 63 and the rest, the
	// last one a word.
	host := func(n int) string {
		h := repeat(63) + "." + repeat(63) + "." + repeat(63) + "."
		return h + strings.Repeat("y", n-len(h))
	}
	image := func(n int) string { // r.example/<path>:1, n characters
		return "r.example/" + strings.Repeat("a", n-len("r.example/:1")) + ":1"
	}
	const envKeyLen = 4 // E000
	return []limitCase{
		{"name", func(a *spec.App) { a.Name = strings.Repeat("a", 32) }, func(a *spec.App) { a.Name = strings.Repeat("a", 33) }, 400, "name"},
		{"image", func(a *spec.App) { a.Image = image(255) }, func(a *spec.App) { a.Image = image(256) }, 400, "image"},
		{"replicas", func(a *spec.App) { a.Replicas = 100 }, func(a *spec.App) { a.Replicas = 101 }, 400, "replicas"},
		{"command args", func(a *spec.App) { a.Command = args(64, 1) }, func(a *spec.App) { a.Command = args(65, 1) }, 400, "command"},
		{"command arg length", func(a *spec.App) { a.Command = args(1, 4096) }, func(a *spec.App) { a.Command = args(1, 4097) }, 400, "command[0]"},
		{"env count", func(a *spec.App) { a.Env = env(128, 1) }, func(a *spec.App) { a.Env = env(129, 1) }, 400, "env"},
		{"env bytes", func(a *spec.App) { a.Env = env(128, 512-envKeyLen) }, func(a *spec.App) {
			a.Env = env(128, 512-envKeyLen)
			a.Env["E000"] += "x"
		}, 400, "env: at most 65536 bytes"},
		{"ports", func(a *spec.App) { a.Ports = ports(32) }, func(a *spec.App) { a.Ports = ports(33) }, 400, "ports"},
		{"volumes", func(a *spec.App) { a.Volumes = volumes(16) }, func(a *spec.App) { a.Volumes = volumes(17) }, 400, "volumes"},
		{"cpu", func(a *spec.App) { a.Resources.CPU = 1024 }, func(a *spec.App) { a.Resources.CPU = 1024.5 }, 400, "resources.cpu"},
		{"memory", func(a *spec.App) { a.Resources.Memory = 1 << 40 }, func(a *spec.App) { a.Resources.Memory = 1<<40 + 1 }, 400, "resources.memory"},
		{"health path", func(a *spec.App) { a.Health = &spec.Health{HTTP: &spec.HTTPCheck{Port: 80, Path: "/" + repeat(1023)}} },
			func(a *spec.App) { a.Health = &spec.Health{HTTP: &spec.HTTPCheck{Port: 80, Path: "/" + repeat(1024)}} }, 400, "health.http.path"},
		{"health exec args", func(a *spec.App) { a.Health = &spec.Health{Exec: args(64, 1)} },
			func(a *spec.App) { a.Health = &spec.Health{Exec: args(65, 1)} }, 400, "health.exec"},
		{"health exec arg length", func(a *spec.App) { a.Health = &spec.Health{Exec: args(1, 4096)} },
			func(a *spec.App) { a.Health = &spec.Health{Exec: args(1, 4097)} }, 400, "health.exec"},
		{"routes", func(a *spec.App) { a.Routes = routes(16) }, func(a *spec.App) { a.Routes = routes(17) }, 400, "routes"},
		{"route host", func(a *spec.App) { a.Routes = []spec.Route{{Host: host(253), Port: 80}} },
			func(a *spec.App) { a.Routes = []spec.Route{{Host: host(254), Port: 80}} }, 400, "longer than 253"},
	}
}

// Every limit of the app spec: a spec at the limit is accepted, one past it
// is refused with 400 and a message that names the field.
func TestSpecLimitsAtAndPast(t *testing.T) {
	h := newHarness(t)
	for i, c := range limitCases() {
		t.Run(c.name, func(t *testing.T) {
			at := spec.App{Name: fmt.Sprintf("lim%d", i), Image: "busybox"}
			c.at(&at)
			body, _ := json.Marshal(at)
			rec := h.do(t, "PUT", "/v1/apps/"+at.Name, string(body))
			if rec.Code/100 != 2 {
				t.Fatalf("at the limit: %d %s", rec.Code, rec.Body.String())
			}
			past := spec.App{Name: fmt.Sprintf("lim%d", i), Image: "busybox"}
			c.past(&past)
			body, _ = json.Marshal(past)
			rec = h.do(t, "POST", "/v1/apps", string(body))
			if rec.Code != c.code {
				t.Fatalf("one past the limit: %d %s, want %d", rec.Code, rec.Body.String(), c.code)
			}
			if msg := errorOf(t, rec); !strings.Contains(msg, c.word) {
				t.Fatalf("one past the limit: message %q does not name %q", msg, c.word)
			}
		})
	}
}

// The request body: 1 MiB is read, one byte more is refused with 413, on
// every route that takes a body.
func TestBodyLimitOnEveryRoute(t *testing.T) {
	h := newHarness(t)
	h.do(t, "PUT", "/v1/apps/web", `{"image":"busybox"}`)
	// A JSON string that pads a valid body to exactly n bytes.
	pad := func(prefix, suffix string, n int) string {
		return prefix + strings.Repeat(" ", n-len(prefix)-len(suffix)) + suffix
	}
	routes := []struct{ method, path, prefix, suffix string }{
		{"POST", "/v1/apps", `{"name":"big","image":"busybox"`, `}`},
		{"PUT", "/v1/apps/big", `{"image":"busybox"`, `}`},
		{"POST", "/v1/apps/web/scale", `{"replicas":1`, `}`},
		{"PUT", "/v1/registries/r.example.com", `{"username":"u","password":"p"`, `}`},
		{"POST", "/v1/nodes/" + testNode + "/eligibility", `{"eligible":true`, `}`},
		{"POST", "/v1/nodes/" + testNode + "/drain", `{"deadline":"1m"`, `}`},
	}
	for _, r := range routes {
		rec := h.do(t, r.method, r.path, pad(r.prefix, r.suffix, MaxBodyBytes))
		if rec.Code == http.StatusRequestEntityTooLarge || rec.Code >= 500 {
			t.Errorf("%s %s with a body of exactly %d bytes: %d %s", r.method, r.path, MaxBodyBytes, rec.Code, rec.Body.String())
		}
		if r.path == "/v1/nodes/"+testNode+"/drain" {
			h.do(t, "POST", "/v1/nodes/"+testNode+"/eligibility", `{"eligible":true}`)
		}
		rec = h.do(t, r.method, r.path, pad(r.prefix, r.suffix, MaxBodyBytes+1))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s %s with a body of %d bytes: %d, want 413", r.method, r.path, MaxBodyBytes+1, rec.Code)
			continue
		}
		if msg := errorOf(t, rec); !strings.Contains(msg, "larger than") {
			t.Errorf("%s %s: 413 message %q", r.method, r.path, msg)
		}
	}
}

// Query values and headers are bounded too.
func TestQueryAndHeaderLimits(t *testing.T) {
	h := newHarness(t)
	h.do(t, "PUT", "/v1/apps/web", `{"image":"busybox"}`)
	for _, q := range []string{"tail=100001", "tail=-1", "replica=100", "replica=-1", "tail=" + repeat(30)} {
		rec := h.do(t, "GET", "/v1/apps/web/logs?"+q, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("logs?%s: %d, want 400", q, rec.Code)
		}
	}
	ht := newHarness(t, func(c *Config) {
		c.IssueJoinToken = func(ttl time.Duration) (string, time.Time, error) { return "token", time.Now().Add(ttl), nil }
	})
	if rec := ht.do(t, "POST", "/v1/join-tokens", `{"ttl":"168h"}`); rec.Code != http.StatusCreated {
		t.Errorf("join token ttl of 7 days: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ht.do(t, "POST", "/v1/join-tokens", `{"ttl":"168h1s"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("join token ttl past 7 days: %d, want 400", rec.Code)
	}

	// The header limit is the http.Server's: a request whose headers are
	// over MaxHeaderBytes is refused with 431 before any handler runs.
	ts := httptest.NewUnstartedServer(h.srv.Handler())
	ts.Config = NewHTTPServer(h.srv.Handler(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts.Start()
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Pad", repeat(MaxHeaderBytes+8192))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("headers over %d bytes: %d, want 431", MaxHeaderBytes, resp.StatusCode)
	}
}
