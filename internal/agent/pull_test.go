package agent

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

const (
	privImage = "ghcr.io/acme/private:1"
	privUser  = "octocat"
	privPass  = "ghp_registry-password-1"
)

func privApp(replicas int) spec.App {
	return spec.App{Name: "priv", Image: privImage, Replicas: replicas}
}

func (e *env) login(host, user, pass string) {
	e.t.Helper()
	if _, err := e.st.PutRegistry(ctxT(e.t), host, user, pass); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) pulls() int {
	e.t.Helper()
	n := e.rt.counts(false)["pull"]
	e.rt.counts(true)
	return n
}

// checkNoSecret fails if the password or the username is in the agent's log
// or in any reported status.
func (e *env) checkNoSecret(secrets ...string) {
	e.t.Helper()
	sts := e.status("")
	for _, s := range secrets {
		if strings.Contains(e.logs.String(), s) {
			e.t.Fatalf("the agent's log contains %q:\n%s", s, e.logs.String())
		}
		for _, st := range sts {
			if strings.Contains(st.Error, s) {
				e.t.Fatalf("status of %s contains %q: %s", st.Name, s, st.Error)
			}
		}
	}
}

func TestPullBackoff(t *testing.T) {
	want := []time.Duration{0, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for n := 1; n < len(want); n++ {
		if got := pullBackoff(n); got != want[n] {
			t.Errorf("pullBackoff(%d) = %s, want %s", n, got, want[n])
		}
	}
	if got := pullBackoff(1000); got != pullCap {
		t.Errorf("pullBackoff(1000) = %s", got)
	}
}

// The pull of an image sends the credential its assignment carries, and a
// pull from a registry with none sends nothing.
func TestPullSendsTheAssignmentsCredential(t *testing.T) {
	e := newEnv(t)
	want := runtime.RegistryAuth{Username: privUser, Password: privPass, ServerAddress: "ghcr.io"}
	e.rt.makePrivate(privImage, want)
	e.login("ghcr.io", privUser, privPass)
	e.put(privApp(1))
	pub := web(1)
	pub.Ports = nil
	e.put(pub)
	e.reconcile()
	if got := e.running(); len(got) != 2 {
		t.Fatalf("running: %v", names(got))
	}
	sent := e.rt.sentAuths()
	if len(sent) != 2 {
		t.Fatalf("%d pulls, want 2", len(sent))
	}
	// Apps are reconciled by name: priv, then web.
	if sent[0] == nil || *sent[0] != want {
		t.Fatalf("pull of %s sent %+v, want %+v", privImage, sent[0], want)
	}
	if sent[1] != nil {
		t.Fatalf("pull of nginx sent a credential: %+v", sent[1])
	}
	e.checkNoSecret(privPass)
}

// A refused pull says which registry refused it and what to run, backs off,
// and is tried at once with a new credential.
func TestRefusedPullNamesTheRegistryAndBacksOff(t *testing.T) {
	e := newEnv(t)
	e.rt.makePrivate(privImage, runtime.RegistryAuth{Username: privUser, Password: privPass, ServerAddress: "ghcr.io"})
	e.put(privApp(1))

	err := e.agent.Reconcile(ctxT(t))
	if !errors.Is(err, runtime.ErrUnauthorized) {
		t.Fatalf("first pass: %v, want ErrUnauthorized", err)
	}
	st := e.status("priv")
	if len(st) != 1 || st[0].State != "pending" ||
		!strings.Contains(st[0].Error, "ghcr.io wants a credential") ||
		!strings.Contains(st[0].Error, "hivemind registry login ghcr.io") {
		t.Fatalf("status without a login: %+v", st)
	}
	if e.pulls() != 1 {
		t.Fatal("want one pull")
	}

	// Backoff: 10 s, then 20 s. Passes in between pull nothing and succeed.
	e.reconcile()
	if n := e.pulls(); n != 0 {
		t.Fatalf("%d pulls inside the first backoff", n)
	}
	if w := e.agent.wake(); !w.Equal(e.clock.Now().Add(pullBase)) {
		t.Fatalf("wake-up %s, want the end of the backoff %s", w, e.clock.Now().Add(pullBase))
	}
	e.clock.Advance(pullBase)
	if err := e.agent.Reconcile(ctxT(t)); !errors.Is(err, runtime.ErrUnauthorized) {
		t.Fatalf("pass after the backoff: %v", err)
	}
	if n := e.pulls(); n != 1 {
		t.Fatalf("%d pulls after the first backoff, want 1", n)
	}
	e.clock.Advance(pullBase)
	e.reconcile()
	if n := e.pulls(); n != 0 {
		t.Fatalf("%d pulls 10 s into the 20 s backoff", n)
	}

	// A wrong password is a new credential: it is tried at once.
	e.login("ghcr.io", privUser, "wrong-password-2")
	if err := e.agent.Reconcile(ctxT(t)); !errors.Is(err, runtime.ErrUnauthorized) {
		t.Fatalf("pass with the wrong password: %v", err)
	}
	if n := e.pulls(); n != 1 {
		t.Fatalf("%d pulls after the login, want 1", n)
	}
	st = e.status("priv")
	if len(st) != 1 || !strings.Contains(st[0].Error, "ghcr.io refused the stored credential (unauthorized)") ||
		!strings.Contains(st[0].Error, "hivemind registry login ghcr.io") {
		t.Fatalf("status with the wrong password: %+v", st)
	}
	e.checkNoSecret("wrong-password-2", privPass, privUser)

	// The right one runs the replica, and clears the error.
	e.login("ghcr.io", privUser, privPass)
	e.reconcile()
	st = e.status("priv")
	if len(st) != 1 || st[0].State != "running" || st[0].Error != "" {
		t.Fatalf("status after the login: %+v", st)
	}
	e.checkNoSecret("wrong-password-2", privPass, privUser)
	if len(e.agent.pulls) != 0 {
		t.Fatalf("pull state left after the pull worked: %v", e.agent.pulls)
	}
}

// A logout changes the next pull only: a running container keeps running,
// and a new replica that must pull is refused again.
func TestLogoutLeavesRunningContainers(t *testing.T) {
	e := newEnv(t)
	e.rt.makePrivate(privImage, runtime.RegistryAuth{Username: privUser, Password: privPass, ServerAddress: "ghcr.io"})
	e.login("ghcr.io", privUser, privPass)
	e.put(privApp(1))
	e.reconcile()
	before := e.running()["hivemind-priv-0-g1"]
	if before.ID == "" {
		t.Fatal("replica 0 is not running")
	}

	if err := e.st.DeleteRegistry(ctxT(t), "ghcr.io"); err != nil {
		t.Fatal(err)
	}
	e.rt.removeImage(privImage)
	e.reconcile()
	if c := e.running()["hivemind-priv-0-g1"]; c.ID != before.ID {
		t.Fatalf("replica 0 changed after the logout: %+v", c)
	}
	if _, err := e.st.Scale(ctxT(t), "priv", 2); err != nil {
		t.Fatal(err)
	}
	if err := e.agent.Reconcile(ctxT(t)); !errors.Is(err, runtime.ErrUnauthorized) {
		t.Fatalf("new replica after the logout: %v", err)
	}
	for _, s := range e.status("priv") {
		switch s.Replica {
		case 0:
			if s.State != "running" || s.Error != "" {
				t.Fatalf("replica 0: %+v", s)
			}
		case 1:
			if s.State != "pending" || !strings.Contains(s.Error, "hivemind registry login ghcr.io") {
				t.Fatalf("replica 1: %+v", s)
			}
		}
	}
}

// A credential whose host is not the image's registry is never sent.
func TestCredentialForAnotherRegistryIsNotSent(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	as := store.Assignment{Auth: &store.RegistryAuth{Host: "registry.example.com", Username: "u", Password: "p"}}
	if got, host := registryAuth(as, privImage, log); got != nil || host != "ghcr.io" {
		t.Fatalf("registryAuth for another host: %+v, %q", got, host)
	}
	as.Auth.Host = "ghcr.io"
	got, _ := registryAuth(as, privImage, log)
	if got == nil || *got != (runtime.RegistryAuth{Username: "u", Password: "p", ServerAddress: "ghcr.io"}) {
		t.Fatalf("registryAuth: %+v", got)
	}
	// Docker Hub's names are one registry.
	as.Auth.Host = "docker.io"
	if got, _ := registryAuth(as, "registry-1.docker.io/acme/app:1", log); got == nil || got.ServerAddress != "docker.io" {
		t.Fatalf("registryAuth for registry-1.docker.io: %+v", got)
	}
}

func TestRefusedMessages(t *testing.T) {
	plain := &runtime.Error{Op: "pull", Err: runtime.ErrUnauthorized, Message: "the registry refused the pull (the token has expired)"}
	both := &runtime.Error{Op: "pull", Err: errors.Join(runtime.ErrNotFound, runtime.ErrUnauthorized), Message: "the registry refused the pull (no such image, or no access to it)"}
	cases := []struct {
		err  error
		sent bool
		want string
	}{
		{plain, false, "ghcr.io wants a credential, and none is stored for it; run: hivemind registry login ghcr.io"},
		{plain, true, "ghcr.io refused the stored credential (the token has expired); run: hivemind registry login ghcr.io"},
		{both, false, "ghcr.io has no such image, or it is private; check the image name, or run: hivemind registry login ghcr.io"},
		{both, true, "ghcr.io has no such image, or the stored credential cannot pull it; check the image name, or run: hivemind registry login ghcr.io"},
	}
	for _, c := range cases {
		err := refused(privImage, "ghcr.io", c.sent, c.err)
		if want := "pulling " + privImage + ": " + c.want; err.Error() != want {
			t.Errorf("sent %v: %q\nwant %q", c.sent, err, want)
		}
		if !errors.Is(err, runtime.ErrUnauthorized) {
			t.Errorf("%q does not match ErrUnauthorized", err)
		}
	}
}
