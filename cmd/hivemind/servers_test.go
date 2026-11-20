package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/ha"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/store"
)

// TestAPIDropsForwardingHeaders: an API client that sends the forwarding
// headers, claiming another address or TLS, is served as itself: the
// handlers never see them, and its address is its own.
func TestAPIDropsForwardingHeaders(t *testing.T) {
	var seen http.Header
	var remote string
	var viaTLS bool
	h := (&haServe{}).apiHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, remote, viaTLS = r.Header.Clone(), r.RemoteAddr, r.TLS != nil
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/session", nil)
	r.RemoteAddr = "10.9.8.7:5555"
	r.Header.Set(ha.HeaderRemote, "127.0.0.1:1")
	r.Header.Set(ha.HeaderTLS, "1")
	r.Header.Set(ha.HeaderKind, ha.KindAPI)
	r.Header.Set("hivemind-forward-chain", "x")
	h.ServeHTTP(httptest.NewRecorder(), r)
	for k := range seen {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "Hivemind-Forward-") {
			t.Errorf("the handler saw %s", k)
		}
	}
	if remote != "10.9.8.7:5555" || viaTLS {
		t.Fatalf("the handler saw the request from %s, TLS %v", remote, viaTLS)
	}
}

// TestLeaderWork: the leader's work starts once, stops, and starts again.
func TestLeaderWork(t *testing.T) {
	var runs, live atomic.Int32
	lw := &leaderWork{run: func(ctx context.Context, _ time.Time) {
		runs.Add(1)
		live.Add(1)
		<-ctx.Done()
		live.Add(-1)
	}}
	if !lw.start(time.Unix(1, 0)) || lw.start(time.Unix(2, 0)) {
		t.Fatal("start ran twice, or not at all")
	}
	for live.Load() != 1 {
		time.Sleep(time.Millisecond)
	}
	if !lw.since().Equal(time.Unix(1, 0)) || !lw.running() {
		t.Fatalf("since %v, running %v", lw.since(), lw.running())
	}
	lw.stop()
	if live.Load() != 0 || lw.running() {
		t.Fatal("stop did not end the work")
	}
	lw.stop()
	if !lw.start(time.Unix(3, 0)) {
		t.Fatal("the work did not start again")
	}
	lw.stop()
	if runs.Load() != 2 {
		t.Fatalf("%d runs", runs.Load())
	}
}

