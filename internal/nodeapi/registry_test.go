package nodeapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

const wirePassword = "wire-password-91c2"

func credAssignment(auth *store.RegistryAuth) store.Assignment {
	return store.Assignment{
		Replica: store.Replica{App: "priv", Index: 0, NodeID: "node-a", Generation: 1, Desired: store.Running},
		Spec:    spec.App{Name: "priv", Image: "ghcr.io/acme/app:1", Replicas: 1},
		Auth:    auth,
	}
}

func TestAssignmentCarriesRegistryAuth(t *testing.T) {
	auth := &store.RegistryAuth{Host: "ghcr.io", Username: "octocat", Password: wirePassword}
	b, err := json.Marshal(AssignmentList{Assignments: FromStore([]store.Assignment{credAssignment(auth)}, true)})
	if err != nil {
		t.Fatal(err)
	}
	var list AssignmentList
	if err := Decode(bytes.NewReader(b), MaxAssignmentsBytes, &list); err != nil {
		t.Fatal(err)
	}
	if err := list.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := list.Assignments[0].ToStore().Auth; got == nil || *got != *auth {
		t.Fatalf("credential after the wire: %+v", got)
	}

	// A node that did not ask gets no credential, and no trace of one.
	b, err = json.Marshal(AssignmentList{Assignments: FromStore([]store.Assignment{credAssignment(auth)}, false)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "registry_auth") || strings.Contains(string(b), wirePassword) {
		t.Fatalf("reply without the opt-in has a credential: %s", b)
	}
}

func TestAssignmentRegistryAuthValidate(t *testing.T) {
	cases := map[string]store.RegistryAuth{
		"another registry":   {Host: "registry.example.com", Username: "u", Password: wirePassword},
		"host not canonical": {Host: "GHCR.io", Username: "u", Password: wirePassword},
		"bad host":           {Host: "https://ghcr.io", Username: "u", Password: wirePassword},
		"no username":        {Host: "ghcr.io", Password: wirePassword},
		"no password":        {Host: "ghcr.io", Username: "u"},
		"newline":            {Host: "ghcr.io", Username: "u", Password: wirePassword + "\n"},
		"long password":      {Host: "ghcr.io", Username: "u", Password: strings.Repeat("p", store.MaxRegistryPasswordLen+1)},
	}
	for name, auth := range cases {
		list := AssignmentList{Assignments: FromStore([]store.Assignment{credAssignment(&auth)}, true)}
		err := list.Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
		if err != nil && strings.Contains(err.Error(), wirePassword) {
			t.Errorf("%s: the error %q has the password", name, err)
		}
	}
}

// assignment21 is the assignment a 2.0 or 2.1 worker decodes: the struct
// has not changed since 2.0. Decode refuses unknown fields, as it did then.
type assignment21 struct {
	App           string   `json:"app"`
	Index         int      `json:"index"`
	NodeID        string   `json:"node_id"`
	Generation    int      `json:"generation"`
	Desired       string   `json:"desired"`
	PendingReason string   `json:"pending_reason,omitempty"`
	Spec          spec.App `json:"spec"`
}

type assignmentList21 struct {
	Assignments []assignment21 `json:"assignments"`
}

// A 2.1 worker reads the reply for apps that need no credential. It never
// asks for credentials, so it gets none; if it did get one, its decoder
// would refuse the whole list, every app on the node with it, which is why
// the server sends them only on request.
func TestOlderWorkerReadsAssignments(t *testing.T) {
	plain := store.Assignment{
		Replica: store.Replica{App: "web", Index: 0, NodeID: "node-a", Generation: 2, Desired: store.Running},
		Spec:    spec.App{Name: "web", Image: "nginx:alpine", Replicas: 1, Env: map[string]string{"A": "1"}},
	}
	auth := &store.RegistryAuth{Host: "ghcr.io", Username: "octocat", Password: wirePassword}
	decode21 := func(as []store.Assignment, withAuth bool) (assignmentList21, error) {
		b, err := json.Marshal(AssignmentList{Assignments: FromStore(as, withAuth)})
		if err != nil {
			t.Fatal(err)
		}
		var list assignmentList21
		return list, Decode(bytes.NewReader(b), MaxAssignmentsBytes, &list)
	}

	// No credential stored: the reply is the same as a 2.1 server's, with
	// or without the opt-in.
	for _, withAuth := range []bool{false, true} {
		list, err := decode21([]store.Assignment{plain, credAssignment(nil)}, withAuth)
		if err != nil || len(list.Assignments) != 2 || list.Assignments[0].Spec.Env["A"] != "1" {
			t.Fatalf("2.1 decoder, opt-in %v: %+v, %v", withAuth, list, err)
		}
	}
	// A credential stored, and a worker that did not ask: it reads every
	// assignment, and the private image's pull fails on that worker alone.
	list, err := decode21([]store.Assignment{plain, credAssignment(auth)}, false)
	if err != nil || len(list.Assignments) != 2 {
		t.Fatalf("2.1 decoder, a credential stored: %+v, %v", list, err)
	}
	// What it would do with a credential it did not ask for.
	if _, err := decode21([]store.Assignment{plain, credAssignment(auth)}, true); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), `unknown field "registry_auth"`) {
		t.Fatalf("2.1 decoder with a credential: %v, want the unknown field refused", err)
	}
}
