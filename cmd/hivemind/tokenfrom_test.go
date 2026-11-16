package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/auth"
)

// refusedFromFlag reports whether err is the refusal of a --token.
func refusedFromFlag(err error) bool {
	return errors.Is(err, errUnauthorized) && strings.HasPrefix(err.Error(), "unauthorized: the server at ") &&
		strings.HasSuffix(err.Error(), " refused the token from --token")
}

// A refused token says where the CLI got it: --token, HIVEMIND_TOKEN, or the
// token file the client config names. The last is the one that is wrong
// when --server names another cluster's server, so that case says to give
// that server's token.
func TestRefusedTokenSaysWhereItCameFrom(t *testing.T) {
	_, ts := apiServer(t, "")
	other, _ := auth.NewToken()

	_, _, err := run(t, "", "status", "--token", other)
	if want := "unauthorized: the server at " + ts.URL + " refused the token from --token"; err == nil || err.Error() != want {
		t.Errorf("--token: %v, want %q", err, want)
	}

	t.Setenv(envToken, other)
	_, _, err = run(t, "", "status")
	if want := "unauthorized: the server at " + ts.URL + " refused the token from HIVEMIND_TOKEN"; err == nil || err.Error() != want {
		t.Errorf("HIVEMIND_TOKEN: %v, want %q", err, want)
	}

	// The client config of this machine's own cluster, and --server naming
	// another server.
	os.Unsetenv(envToken)
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := auth.WriteAdminToken(dir, other); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	tokenFile := filepath.Join(dir, auth.TokenFileName)
	if err := writeClientConfig(cfg, clientConfig{Server: "http://127.0.0.1:4483", TokenFile: tokenFile}); err != nil {
		t.Fatal(err)
	}
	_, _, err = run(t, "", "status", "--client-config", cfg, "--server", ts.URL)
	want := "unauthorized: the server at " + ts.URL + " refused the token from the token file " + tokenFile + ", which the client config " +
		cfg + " names; for another cluster's server, give its token with --token or HIVEMIND_TOKEN"
	if err == nil || err.Error() != want || !errors.Is(err, errUnauthorized) {
		t.Errorf("the client config's token: %v\nwant %q", err, want)
	}
}
