package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/store"
)

// The worker's epoch file: none is 0, a write replaces it whole, and a
// damaged one is an error that names it.
func TestWorkerEpochFile(t *testing.T) {
	dir := t.TempDir()
	if e, err := readWorkerEpoch(dir); err != nil || e != 0 {
		t.Fatalf("no file: %d, %v", e, err)
	}
	for _, e := range []int64{1, 12, 3} {
		if err := saveWorkerEpoch(dir, e); err != nil {
			t.Fatal(err)
		}
		if got, err := readWorkerEpoch(dir); err != nil || got != e {
			t.Fatalf("after saving %d: %d, %v", e, got, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, workerEpochFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", fi.Mode().Perm(), err)
	}
	if _, err := os.Stat(filepath.Join(dir, workerEpochNext)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staging file is left: %v", err)
	}
	// A staging file from a write that stopped before its rename is
	// replaced.
	if err := os.WriteFile(filepath.Join(dir, workerEpochNext), []byte("9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveWorkerEpoch(dir, 4); err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorkerEpoch(dir); got != 4 {
		t.Fatalf("after a left staging file: %d, want 4", got)
	}
	if err := os.WriteFile(filepath.Join(dir, workerEpochFile), []byte("four\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkerEpoch(dir); err == nil || !strings.Contains(err.Error(), workerEpochFile) {
		t.Fatalf("a damaged file: %v", err)
	}
}

// Two servers of one CA, one restored at epoch 1 and the old one at epoch
// 0. set-server takes the restored one and writes its epoch; then it refuses
// the old one, says why and what to do, and leaves every file of the worker
// as it was. A worker that has seen no epoch takes either.
func TestSetServerRefusesAServerFromBeforeARestore(t *testing.T) {
	ctx := context.Background()
	restored := newTestClusterAt(t, nil, "127.0.0.1", 1)
	old := newTestClusterAt(t, restored.ca, "127.0.0.1", 0)

	fresh := filepath.Join(t.TempDir(), "fresh")
	saveJoined(t, fresh, restored.ca, "node-w0")
	if err := runSetServer(ctx, io.Discard, fresh, old.cl.NodeAddr()); err != nil {
		t.Fatalf("a worker that has seen no epoch, to the server at epoch 0: %v", err)
	}
	if e, err := readWorkerEpoch(fresh); err != nil || e != 0 {
		t.Fatalf("epoch after the server at 0: %d, %v", e, err)
	}

	wdir := filepath.Join(t.TempDir(), "worker")
	saveJoined(t, wdir, restored.ca, "node-w1")
	if err := runSetServer(ctx, io.Discard, wdir, restored.cl.NodeAddr()); err != nil {
		t.Fatalf("to the restored server: %v", err)
	}
	if e, err := readWorkerEpoch(wdir); err != nil || e != 1 {
		t.Fatalf("epoch after the restored server: %d, %v; want 1", e, err)
	}
	before := workerFiles(t, wdir)
	var out bytes.Buffer
	err := runSetServer(ctx, &out, wdir, old.cl.NodeAddr())
	want := "the server at " + old.cl.NodeAddr() + " is from before a restore of this cluster: it is at epoch 0, and this worker has seen epoch 1"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "Stop the old server, and give set-server the address of the restored one") ||
		!strings.Contains(err.Error(), "node.json is unchanged") {
		t.Fatalf("to the old server: %v; want an error with %q", err, want)
	}
	if !sameFiles(before, workerFiles(t, wdir)) {
		t.Error("the refused set-server changed the worker's files")
	}
	// Back to the restored one: taken, as it is at the epoch seen.
	if err := runSetServer(ctx, io.Discard, wdir, restored.cl.NodeAddr()); err != nil {
		t.Fatalf("to the restored server again: %v", err)
	}
}

// startWorkerLogged is startWorker with the worker's log in log.
func startWorkerLogged(t *testing.T, dir string, rt *memRuntime, log *slog.Logger) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWorker(ctx, &config.Config{DataDir: dir, ReconcileInterval: 50 * time.Millisecond}, log, rt)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Error("the worker did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// containersOf lists rt's containers of app, as "ID state", sorted.
func containersOf(rt *memRuntime, app string) []string {
	cs, _ := rt.List(context.Background(), map[string]string{runtime.LabelApp: app})
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID+" "+c.State)
	}
	slices.Sort(out)
	return out
}

// waitLong is waitUntil with a longer deadline, for the agent's backoff.
func waitLong(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A running worker meets the server it was restored from, back at the
// address it dials, as when a recovered machine comes back up and a DNS
// record or an unmoved worker still leads to it. The worker refuses it: it
// logs why once, at error, keeps its containers as they are, and sends it
// nothing that changes its store, and the old server logs that a newer copy
// of the cluster exists. When the restored server is back at the address,
// the worker reports to it again, with the same containers.
func TestAWorkerRefusesTheServerFromBeforeARestore(t *testing.T) {
	old := staleServerRetry
	staleServerRetry = 200 * time.Millisecond
	t.Cleanup(func() { staleServerRetry = old })
	ctx := context.Background()
	home := isolate(t)
	dataA := filepath.Join(home, "old")
	if _, _, err := run(t, "", "init", "--data-dir", dataA); err != nil {
		t.Fatal(err)
	}
	port := unusedPort(t)
	env := map[string]string{"HIVEMIND_ADVERTISE": "127.0.0.1", "HIVEMIND_CLUSTER_PORT": strconv.Itoa(port)}
	a := testServe(t, dataA, env)

	var tok server.JoinTokenReply
	apiCall(t, a, dataA, http.MethodPost, "/v1/join-tokens", `{}`, &tok)
	wdir := filepath.Join(home, "worker")
	if err := enroll(ctx, slog.New(slog.DiscardHandler), wdir, "127.0.0.1", port, tok.Token); err != nil {
		t.Fatal(err)
	}
	id := certNodeID(wdir)
	rt := newMemRuntime()
	wlogs := &syncLog{}
	startWorkerLogged(t, wdir, rt, slog.New(slog.NewTextHandler(wlogs, nil)))
	// reportsTo: the worker has renewed its lease with n since since,
	// and n has it ready.
	reportsTo := func(n *servedNode, data string, since time.Time) func() bool {
		return func() bool {
			for _, r := range nodeRows(t, n, data) {
				if r.ID == id && r.State == "ready" && r.LastLease.After(since) {
					return true
				}
			}
			return false
		}
	}
	waitUntil(t, "the worker ready", reportsTo(a, dataA, time.Time{}))
	apiCall(t, a, dataA, http.MethodPost, "/v1/apps", `{"name":"web","image":"nginx:alpine","replicas":4}`, nil)
	waitUntil(t, "a replica on the worker", func() bool { return rt.running("web") >= 1 })

	// The server's machine is lost, and its backup restored at the same
	// address, at epoch 1. The worker, never stopped, takes the epoch.
	file := filepath.Join(home, "hm.tar")
	if _, _, err := run(t, "", "backup", file, "--data-dir", dataA); err != nil {
		t.Fatal(err)
	}
	if err := a.stop(); err != nil {
		t.Fatal(err)
	}
	dataB := filepath.Join(home, "new")
	if out, _, err := run(t, "", "restore", file, "--data-dir", dataB); err != nil || !strings.Contains(out, "The cluster is at epoch 1 now") {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	started := time.Now()
	b := testServe(t, dataB, env)
	waitLong(t, "the worker reporting to the restored server", reportsTo(b, dataB, started))
	if e, err := readWorkerEpoch(wdir); err != nil || e != 1 {
		t.Fatalf("the worker's epoch file: %d, %v; want 1", e, err)
	}
	if !strings.Contains(b.logs.String(), "epoch=1") {
		t.Errorf("the restored server did not log its epoch:\n%s", b.logs.String())
	}
	kept := containersOf(rt, "web")

	// The old machine comes back at the address, with its old store.
	if err := b.stop(); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	a2 := testServe(t, dataA, env)
	const refusal = `level=ERROR msg="the server at 127.0.0.1:` // the client's line
	waitLong(t, "the worker refusing the old server", func() bool { return strings.Contains(wlogs.String(), refusal) })
	waitUntil(t, "the old server's warning", func() bool {
		return strings.Contains(a2.logs.String(), "a newer copy of this cluster exists: this server was replaced by a restore")
	})
	time.Sleep(2 * time.Second) // 40 passes of the worker's agent
	if got := containersOf(rt, "web"); !slices.Equal(got, kept) {
		t.Errorf("the worker's containers changed while the old server ran: %v, then %v", kept, got)
	}
	l := wlogs.String()
	if got := strings.Count(l, refusal); got != 1 {
		t.Errorf("the worker logged the refusal %d times, want once:\n%s", got, l)
	}
	if !strings.Contains(l, "is from before a restore of this cluster") || !strings.Contains(l, "server_epoch=0 seen_epoch=1") ||
		!strings.Contains(l, "Stop the old server; if this worker should dial another address, stop the worker and move it with hivemind node set-server <address>") {
		t.Errorf("the worker's refusal does not say what to do:\n%s", l)
	}
	if reportsTo(a2, dataA, started)() {
		t.Error("the old server took a lease or a report from the worker")
	}
	if strings.Count(a2.logs.String(), "a newer copy of this cluster exists") != 1 {
		t.Errorf("the old server logged its warning more than once:\n%s", a2.logs.String())
	}

	// The restored server is back at the address.
	if err := a2.stop(); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	b2 := testServe(t, dataB, env)
	waitLong(t, "the worker reporting to the restored server again", reportsTo(b2, dataB, started))
	if !strings.Contains(wlogs.String(), "answers at this cluster's epoch now; this worker talks to it again") {
		t.Errorf("the worker did not log that it talks to the restored server again:\n%s", wlogs.String())
	}
	if got := containersOf(rt, "web"); !slices.Equal(got, kept) {
		t.Errorf("the worker's containers changed: %v, then %v", kept, got)
	}
	if e, _ := readWorkerEpoch(wdir); e != 1 {
		t.Errorf("the worker's epoch is %d, want 1", e)
	}
}

// A backup of a store from before epochs, schema version 15, restores as it
// is, so that release can still serve it, and the epoch waits in
// restored-epoch. serve migrates the store, gives the cluster that epoch,
// and removes the file.
func TestServeGivesAStoreRestoredFromBeforeEpochsItsEpoch(t *testing.T) {
	home := isolate(t)
	data := filepath.Join(home, "old")
	if _, _, err := run(t, "", "init", "--data-dir", data); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(data, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE servers`, `DROP TABLE raft_applied`, `ALTER TABLE join_tokens DROP COLUMN role`,
		`DROP TABLE ca_rotation_nodes`, `DROP TABLE ca_rotation`,
		`ALTER TABLE cluster DROP COLUMN ca_fp`, `ALTER TABLE cluster DROP COLUMN epoch`, `ALTER TABLE nodes DROP COLUMN dials`, `UPDATE schema_version SET version = 15`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	file := filepath.Join(home, "hm.tar")
	if _, _, err := run(t, "", "backup", file, "--data-dir", data); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(home, "new")
	out, _, err := run(t, "", "restore", file, "--data-dir", restored)
	if err != nil || !strings.Contains(out, "The store stays at schema version 15, from before epochs") || !strings.Contains(out, "puts the cluster at epoch 1") {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(restored, "restored-epoch")); err != nil || string(b) != "1\n" {
		t.Fatalf("restored-epoch: %q, %v", b, err)
	}
	s := testServe(t, restored, map[string]string{"HIVEMIND_ADVERTISE": "127.0.0.1", "HIVEMIND_CLUSTER_PORT": strconv.Itoa(unusedPort(t))})
	if l := s.logs.String(); !strings.Contains(l, "the restored store has its epoch now") || !strings.Contains(l, "cluster listening") ||
		!strings.Contains(l, "epoch=1") {
		t.Errorf("serve on the restored store:\n%s", l)
	}
	if _, err := os.Stat(filepath.Join(restored, "restored-epoch")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("restored-epoch after serve: %v", err)
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(restored, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if cl, err := st.GetCluster(context.Background()); err != nil || cl.Epoch != 1 {
		t.Fatalf("the restored cluster: %+v, %v", cl, err)
	}
}
