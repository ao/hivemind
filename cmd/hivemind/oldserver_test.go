package main

import (
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
