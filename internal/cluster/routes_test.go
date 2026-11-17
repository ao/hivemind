package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// A worker registers its private address, its routed containers report their
// ports, and its router reads the table over the node API.
func TestRoutesOverTheNodeAPI(t *testing.T) {
	fx := newFixture(t, nil, store.WithMinReady(0))
	ctx := context.Background()
	id, cert := fx.enroll(fx.issue(time.Hour))
	c := fx.apiClient(cert)

	// The local address of the connection is what the server sees; on this
	// test listener it is loopback, which a worker must not register.
	local, err := c.LocalAddress(ctx, id)
	if err != nil || local != "127.0.0.1" {
		t.Fatalf("local address %q, %v", local, err)
	}
	if err := c.Register(ctx, id, nodeapi.Register{Name: "w", CPU: 2, Memory: 1 << 30, Address: local}); err == nil {
		t.Fatal("a loopback address was registered")
	}
	if err := c.Register(ctx, id, nodeapi.Register{Name: "w", CPU: 2, Memory: 1 << 30, Address: "10.9.0.5"}); err != nil {
		t.Fatal(err)
	}
	if n, err := fx.st.GetNode(ctx, id); err != nil || n.Address != "10.9.0.5" {
		t.Fatalf("node after register: %+v, %v", n, err)
	}

	if _, err := fx.st.PutApp(ctx, spec.App{Name: "web", Image: "nginx", Replicas: 1,
		Routes: []spec.Route{{Host: "www.example.com", Port: 80}}}); err != nil {
		t.Fatal(err)
	}
	as, err := c.AssignmentsForNode(ctx, id)
	if err != nil || len(as) != 1 || len(as[0].Spec.Routes) != 1 {
		t.Fatalf("assignments: %+v, %v", as, err)
	}
	routes, err := c.Routes(ctx, id)
	if err != nil || len(routes) != 1 || routes[0].Host != "www.example.com" || len(routes[0].Endpoints) != 0 {
		t.Fatalf("routes before a report: %+v, %v", routes, err)
	}
	sts := []store.Status{{Name: "hivemind-web-0-g1", App: "web", Replica: 0, Generation: 1, State: "running", Ready: true,
		RoutePorts: []store.RoutePort{{Port: 80, HostPort: 32768}}}}
	if err := c.ReportStatus(ctx, id, sts); err != nil {
		t.Fatal(err)
	}
	routes, err = c.Routes(ctx, id)
	if err != nil || len(routes) != 1 || !slices.Equal(routes[0].Endpoints, []string{"10.9.0.5:32768"}) {
		t.Fatalf("routes after a report: %+v, %v", routes, err)
	}
}

