package docker

import (
	"context"
	"net/http"
	"testing"

	"github.com/ao/hivemind/internal/runtime"
)

// A redirect from the daemon is not followed, so the registry credential in
// X-Registry-Auth goes nowhere else, and the call fails.
func TestRedirectIsNotFollowed(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
		w.Header().Set("Location", "/v"+APIVersion+"/elsewhere")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	err := c.Pull(context.Background(), "registry.example.com/app:1",
		&runtime.RegistryAuth{Username: "u", Password: "secret", ServerAddress: "registry.example.com"})
	if err == nil {
		t.Fatal("a pull answered with a redirect succeeded")
	}
	reqs := fd.requests()
	if len(reqs) != 1 || reqs[0].Path != "/images/create" {
		t.Fatalf("requests = %+v, want only the pull: the redirect was followed", reqs)
	}
	if _, err := c.Inspect(context.Background(), "x"); err == nil {
		t.Fatal("an inspect answered with a redirect succeeded")
	}
	if n := len(fd.requests()); n != 2 {
		t.Fatalf("%d requests after the inspect, want 2", n)
	}
}
