package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOldServer answers the API as a 2.0.0 server does: GET /v1/nodes
// without the fields added since, and {"error":"not found"} for a route it
// does not have. With version set, it sends that version, as a newer server
// that lacks a route would.
func fakeOldServer(t *testing.T, version string) {
	t.Helper()
	isolate(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if version != "" {
			w.Header().Set("Hivemind-Version", version)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/status" {
			// 2.0.0 sends no ready count.
			_, _ = w.Write([]byte(`[{"name":"web","image":"nginx:alpine","generation":1,"desired":2,"running":2,
				"replicas":[{"index":0,"node_id":"node-acc3d9097247","generation":1,"desired":"running","state":"running","restarts":0,"ready":true}]}]`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/nodes" {
			_, _ = w.Write([]byte(`[{"id":"node-acc3d9097247","name":"srv","cpu":2,"memory":4000000000,
				"last_lease":"2026-11-16T00:00:00Z","lease_age_seconds":1,"state":"ready","server":true,"replicas":0}]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	t.Cleanup(ts.Close)
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, "hm_test")
}

// The CLI against a server older than itself shows what it does not know as
// unknown, and names the server's age for a command the server lacks.
func TestCLIAgainstAnOlderServer(t *testing.T) {
	fakeOldServer(t, "")
	out, _, err := run(t, "", "nodes")
	if err != nil {
		t.Fatal(err)
	}
	rows := tableRows(t, out)
	if len(rows) != 1 || rows[0]["ELIGIBILITY"] != "-" || rows[0]["VERSION"] != "-" || rows[0]["ADDRESS"] != "-" || rows[0]["STATE"] != "ready" {
		t.Fatalf("nodes from a 2.0.0 server:\n%s", out)
	}
	out, _, err = run(t, "", "status")
	if err != nil {
		t.Fatal(err)
	}
	if apps := tableRows(t, out); len(apps) < 1 || apps[0]["APP"] != "web" || apps[0]["RUNNING"] != "2" || apps[0]["READY"] != "-" {
		t.Fatalf("status from a 2.0.0 server, which sends no ready count: READY must be -, not 0:\n%s", out)
	}
	out, _, err = run(t, "", "nodes", "--json")
	if err != nil || !strings.Contains(out, `"eligible": null`) {
		t.Fatalf("nodes --json from a 2.0.0 server: %q, %v", out, err)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"node", "drain", "node-acc3d9097247"}, "the server is older than 2.2 and has no node drain; upgrade the server"},
		{[]string{"node", "eligibility", "node-acc3d9097247", "--disable"}, "the server is older than 2.2 and has no node eligibility; upgrade the server"},
		{[]string{"registry", "list"}, "the server is older than 2.2 and has no registry list; upgrade the server"},
		{[]string{"registry", "logout", "ghcr.io"}, "the server is older than 2.2 and has no registry logout; upgrade the server"},
		{[]string{"backup", "--api", filepath.Join(t.TempDir(), "b.tar")}, "the server is older than 2.1 and has no backup through the API; upgrade the server"},
	} {
		if _, _, err := run(t, "", c.args...); err == nil || err.Error() != c.want {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}

	fakeOldServer(t, "v2.9.0")
	if _, _, err := run(t, "", "node", "drain", "node-acc3d9097247"); err == nil ||
		err.Error() != "the server runs v2.9.0, which has no node drain; upgrade the server" {
		t.Errorf("a server with a version but no drain: %v", err)
	}
}

// join-token --role server against a server before 2.3.0, which refuses
// the request's server field as an unknown field, says that the server is
// too old to have other servers, not "invalid JSON" (the fifth acceptance
// run's ninth finding). A worker's token from it is unchanged, and so is
// another 400.
func TestServerTokenFromAnOlderServer(t *testing.T) {
	for _, c := range []struct {
		version, want string
	}{
		{"v2.2.0", "the server runs v2.2.0, which is older than 2.3.0 and cannot have other servers; upgrade it first (docs/upgrading.md)"},
		{"", "the server is older than 2.3.0 and cannot have other servers; upgrade it first (docs/upgrading.md)"},
	} {
		isolate(t)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.version != "" {
				w.Header().Set("Hivemind-Version", c.version)
			}
			w.Header().Set("Content-Type", "application/json")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch _, server := body["server"]; {
			case r.URL.Path != "/v1/join-tokens":
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
			case server:
				// 2.2.0's strict decoder.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid JSON: json: unknown field \"server\""}`))
			case body["ttl"] == "bad":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"ttl must be a duration such as 1h"}`))
			default:
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"token":"hivemind-join-v1.x.y","expires_at":"2026-11-24T00:00:00Z"}`))
			}
		}))
		t.Setenv(envServer, ts.URL)
		t.Setenv(envToken, "hm_test")
		if _, _, err := run(t, "", "join-token", "--role", "server"); err == nil || err.Error() != c.want {
			t.Errorf("server %q: join-token --role server: %v, want %q", c.version, err, c.want)
		}
		if out, _, err := run(t, "", "join-token"); err != nil || strings.TrimSpace(out) != "hivemind-join-v1.x.y" {
			t.Errorf("server %q: a worker's token: %q, %v", c.version, out, err)
		}
		if _, _, err := run(t, "", "join-token", "--ttl", "bad"); err == nil || err.Error() != "ttl must be a duration such as 1h" {
			t.Errorf("server %q: another 400: %v", c.version, err)
		}
		ts.Close()
	}
}
