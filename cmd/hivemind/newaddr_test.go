package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/pki"
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
