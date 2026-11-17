package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/store"
)

// servedNode is a serve that runs in this process, from testServe.
type servedNode struct {
	api  string
	logs *syncLog
	// stop ends serve and returns what it returned.
	stop func() error
}

// testServe runs serve on the data directory data in this process, with the
// settings in env over an API on a free loopback port and a fast reconcile,
// until stop is called or the test ends. It returns once the API answers.
func testServe(t *testing.T, data string, env map[string]string) *servedNode {
	t.Helper()
	apiPort := unusedPort(t)
	all := map[string]string{
		"HIVEMIND_DATA_DIR":           data,
		"HIVEMIND_API_ADDR":           "127.0.0.1:" + strconv.Itoa(apiPort),
		"HIVEMIND_RECONCILE_INTERVAL": "50ms",
		"HIVEMIND_MIN_READY":          "0s",
	}
	for k, v := range env {
		all[k] = v
	}
	cfg, err := config.Load(nil, config.Options{Lookup: func(k string) (string, bool) { v, ok := all[k]; return v, ok }})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncLog{}
	log, err := newLogger(cfg, logs)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, log, newMemRuntime()) }()
	var stopped bool
	var stopErr error
	stop := func() error {
		if stopped {
			return stopErr
		}
		stopped = true
		cancel()
		select {
		case stopErr = <-done:
		case <-time.After(20 * time.Second):
			t.Error("serve did not stop")
		}
		return stopErr
	}
	t.Cleanup(func() { _ = stop() })
	n := &servedNode{api: "http://127.0.0.1:" + strconv.Itoa(apiPort), logs: logs, stop: stop}
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(n.api + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return n
			}
		}
		select {
		case err := <-done:
			stopped, stopErr = true, err
			t.Fatalf("serve stopped before its API answered: %v\n%s", err, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve's API did not answer:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// nodeRows reads GET /v1/nodes with the admin token in data.
func nodeRows(t *testing.T, n *servedNode, data string) []nodeRow {
	t.Helper()
	token, err := auth.ReadAdminToken(data)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, n.api+"/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rows []nodeRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/nodes: %d, %v", resp.StatusCode, err)
	}
	return rows
}

// serverAddress is the address of the server's node in rows.
func serverAddress(rows []nodeRow) string {
	for _, r := range rows {
		if r.Server {
			return r.Address
		}
	}
	return ""
}

// joinPortCert is the certificate the join listener at addr presents, which
// is the server's own, checked against ca for name.
func joinPortCert(addr, name string, ca *pki.CA) (*x509.Certificate, error) {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr,
		&tls.Config{RootCAs: pool, ServerName: name, MinVersion: tls.VersionTLS13})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0], nil
}

// serve --advertise with a DNS name that does not resolve, and --cluster-addr:
// the cluster listeners bind --cluster-addr, the server's certificate names
// the name and that address, the server's node has that address, and serve
// says that workers reach it once the name resolves there.
func TestServeAdvertisesANameAndBindsTheClusterAddress(t *testing.T) {
	home := isolate(t)
	data := filepath.Join(home, "data")
	if _, _, err := run(t, "", "init", "--data-dir", data); err != nil {
		t.Fatal(err)
	}
	port := unusedPort(t)
	n := testServe(t, data, map[string]string{
		"HIVEMIND_ADVERTISE":    "hivemind.invalid",
		"HIVEMIND_CLUSTER_ADDR": "127.0.0.1",
		"HIVEMIND_CLUSTER_PORT": strconv.Itoa(port),
	})
	ca, err := pki.LoadCA(data)
	if err != nil {
		t.Fatal(err)
	}
	join := net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1))
	for _, name := range []string{"hivemind.invalid", "127.0.0.1"} {
		if _, err := joinPortCert(join, name, ca); err != nil {
			t.Errorf("the server's certificate, checked for %s: %v", name, err)
		}
	}
	if _, err := joinPortCert(join, "10.0.0.5", ca); err == nil {
		t.Error("the server's certificate names an address it was not given")
	}
	if got := serverAddress(nodeRows(t, n, data)); got != "127.0.0.1" {
		t.Errorf("the server's node has address %q, want 127.0.0.1", got)
	}
	if l := n.logs.String(); !strings.Contains(l, "--advertise does not resolve") || !strings.Contains(l, "advertise=hivemind.invalid") {
		t.Errorf("serve did not say the name does not resolve:\n%s", l)
	}
	if err := n.stop(); err != nil {
		t.Fatal(err)
	}

	// Without --cluster-addr, the name must resolve to this machine.
	err = runServeOnce(t, data, map[string]string{"HIVEMIND_ADVERTISE": "hivemind.invalid", "HIVEMIND_CLUSTER_PORT": strconv.Itoa(port)})
	if err == nil || !strings.Contains(err.Error(), "give this machine's address with --cluster-addr") {
		t.Fatalf("serve --advertise with a name that does not resolve: %v", err)
	}
}