// The TLS storage routes: every method through the worker's client, keys
// confined to certmagic's namespace, size limits, and locks held per node.
func TestTLSStorageOverTheNodeAPI(t *testing.T) {
	fx := newFixture(t, nil)
	ctx := context.Background()
	idA, certA := fx.enroll(fx.issue(time.Hour))
	idB, certB := fx.enroll(fx.issue(time.Hour))
	fx.registerNode(fx.nodeClient(certA), idA)
	fx.registerNode(fx.nodeClient(certB), idB)
	a, b := fx.apiClient(certA).TLS(idA), fx.apiClient(certB).TLS(idB)

	const key = "certificates/ca/www.example.com/www.example.com.key"
	if _, err := a.Load(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("load of a missing key: %v", err)
	}
	if _, err := a.List(ctx, "certificates", false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("list of a missing prefix: %v", err)
	}
	if _, err := a.Stat(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat of a missing key: %v", err)
	}
	if err := a.Store(ctx, key, []byte("private key")); err != nil {
		t.Fatal(err)
	}
	// Node B sees what node A stored: the storage is shared.
	if v, err := b.Load(ctx, key); err != nil || string(v) != "private key" {
		t.Fatalf("load from another node: %q, %v", v, err)
	}
	if ok, err := b.Exists(ctx, "certificates/ca"); !ok || err != nil {
		t.Fatalf("exists: %v %v", ok, err)
	}
	keys, err := b.List(ctx, "certificates", true)
	if err != nil || !slices.Equal(keys, []string{"certificates/ca", "certificates/ca/www.example.com", key}) {
		t.Fatalf("list: %v, %v", keys, err)
	}
	info, err := b.Stat(ctx, key)
	if err != nil || !info.Terminal || info.Size != int64(len("private key")) {
		t.Fatalf("stat: %+v, %v", info, err)
	}
	if err := b.Delete(ctx, "certificates/ca"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Exists(ctx, key); ok {
		t.Fatal("the key survived the delete of its directory")
	}

	// Locks: B cannot take what A holds, nor renew or release A's hold,
	// even with A's owner token.
	const lock = "issue_cert_www.example.com"
	if ok, err := a.TryLock(ctx, lock, "aaa-1", 30*time.Second); !ok || err != nil {
		t.Fatalf("A takes the lock: %v %v", ok, err)
	}
	if ok, err := b.TryLock(ctx, lock, "bbb-1", 30*time.Second); ok || err != nil {
		t.Fatalf("B took a held lock: %v %v", ok, err)
	}
	if err := b.Unlock(ctx, lock, "aaa-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("B released A's lock: %v", err)
	}
	if err := b.RenewLock(ctx, lock, "aaa-1", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("B renewed A's lock: %v", err)
	}
	if err := a.RenewLock(ctx, lock, "aaa-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := a.Unlock(ctx, lock, "aaa-1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.TryLock(ctx, lock, "bbb-1", 30*time.Second); !ok {
		t.Fatal("B could not take a released lock")
	}

	// Raw requests, past the client's own checks: the server refuses keys
	// outside the namespace and bodies over the limits.
	hc := fx.nodeClient(certA)
	for _, body := range []string{
		`{"key":"hivemind.db","value":"eA=="}`,
		`{"key":"acme/../../hivemind.db","value":"eA=="}`,
		`{"key":"/etc/passwd","value":"eA=="}`,
		`{"key":"locks/x","value":"eA=="}`,
		`{"key":"acme/x","value":"eA==","extra":1}`,
	} {
		code, _ := fx.send(hc, http.MethodPost, fx.nodeURL(nodeapi.TLSPath(idA, nodeapi.TLSStore)), []byte(body))
		if code != http.StatusBadRequest {
			t.Errorf("store %s: %d, want 400", body, code)
		}
	}
	big, _ := json.Marshal(nodeapi.TLSRequest{Key: "acme/x", Value: make([]byte, store.MaxTLSValueLen+1)})
	if code, _ := fx.send(hc, http.MethodPost, fx.nodeURL(nodeapi.TLSPath(idA, nodeapi.TLSStore)), big); code != http.StatusRequestEntityTooLarge && code != http.StatusBadRequest {
		t.Errorf("value over the limit: %d", code)
	}
	huge := []byte(`{"key":"acme/x","value":"` + strings.Repeat("A", nodeapi.MaxTLSRequestBytes) + `"}`)
	if code, _ := fx.send(hc, http.MethodPost, fx.nodeURL(nodeapi.TLSPath(idA, nodeapi.TLSStore)), huge); code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over the limit: %d, want 413", code)
	}
	if code, _ := fx.send(hc, http.MethodPost, fx.nodeURL(nodeapi.TLSPath(idA, "drop")), []byte(`{}`)); code != http.StatusBadRequest {
		t.Errorf("unknown operation: %d", code)
	}
	// A node cannot use another node's path.
	if code, _ := fx.send(hc, http.MethodPost, fx.nodeURL(nodeapi.TLSPath(idB, nodeapi.TLSLoad)), []byte(`{"key":"acme/x"}`)); code != http.StatusForbidden {
		t.Errorf("A on B's path: %d, want 403", code)
	}
	if code, _ := fx.send(hc, http.MethodGet, fx.nodeURL(nodeapi.RoutesPath(idB)), nil); code != http.StatusForbidden {
		t.Errorf("A reading routes on B's path: %d, want 403", code)
	}
	if _, err := fx.st.TLSLoad(ctx, "acme/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused request wrote: %v", err)
	}
}

