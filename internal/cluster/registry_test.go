package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// A registry credential reaches the node that runs an image from that
// registry, when it asks for credentials, and no other node.
func TestRegistryCredentialsReachOnlyTheirNode(t *testing.T) {
	const password = "cluster-registry-password-5d1"
	var idA, idB string
	place := func(_ []store.Node, _ []store.Assignment, a spec.App, _ int, _ time.Time) (string, error) {
		if a.Name == "priv" {
			return idA, nil
		}
		return idB, nil
	}
	fx := newFixture(t, nil, store.WithPlacement(place), store.WithMinReady(0))
	ctx := context.Background()
	idA, certA := fx.enroll(fx.issue(time.Hour))
	idB, certB := fx.enroll(fx.issue(time.Hour))
	a, b := fx.nodeClient(certA), fx.nodeClient(certB)
	fx.registerNode(a, idA)
	fx.registerNode(b, idB)
	if _, err := fx.st.PutApp(ctx, spec.App{Name: "priv", Image: "ghcr.io/acme/app:1", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.PutApp(ctx, spec.App{Name: "pub", Image: "registry.example.com/acme/app:1", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.PutRegistry(ctx, "ghcr.io", "octocat", password); err != nil {
		t.Fatal(err)
	}

	// Node A, through the client a 2.2 worker uses, which asks.
	cfg, err := pki.ClientTLSConfig(fx.ca, certA, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	ca, err := nodeapi.NewClient("https://"+fx.nodeAddr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	as, err := ca.AssignmentsForNode(ctx, idA)
	if err != nil || len(as) != 1 || as[0].App != "priv" {
		t.Fatalf("A's assignments: %+v, %v", as, err)
	}
	if got := as[0].Auth; got == nil || got.Host != "ghcr.io" || got.Username != "octocat" || got.Password != password {
		t.Fatalf("A's credential: %+v", got)
	}

	// Node A without asking, as a 2.1 worker reads it: no credential.
	code, body := fx.send(a, http.MethodGet, fx.nodeURL(nodeapi.AssignmentsPath(idA)), nil)
	if code != http.StatusOK || strings.Contains(string(body), password) || strings.Contains(string(body), "registry_auth") {
		t.Fatalf("A's assignments without the opt-in: %d %s", code, body)
	}
	var list nodeapi.AssignmentList
	if err := json.Unmarshal(body, &list); err != nil || len(list.Assignments) != 1 {
		t.Fatalf("A's assignments without the opt-in: %s, %v", body, err)
	}

	// Node B asks, and runs nothing from ghcr.io: nothing.
	code, body = fx.send(b, http.MethodGet, fx.nodeURL(nodeapi.AssignmentsPath(idB))+"?"+nodeapi.RegistryAuthParam+"=1", nil)
	if code != http.StatusOK || strings.Contains(string(body), password) || strings.Contains(string(body), "registry_auth") {
		t.Fatalf("B's assignments: %d %s", code, body)
	}
	// Nor can it read A's.
	if code, body := fx.send(b, http.MethodGet, fx.nodeURL(nodeapi.AssignmentsPath(idA))+"?"+nodeapi.RegistryAuthParam+"=1", nil); code != http.StatusForbidden || strings.Contains(string(body), password) {
		t.Fatalf("B reads A's assignments: %d %s", code, body)
	}
}
