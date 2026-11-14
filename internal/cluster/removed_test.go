package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
)

// A removed node's certificate still verifies against the CA, but every node
// route refuses it with 403, and the node API client reports ErrNodeRemoved.
// Another node is not affected.
func TestRemovedNodesCertificateIsRefused(t *testing.T) {
	fx := newFixture(t, nil)
	id, cert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	otherID, otherCert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	c, other := fx.nodeClient(cert), fx.nodeClient(otherCert)
	fx.registerNode(c, id)
	fx.registerNode(other, otherID)

	if _, err := fx.st.RemoveNode(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}

	routes := []struct{ method, path string }{
		{http.MethodPost, nodeapi.RegisterPath(id)},
		{http.MethodGet, nodeapi.AssignmentsPath(id)},
		{http.MethodPost, nodeapi.StatusPath(id)},
		{http.MethodGet, nodeapi.StatusPath(id)},
		// The denylist check runs before routing and before the path check.
		{http.MethodGet, nodeapi.AssignmentsPath(otherID)},
		{http.MethodGet, nodeapi.Prefix + "/nope"},
	}
	for _, r := range routes {
		var body []byte
		if r.method == http.MethodPost {
			body = []byte(`{}`)
		}
		code, b := fx.send(c, r.method, fx.nodeURL(r.path), body)
		var e nodeapi.ErrorBody
		_ = json.Unmarshal(b, &e)
		if code != http.StatusForbidden || e.Error != nodeapi.RemovedMessage {
			t.Errorf("%s %s from the removed node: %d %q, want 403 %q", r.method, r.path, code, e.Error, nodeapi.RemovedMessage)
		}
	}

	// The worker's own client reports the removal as ErrNodeRemoved.
	tlsCfg, err := pki.ClientTLSConfig(fx.ca, cert, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nodeapi.NewClient("https://"+fx.nodeAddr, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := nc.Register(ctx, id, nodeapi.Register{Name: id, CPU: 1, Memory: 1 << 30}); !errors.Is(err, nodeapi.ErrNodeRemoved) {
		t.Errorf("Register: %v", err)
	}
	if _, err := nc.AssignmentsForNode(ctx, id); !errors.Is(err, nodeapi.ErrNodeRemoved) {
		t.Errorf("AssignmentsForNode: %v", err)
	}
	if err := nc.ReportStatus(ctx, id, nil); !errors.Is(err, nodeapi.ErrNodeRemoved) {
		t.Errorf("ReportStatus: %v", err)
	}
	if _, err := nc.StatusForNode(ctx, id); !errors.Is(err, nodeapi.ErrNodeRemoved) {
		t.Errorf("StatusForNode: %v", err)
	}
	if _, err := fx.st.GetNode(ctx, id); err == nil {
		t.Error("the removed node registered again")
	}

	// The other node still works.
	if code, b := fx.send(other, http.MethodGet, fx.nodeURL(nodeapi.AssignmentsPath(otherID)), nil); code != http.StatusOK {
		t.Fatalf("assignments for a node that was not removed: %d %s", code, b)
	}
}

// Join never hands out the ID of a removed node, or of a registered one.
func TestJoinDoesNotReuseARemovedNodeID(t *testing.T) {
	fx := newFixture(t, nil)
	removedID, cert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	fx.registerNode(fx.nodeClient(cert), removedID)
	liveID, liveCert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	fx.registerNode(fx.nodeClient(liveCert), liveID)
	if _, err := fx.st.RemoveNode(context.Background(), removedID, true); err != nil {
		t.Fatal(err)
	}

	ids := []string{removedID, liveID, "node-0123456789ab"}
	orig := newNodeID
	t.Cleanup(func() { newNodeID = orig })
	newNodeID = func() (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	got, _ := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	if got != "node-0123456789ab" {
		t.Fatalf("join issued %q, want the first unused ID", got)
	}
}