// A removed node, and one whose certificate has expired, get nothing from
// the routing and TLS storage routes, as from every node route.
func TestRoutesAndTLSStorageRefuseRemovedAndExpiredNodes(t *testing.T) {
	clk := &fakeClock{}
	fx, _ := newRenewFixture(t, time.Hour, clk.now)
	ctx := context.Background()
	id, cert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	c := fx.apiClient(cert)
	if err := c.Register(ctx, id, nodeapi.Register{Name: id, CPU: 1, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.TLSStore(ctx, "acme/ca/users/a@example.com/a.key", []byte("account key")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TLS(id).Load(ctx, "acme/ca/users/a@example.com/a.key"); err != nil {
		t.Fatalf("load while valid: %v", err)
	}
	clk.advance(2 * time.Hour)
	if _, err := c.Routes(ctx, id); !errors.Is(err, nodeapi.ErrCertInvalid) {
		t.Errorf("routes with an expired certificate: %v", err)
	}
	if _, err := c.TLS(id).Load(ctx, "acme/ca/users/a@example.com/a.key"); !errors.Is(err, nodeapi.ErrCertInvalid) {
		t.Errorf("TLS load with an expired certificate: %v", err)
	}

	fx2 := newFixture(t, nil)
	id2, cert2 := fx2.enroll(fx2.issue(pki.DefaultTokenTTL))
	c2 := fx2.apiClient(cert2)
	fx2.registerNode(fx2.nodeClient(cert2), id2)
	if _, err := fx2.st.RemoveNode(ctx, id2, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Routes(ctx, id2); !errors.Is(err, nodeapi.ErrNodeRemoved) {
		t.Errorf("routes for a removed node: %v", err)
	}
	if _, err := c2.TLS(id2).Load(ctx, "acme/ca/users/a@example.com/a.key"); !errors.Is(err, nodeapi.ErrNodeRemoved) {
		t.Errorf("TLS load for a removed node: %v", err)
	}
	if _, err := c2.TLS(id2).TryLock(ctx, "issue_cert_x", "o-1", time.Minute); !errors.Is(err, nodeapi.ErrNodeRemoved) {
		t.Errorf("TLS lock for a removed node: %v", err)
	}
}

// A node may hold at most store.MaxTLSLocksPerHolder TLS locks: the next is
// refused with 409 and a message that says why, and another node is not
// affected.
func TestTLSLocksPerNodeOverTheNodeAPI(t *testing.T) {
	fx := newFixture(t, nil)
	ctx := context.Background()
	idA, certA := fx.enroll(fx.issue(time.Hour))
	idB, certB := fx.enroll(fx.issue(time.Hour))
	fx.registerNode(fx.nodeClient(certA), idA)
	fx.registerNode(fx.nodeClient(certB), idB)
	for i := 0; i < store.MaxTLSLocksPerHolder; i++ {
		if ok, err := fx.st.TLSTryLock(ctx, fmt.Sprintf("lock-%d", i), idA+"/o", time.Minute); !ok || err != nil {
			t.Fatalf("lock %d: %v %v", i, ok, err)
		}
	}
	body, _ := json.Marshal(nodeapi.TLSRequest{Name: "one-more", Owner: "o", TTLMillis: 30_000})
	code, reply := fx.send(fx.nodeClient(certA), http.MethodPost, fx.nodeURL(nodeapi.TLSPath(idA, nodeapi.TLSLock)), body)
	if code != http.StatusConflict || !strings.Contains(string(reply), "this node holds 1000 TLS locks, the most a node may hold") {
		t.Fatalf("lock over the cap: %d %s", code, reply)
	}
	if ok, err := fx.apiClient(certB).TLS(idB).TryLock(ctx, "one-more", "o", 30*time.Second); !ok || err != nil {
		t.Fatalf("another node at A's cap: %v %v", ok, err)
	}
}
