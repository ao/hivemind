package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/store"
)

// apiServer serves the real API handler, with the given server version, on
// a test listener, and points the CLI at it.
func apiServer(t *testing.T, serverVersion string) (*store.Store, *httptest.Server) {
	t.Helper()
	home := isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(home, "d", storeFileName), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertNode(ctx, store.Node{ID: "node-s", Name: "srv", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	tok, _ := auth.NewToken()
	sess, _ := auth.NewSessions(time.Hour)
	authn, _ := auth.NewAuthenticator(auth.HashToken(tok), sess)
	api, err := server.New(server.Config{Store: st, Runtime: fakeRuntime{}, NodeID: "node-s", Auth: authn,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: serverVersion})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, tok)
	return st, ts
}

// version prints the server's version when the CLI has a server and a
// token, says when the server reports none, and prints only its own
// without them.
func TestVersionCommandAsksTheServer(t *testing.T) {
	_, ts := apiServer(t, "v2.2.0")
	out, _, err := run(t, "", "version")
	if err != nil || !strings.HasPrefix(out, "hivemind "+version+" (") || !strings.Contains(out, "\nserver "+ts.URL+": hivemind v2.2.0\n") {
		t.Fatalf("version against a 2.2 server: %q, %v", out, err)
	}

	_, ts = apiServer(t, "")
	out, _, err = run(t, "", "version")
	if err != nil || !strings.Contains(out, "server "+ts.URL+": older than 2.2, which reports no version\n") {
		t.Fatalf("version against a server with no version: %q, %v", out, err)
	}

	isolate(t)
	out, _, err = run(t, "", "version")
	if err != nil || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "hivemind ") {
		t.Fatalf("version with no server: %q, %v", out, err)
	}
	if _, _, err := run(t, "", "version", "--server", "http://127.0.0.1:1", "--token", "hm_x"); err == nil ||
		!strings.Contains(err.Error(), "cannot reach hivemind") {
		t.Fatalf("version with a server that is down: %v", err)
	}
}