// TestServerStateFile: server.json is written once, mode 0600, and read
// back; a directory with it refuses init and another server join.
func TestServerStateFile(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	if _, err := readServerState(dir); err != errNotJoinedServer {
		t.Fatalf("no server.json: %v", err)
	}
	if err := checkEmptyForServer(dir); err != nil {
		t.Fatalf("an empty directory: %v", err)
	}
	st := serverState{NodeID: "node-abc", Joined: time.Unix(1_700_000_000, 0).UTC(),
		Servers: []nodeapi.ServerInfo{{ID: "node-abc", Advertise: "10.0.0.6", NodePort: 4484, RaftAddr: "10.0.0.6:4486"},
			{ID: "node-s0", Advertise: "10.0.0.5", NodePort: 4484, RaftAddr: "10.0.0.5:4486"}}}
	if err := writeServerState(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := writeServerState(dir, st); err == nil {
		t.Fatal("server.json was written over")
	}
	fi, err := os.Stat(filepath.Join(dir, serverStateFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("server.json: %v %v", fi.Mode(), err)
	}
	got, err := readServerState(dir)
	if err != nil || got.NodeID != "node-abc" || len(got.Servers) != 2 {
		t.Fatalf("read back: %+v %v", got, err)
	}
	if seeds := seedIDs(got); len(seeds) != 1 || seeds[0] != "node-s0" {
		t.Fatalf("seeds %v", seeds)
	}
	if err := checkEmptyForServer(dir); err == nil || !strings.Contains(err.Error(), "holds server node-abc") {
		t.Fatalf("a joined server's directory: %v", err)
	}
	if _, _, err := run(t, "", "init", "--data-dir", dir); err == nil || !strings.Contains(err.Error(), "holds server node-abc") {
		t.Fatalf("init on a joined server's directory: %v", err)
	}
}

// TestJoinServerNeedsItsAddress: join --role server refuses to run without
// a token or --advertise, before it dials anything.
func TestJoinServerNeedsItsAddress(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "s")
	if _, _, err := run(t, "", "join", "10.255.255.1", "--role", "server", "--token", "x", "--data-dir", dir); err == nil ||
		!strings.Contains(err.Error(), "--advertise is required") {
		t.Fatalf("join --role server without --advertise: %v", err)
	}
	if _, _, err := run(t, "", "join", "10.255.255.1", "--role", "server", "--advertise", "10.255.255.2", "--data-dir", dir); err == nil ||
		!strings.Contains(err.Error(), "--token is required") {
		t.Fatalf("join --role server without --token: %v", err)
	}
	if _, _, err := run(t, "", "join", "10.255.255.1", "--role", "boss", "--token", "x", "--data-dir", dir); err == nil ||
		!strings.Contains(err.Error(), "must be worker or server") {
		t.Fatalf("join --role boss: %v", err)
	}
	if _, _, err := run(t, "", "join", "10.255.255.1", "--token", "x", "--data-dir", dir, "--cluster-port", "4490"); err == nil ||
		!strings.Contains(err.Error(), "--cluster-port does not apply to join") {
		t.Fatalf("a worker's join with --cluster-port: %v", err)
	}
}

// TestCLITakesAListOfServers: with several servers given, the CLI passes
// one that does not answer and one that answers that no server leads, and
// prints the list of the one that answers.
func TestCLITakesAListOfServers(t *testing.T) {
	isolate(t)
	noLeader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(nodeapi.NoLeaderHeader, "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no leader: this server reaches 1 of the 3 servers"}`))
	}))
	defer noLeader.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/servers" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":"unexpected"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"leader":"node-b","answered_by":"node-b","servers":[{"id":"node-a","advertise":"10.0.0.5","node_port":4484,"role":"follower"},` +
			`{"id":"node-b","advertise":"10.0.0.6","node_port":4484,"role":"leader","last_contact":"2026-11-20T00:00:00Z"}]}`))
	}))
	defer good.Close()
	list := "http://127.0.0.1:1," + noLeader.URL + "," + good.URL
	out, _, err := run(t, "", "servers", "--server", list, "--token", "tok")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"node-a", "10.0.0.5:4484", "follower", "node-b", "leader"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	// One server that says no leader is the answer.
	if _, _, err := run(t, "", "servers", "--server", noLeader.URL, "--token", "tok"); err == nil || !strings.Contains(err.Error(), "no leader") {
		t.Fatalf("with no leader: %v", err)
	}
}

// TestServeRefusesAServerWithoutItsRaftState: a store that lists the
// servers of a cluster of several, in a data directory without Raft
// state, is refused, rather than served as a second cluster.
func TestServeRefusesAServerWithoutItsRaftState(t *testing.T) {
	isolate(t)
	data := filepath.Join(t.TempDir(), "data")
	if _, _, err := run(t, "", "init", "--data-dir", data); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(data, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutServer(ctx, store.Server{ID: "node-s2", Advertise: "10.0.0.6", NodePort: 4484, RaftAddr: "10.0.0.6:4486"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	env := map[string]string{"HIVEMIND_DATA_DIR": data, "HIVEMIND_API_ADDR": "127.0.0.1:" + strconv.Itoa(unusedPort(t))}
	cfg, err := config.Load(nil, config.Options{Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	if err != nil {
		t.Fatal(err)
	}
	log, err := newLogger(cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	err = runServe(ctx, cfg, log, newMemRuntime())
	if err == nil || !strings.Contains(err.Error(), "has no Raft state") || !strings.Contains(err.Error(), "hivemind restore") {
		t.Fatalf("serve of a server's store without its Raft state: %v", err)
	}
}

// TestRemovalWatch: a server is taken as removed once the server table
// lacks it, after it has seen its row there, or from the start of a serve
// that found Raft state; not while it applies the entries that add the
// servers after it joined.
func TestRemovalWatch(t *testing.T) {
	a := store.Server{ID: "node-a"}
	me := store.Server{ID: "node-me"}
	var w removalWatch
	for _, step := range []struct {
		servers []store.Server
		removed bool
	}{
		{nil, false},
		{[]store.Server{a}, false},
		{[]store.Server{a, me}, false},
		{[]store.Server{a, me}, false},
		{[]store.Server{a}, true},
	} {
		if got := w.removed(step.servers, me.ID); got != step.removed {
			t.Fatalf("with %v: removed %v, want %v", step.servers, got, step.removed)
		}
	}
	restarted := removalWatch{seen: true}
	if !restarted.removed([]store.Server{a}, me.ID) {
		t.Fatal("a server restarted with Raft state, missing from the table, is not taken as removed")
	}
	if restarted.removed(nil, me.ID) {
		t.Fatal("an empty table is taken as a removal")
	}
}
