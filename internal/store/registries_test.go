package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

const testPassword = "registry-password-7f3a"

func TestRegistryCredentials(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newT(t, WithClock(func() time.Time { return now }))
	ctx := ctxT(t)

	if rs, err := s.ListRegistries(ctx); err != nil || len(rs) != 0 {
		t.Fatalf("empty store: %+v, %v", rs, err)
	}
	r, err := s.PutRegistry(ctx, "GHCR.io", "octocat", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if r.Host != "ghcr.io" || r.Username != "octocat" || !r.CreatedAt.Equal(now) || !r.UpdatedAt.Equal(now) {
		t.Fatalf("put: %+v", r)
	}
	// Docker Hub's names are one host.
	if r, err := s.PutRegistry(ctx, "index.docker.io", "hubuser", testPassword); err != nil || r.Host != "docker.io" {
		t.Fatalf("put index.docker.io: %+v, %v", r, err)
	}

	// Replacing keeps the creation time.
	later := now.Add(time.Hour)
	now = later
	r, err = s.PutRegistry(ctx, "ghcr.io", "octocat2", "another-password")
	if err != nil || r.Username != "octocat2" || !r.CreatedAt.Equal(later.Add(-time.Hour)) || !r.UpdatedAt.Equal(later) {
		t.Fatalf("replace: %+v, %v", r, err)
	}
	rs, err := s.ListRegistries(ctx)
	if err != nil || len(rs) != 2 || rs[0].Host != "docker.io" || rs[1].Host != "ghcr.io" || rs[1].Username != "octocat2" {
		t.Fatalf("list: %+v, %v", rs, err)
	}
	// A list carries no password: Registry has no field for one.
	if strings.Contains(fmt.Sprintf("%+v", rs), "password") {
		t.Fatalf("list shows a password: %+v", rs)
	}

	// Delete by any of Docker Hub's names.
	if err := s.DeleteRegistry(ctx, "registry-1.docker.io"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRegistry(ctx, "docker.io"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
	if err := s.DeleteRegistry(ctx, "https://ghcr.io"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete of a bad host: %v, want ErrInvalid", err)
	}
	if rs, _ := s.ListRegistries(ctx); len(rs) != 1 || rs[0].Host != "ghcr.io" {
		t.Fatalf("after delete: %+v", rs)
	}
}

func TestRegistryCredentialValidation(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	cases := []struct {
		host, user, pass, want string
	}{
		{"", "u", testPassword, "required"},
		{"https://ghcr.io", "u", testPassword, "scheme"},
		{"ghcr.io/org", "u", testPassword, "path"},
		{"myregistry", "u", testPassword, "Docker Hub"},
		{"ghcr.io", "", testPassword, "username is required"},
		{"ghcr.io", strings.Repeat("u", MaxRegistryUsernameLen+1), testPassword, "username is longer"},
		{"ghcr.io", "a:b", testPassword, "colon"},
		{"ghcr.io", "a\tb", testPassword, "control"},
		{"ghcr.io", "a\xffb", testPassword, "UTF-8"},
		{"ghcr.io", "u", "", "password is required"},
		{"ghcr.io", "u", testPassword + strings.Repeat("p", MaxRegistryPasswordLen), "password is longer"},
		{"ghcr.io", "u", testPassword + "\n", "control"},
		{"ghcr.io", "u", testPassword + "\x00", "control"},
		{"ghcr.io", "u", testPassword + "\xff", "UTF-8"},
	}
	for _, c := range cases {
		_, err := s.PutRegistry(ctx, c.host, c.user, c.pass)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("PutRegistry(%q, %q): %v, want ErrInvalid with %q", c.host, c.user, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), testPassword) {
			t.Errorf("error %q contains the password", err)
		}
	}
	if rs, _ := s.ListRegistries(ctx); len(rs) != 0 {
		t.Fatalf("refused puts stored %+v", rs)
	}
	// The limits themselves are allowed.
	if _, err := s.PutRegistry(ctx, "ghcr.io", strings.Repeat("u", MaxRegistryUsernameLen), strings.Repeat("p", MaxRegistryPasswordLen)); err != nil {
		t.Fatalf("credential at the limits: %v", err)
	}
}

func TestRegistryLimit(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	for i := range MaxRegistries {
		if _, err := s.PutRegistry(ctx, fmt.Sprintf("r%d.example.com", i), "u", "p"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutRegistry(ctx, "one-more.example.com", "u", "p"); !errors.Is(err, ErrConflict) {
		t.Fatalf("past the limit: %v, want ErrConflict", err)
	}
	// Replacing a stored one is still allowed.
	if _, err := s.PutRegistry(ctx, "r0.example.com", "u2", "p2"); err != nil {
		t.Fatalf("replace at the limit: %v", err)
	}
}

// byApp places app "priv" and "hub" on node1 and every other app on node2.
func byApp(nodes []Node, _ []Assignment, a spec.App, _ int, _ time.Time) (string, error) {
	if a.Name == "priv" || a.Name == "hub" {
		return node1, nil
	}
	return "node-2", nil
}

// A node gets the credential with each assignment that is to run an image
// from that registry, and nothing else does.
func TestAssignmentsCarryRegistryAuth(t *testing.T) {
	s := newT(t, WithPlacement(byApp))
	ctx := ctxT(t)
	if err := s.UpsertNode(ctx, Node{ID: "node-2", Name: "two", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, spec.App{Name: "priv", Image: "GHCR.io/acme/app:1", Replicas: 1})
	mustPut(t, s, spec.App{Name: "hub", Image: "registry-1.docker.io/acme/private:2", Replicas: 1})
	mustPut(t, s, spec.App{Name: "other", Image: "ghcr.io/acme/app:1", Replicas: 1})
	mustPut(t, s, spec.App{Name: "public", Image: "nginx:alpine", Replicas: 1})

	auths := func(node string) map[string]*RegistryAuth {
		t.Helper()
		as, err := s.AssignmentsForNode(ctx, node)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*RegistryAuth{}
		for _, a := range as {
			out[a.App] = a.Auth
		}
		return out
	}
	// No credential stored: none is sent.
	for _, n := range []string{node1, "node-2"} {
		for app, a := range auths(n) {
			if a != nil {
				t.Fatalf("%s on %s has a credential before any login: %+v", app, n, a)
			}
		}
	}

	if _, err := s.PutRegistry(ctx, "ghcr.io", "octocat", testPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutRegistry(ctx, "docker.io", "hubuser", "hub-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutRegistry(ctx, "registry.example.com", "unused", "unused-password"); err != nil {
		t.Fatal(err)
	}
	got := auths(node1)
	if a := got["priv"]; a == nil || *a != (RegistryAuth{Host: "ghcr.io", Username: "octocat", Password: testPassword}) {
		t.Fatalf("priv on node-1: %+v", a)
	}
	if a := got["hub"]; a == nil || a.Host != "docker.io" || a.Username != "hubuser" {
		t.Fatalf("hub on node-1: %+v", a)
	}
	got = auths("node-2")
	if a := got["other"]; a == nil || a.Host != "ghcr.io" {
		t.Fatalf("other on node-2: %+v", a)
	}
	// Every image on Docker Hub, public or not, gets Docker Hub's
	// credential, as docker pull sends it after docker login.
	if a := got["public"]; a == nil || a.Host != "docker.io" {
		t.Fatalf("public on node-2 is on docker.io, which has a credential: %+v", a)
	}
	if err := s.DeleteRegistry(ctx, "docker.io"); err != nil {
		t.Fatal(err)
	}
	if a := auths("node-2")["public"]; a != nil {
		t.Fatalf("public after the Docker Hub logout: %+v", a)
	}

	// A replica marked for removal pulls nothing, and gets no credential.
	if err := s.ReportStatus(ctx, node1, []Status{{Name: "hivemind-priv-0-g1", App: "priv", Generation: 1, State: "running"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteApp(ctx, "priv"); err != nil {
		t.Fatal(err)
	}
	as, err := s.AssignmentsForNode(ctx, node1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range as {
		if a.App == "priv" {
			found = true
			if a.Desired != Removed || a.Auth != nil {
				t.Fatalf("deleted replica: %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("the deleted replica, still reported, is not in the assignments")
	}

	// Logging out stops the credential going out.
	if err := s.DeleteRegistry(ctx, "ghcr.io"); err != nil {
		t.Fatal(err)
	}
	if a := auths("node-2")["other"]; a != nil {
		t.Fatalf("other after the logout: %+v", a)
	}
}
