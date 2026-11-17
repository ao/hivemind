package main

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/store"
)

// A serve that cannot bind its listeners, here because something else
// holds the cluster port, records nothing: not the server node's new
// address, not the start, and it logs neither the address change nor the
// waiting workers. The next start that binds compares with the address of
// the last start that did. 127.0.0.2 and 127.0.0.3 stand for two addresses
// of the machine; Linux routes all of 127/8 to loopback.
func TestAStartThatCannotBindRecordsNothing(t *testing.T) {
	for _, a := range []string{"127.0.0.2", "127.0.0.3"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(a, "0"))
		if err != nil {
			t.Skipf("this machine cannot bind %s: %v", a, err)
		}
		ln.Close()
	}
	ctx := context.Background()
	home := isolate(t)
	data := filepath.Join(home, "server")
	if _, _, err := run(t, "", "init", "--data-dir", data); err != nil {
		t.Fatal(err)
	}
	port := unusedPort(t)
	s := testServe(t, data, map[string]string{"HIVEMIND_ADVERTISE": "127.0.0.2", "HIVEMIND_CLUSTER_PORT": strconv.Itoa(port)})
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	// A worker in the store, so that an address change is logged.
	dbPath := filepath.Join(data, storeFileName)
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := st.GetCluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, store.Node{ID: "node-w00000000001", Name: "w1"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	serverAddr := func() string {
		t.Helper()
		st, err := store.Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		n, err := st.GetNode(ctx, cl.NodeID)
		if err != nil {
			t.Fatal(err)
		}
		return n.Address
	}

	// Something else holds the cluster port on 127.0.0.3.
	held, err := net.Listen("tcp", net.JoinHostPort("127.0.0.3", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"HIVEMIND_DATA_DIR":     data,
		"HIVEMIND_API_ADDR":     "127.0.0.1:" + strconv.Itoa(unusedPort(t)),
		"HIVEMIND_ADVERTISE":    "127.0.0.3",
		"HIVEMIND_CLUSTER_PORT": strconv.Itoa(port),
	}
	cfg, err := config.Load(nil, config.Options{Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncLog{}
	log, err := newLogger(cfg, logs)
	if err != nil {
		t.Fatal(err)
	}
	err = runServe(ctx, cfg, log, newMemRuntime())
	held.Close()
	if err == nil || !strings.Contains(err.Error(), "cluster listener on 127.0.0.3") {
		t.Fatalf("serve with its cluster port held: %v", err)
	}
	if l := logs.String(); strings.Contains(l, "address changed") || strings.Contains(l, "wait for their first report") {
		t.Errorf("the start that did not bind logged as if it had started:\n%s", l)
	}
	if got := serverAddr(); got != "127.0.0.2" {
		t.Fatalf("after the start that did not bind, the server's node has address %q, want 127.0.0.2", got)
	}

	// The start that binds says the address changed from the last start
	// that did.
	b := testServe(t, data, map[string]string{"HIVEMIND_ADVERTISE": "127.0.0.3", "HIVEMIND_CLUSTER_PORT": strconv.Itoa(port)})
	if l := b.logs.String(); !strings.Contains(l, "old_address=127.0.0.2 address=127.0.0.3") {
		t.Errorf("the next start did not compare with 127.0.0.2:\n%s", l)
	}
	if err := b.stop(); err != nil {
		t.Fatal(err)
	}
	if got := serverAddr(); got != "127.0.0.3" {
		t.Errorf("after the start that bound, the server's node has address %q, want 127.0.0.3", got)
	}
}