// runServeOnce runs serve on data with env, and returns its error. It is for
// a serve that fails at the start; one that runs is stopped after 10 s.
func runServeOnce(t *testing.T, data string, env map[string]string) error {
	t.Helper()
	all := map[string]string{"HIVEMIND_DATA_DIR": data, "HIVEMIND_API_ADDR": "127.0.0.1:" + strconv.Itoa(unusedPort(t))}
	for k, v := range env {
		all[k] = v
	}
	cfg, err := config.Load(nil, config.Options{Lookup: func(k string) (string, bool) { v, ok := all[k]; return v, ok }})
	if err != nil {
		return err
	}
	log, err := newLogger(cfg, &syncLog{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return runServe(ctx, cfg, log, newMemRuntime())
}

// apiCall makes one admin call to n's API, with the token in data, and
// decodes the reply into out when it is not nil.
func apiCall(t *testing.T, n *servedNode, data, method, path, body string, out any) {
	t.Helper()
	token, err := auth.ReadAdminToken(data)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(method, n.api+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

// startWorker runs the worker in dir in this process until stop or the end
// of the test.
func startWorker(t *testing.T, dir string, rt *memRuntime) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWorker(ctx, &config.Config{DataDir: dir, ReconcileInterval: 50 * time.Millisecond},
			slog.New(slog.NewTextHandler(io.Discard, nil)), rt)
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

// storeValuesWith lists the rows of the store at path that hold s in any
// column, as "table: row".
func storeValuesWith(t *testing.T, path, s string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	var hits []string
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				text := fmt.Sprint(v)
				if b, ok := v.([]byte); ok {
					text = string(b)
				}
				if strings.Contains(text, s) {
					hits = append(hits, fmt.Sprintf("%s: %v", table, vals))
					break
				}
			}
		}
		rows.Close()
	}
	return hits
}

// A server restored on a machine with a new IP address, advertising that
// address: its certificate names the new address and not the old one, its
// node has the new address, it warns that the workers that joined the old
// address need set-server, and once the worker is moved it runs against the
// restored server with its own node ID and certificate. Nothing in the
// restored store names the old address then. 127.0.0.2 and 127.0.0.3 stand
// for the two machines' addresses; Linux routes all of 127/8 to loopback.
func TestRestoreAtANewAddress(t *testing.T) {
	for _, a := range []string{"127.0.0.2", "127.0.0.3"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(a, "0"))
		if err != nil {
			t.Skipf("this machine cannot bind %s: %v", a, err)
		}
		ln.Close()
	}
	ctx := context.Background()
	home := isolate(t)
	dataA := filepath.Join(home, "old")
	if _, _, err := run(t, "", "init", "--data-dir", dataA); err != nil {
		t.Fatal(err)
	}
	portA := unusedPort(t)
	a := testServe(t, dataA, map[string]string{"HIVEMIND_ADVERTISE": "127.0.0.2", "HIVEMIND_CLUSTER_PORT": strconv.Itoa(portA)})

	// A worker joins the old address, and runs.
	var tok server.JoinTokenReply
	apiCall(t, a, dataA, http.MethodPost, "/v1/join-tokens", `{}`, &tok)
	wdir := filepath.Join(home, "worker")
	if err := enroll(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), wdir, "127.0.0.2", portA, tok.Token); err != nil {
		t.Fatal(err)
	}
	joined, err := loadWorkerState(wdir)
	if err != nil {
		t.Fatal(err)
	}
	certBefore, err := os.ReadFile(filepath.Join(wdir, workerCertFile))
	if err != nil {
		t.Fatal(err)
	}
	rt := newMemRuntime()
	stopWorker := startWorker(t, wdir, rt)
	waitUntil(t, "the worker registering", func() bool { return len(nodeRows(t, a, dataA)) == 2 })
	stopWorker()

	// The server's machine is lost; its backup is restored on another.
	file := filepath.Join(home, "hm.tar")
	if _, _, err := run(t, "", "backup", file, "--data-dir", dataA); err != nil {
		t.Fatal(err)
	}
	if err := a.stop(); err != nil {
		t.Fatal(err)
	}
	dataB := filepath.Join(home, "new")
	if _, _, err := run(t, "", "restore", file, "--data-dir", dataB); err != nil {
		t.Fatal(err)
	}
	portB := unusedPort(t)
	b := testServe(t, dataB, map[string]string{"HIVEMIND_ADVERTISE": "127.0.0.3", "HIVEMIND_CLUSTER_PORT": strconv.Itoa(portB)})
	ca, err := pki.LoadCA(dataB)
	if err != nil {
		t.Fatal(err)
	}
	joinB := net.JoinHostPort("127.0.0.3", strconv.Itoa(portB+1))
	if _, err := joinPortCert(joinB, "127.0.0.3", ca); err != nil {
		t.Errorf("the restored server's certificate for its new address: %v", err)
	}
	if _, err := joinPortCert(joinB, "127.0.0.2", ca); err == nil {
		t.Error("the restored server's certificate names the old address")
	}
	if got := serverAddress(nodeRows(t, b, dataB)); got != "127.0.0.3" {
		t.Errorf("the restored server's node has address %q, want 127.0.0.3", got)
	}
	if l := b.logs.String(); !strings.Contains(l, "this server's address changed since its last start") ||
		!strings.Contains(l, "hivemind node set-server 127.0.0.3") || !strings.Contains(l, "old_address=127.0.0.2") {
		t.Errorf("the restored server did not say that the workers need set-server:\n%s", l)
	}

	// The worker is moved, and runs with the restored server.
	newAddr := net.JoinHostPort("127.0.0.3", strconv.Itoa(portB))
	if err := runSetServer(ctx, io.Discard, wdir, newAddr); err != nil {
		t.Fatal(err)
	}
	stopWorker = startWorker(t, wdir, rt)
	waitUntil(t, "the moved worker ready on the restored server", func() bool {
		for _, r := range nodeRows(t, b, dataB) {
			if r.ID == joined.state.NodeID && r.State == "ready" {
				return true
			}
		}
		return false
	})
	stopWorker()
	moved, err := loadWorkerState(wdir)
	if err != nil {
		t.Fatal(err)
	}
	certAfter, err := os.ReadFile(filepath.Join(wdir, workerCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if moved.state.NodeID != joined.state.NodeID || moved.state.Server != newAddr || !bytes.Equal(certBefore, certAfter) {
		t.Errorf("the moved worker: %+v, certificate kept %v; it joined as %+v", moved.state, bytes.Equal(certBefore, certAfter), joined.state)
	}
	if err := b.stop(); err != nil {
		t.Fatal(err)
	}
	if hits := storeValuesWith(t, filepath.Join(dataB, storeFileName), "127.0.0.2"); len(hits) > 0 {
		t.Errorf("the restored store still names the old address:\n%s", strings.Join(hits, "\n"))
	}
}

// noteAddressChange: an IP address the workers joined needs set-server,
// which is a warning; a name, or an advertised IP that is not the bound
// one, is a note; no change, or no worker, is nothing.
func TestNoteAddressChange(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.InitCluster(ctx, store.Cluster{AdminTokenHash: auth.HashToken("x"), NodeID: "node-server"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, store.Node{ID: "node-server", Name: "s", Address: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	note := func(advertise, clusterAddr, addr string) string {
		logs := &syncLog{}
		noteAddressChange(ctx, st, &config.Config{Advertise: advertise, ClusterAddr: clusterAddr}, "node-server", addr, slog.New(slog.NewTextHandler(logs, nil)))
		return logs.String()
	}
	if l := note("10.0.0.9", "", "10.0.0.9"); l != "" {
		t.Errorf("without workers: %s", l)
	}
	if err := st.UpsertNode(ctx, store.Node{ID: "node-w1", Name: "w1"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ advertise, clusterAddr, addr, want string }{
		{"10.0.0.9", "", "10.0.0.9", "level=WARN msg=\"this server's address changed since its last start: workers that joined the old address still dial it"},
		{"hm.example.internal", "", "10.0.0.9", "level=INFO msg=\"this server's address changed since its last start: workers that dial --advertise reach it"},
		{"203.0.113.5", "10.0.0.9", "10.0.0.9", "level=INFO"},
		{"10.0.0.5", "", "10.0.0.5", ""},
		{"", "", "127.0.0.1", ""},
	} {
		l := note(c.advertise, c.clusterAddr, c.addr)
		if (c.want == "") != (l == "") || !strings.Contains(l, c.want) {
			t.Errorf("advertise %q, cluster-addr %q, address %s: logged %q, want %q", c.advertise, c.clusterAddr, c.addr, l, c.want)
		}
	}
}
