package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/store"
)

const cliPassword = "cli-registry-password-0a4f"

// noPassword fails if the password is in any of the outputs or errors.
func noPassword(t *testing.T, password string, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		if strings.Contains(o, password) {
			t.Fatalf("output contains the password: %q", o)
		}
	}
}

// webAuth returns the credential that web's running assignments carry.
func webAuth(t *testing.T, st *store.Store) *store.RegistryAuth {
	t.Helper()
	as, err := st.AssignmentsForNode(context.Background(), "node-s")
	if err != nil || len(as) == 0 {
		t.Fatalf("assignments: %+v, %v", as, err)
	}
	return as[0].Auth
}

func TestRegistryCommands(t *testing.T) {
	st := nodesAPI(t) // web runs nginx:alpine, from Docker Hub

	out, errb, err := run(t, cliPassword+"\n", "registry", "login", "index.docker.io", "--username", "hubuser", "--password-stdin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "docker.io: credential stored for hubuser, not checked with the registry") {
		t.Fatalf("login output: %q", out)
	}
	noPassword(t, cliPassword, out, errb)
	// One trailing newline is removed, and nothing else.
	if a := webAuth(t, st); a == nil || a.Host != "docker.io" || a.Username != "hubuser" || a.Password != cliPassword {
		t.Fatalf("stored credential: %+v", a)
	}

	// From a file, with a CRLF ending, replacing the first.
	pwFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(pwFile, []byte("file-"+cliPassword+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errb, err = run(t, "", "registry", "login", "docker.io", "--username", "hubuser2", "--password-file", pwFile)
	if err != nil {
		t.Fatal(err)
	}
	noPassword(t, cliPassword, out, errb)
	if a := webAuth(t, st); a == nil || a.Username != "hubuser2" || a.Password != "file-"+cliPassword {
		t.Fatalf("credential from the file: %+v", a)
	}
	// A password with spaces inside is kept as it is.
	if _, _, err := run(t, "  spaced "+cliPassword+"  ", "registry", "login", "ghcr.io", "--username", "octocat", "--password-stdin"); err != nil {
		t.Fatal(err)
	}

	out, errb, err = run(t, "", "registry", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "HOST") || !strings.Contains(lines[0], "USERNAME") || !strings.Contains(lines[0], "UPDATED") ||
		!strings.HasPrefix(lines[1], "docker.io") || !strings.Contains(lines[1], "hubuser2") || !strings.HasSuffix(lines[1], "UTC") ||
		!strings.HasPrefix(lines[2], "ghcr.io") || !strings.Contains(lines[2], "octocat") {
		t.Fatalf("registry list:\n%s", out)
	}
	noPassword(t, cliPassword, out, errb)
	out, errb, err = run(t, "", "registry", "list", "--json")
	if err != nil || !strings.Contains(out, `"host": "ghcr.io"`) || strings.Contains(out, "password") {
		t.Fatalf("registry list --json: %v\n%s", err, out)
	}
	noPassword(t, cliPassword, out, errb)

	out, _, err = run(t, "", "registry", "logout", "registry-1.docker.io")
	if err != nil || !strings.Contains(out, "docker.io: credential removed; running containers keep running") {
		t.Fatalf("logout: %v %q", err, out)
	}
	if a := webAuth(t, st); a != nil {
		t.Fatalf("credential after the logout: %+v", a)
	}
	if _, _, err := run(t, "", "registry", "logout", "docker.io"); err == nil || !strings.Contains(err.Error(), "no credential is stored for registry docker.io") {
		t.Fatalf("second logout: %v", err)
	}
	if _, _, err := run(t, "", "registry", "logout", "ghcr.io"); err != nil {
		t.Fatal(err)
	}
	if out, _, err := run(t, "", "registry", "list"); err != nil || strings.TrimSpace(out) != "No registries." {
		t.Fatalf("empty list: %v %q", err, out)
	}
}

// The password never comes from the command line, and a refused one is not
// echoed back.
func TestRegistryLoginInput(t *testing.T) {
	isolate(t)
	t.Setenv(envServer, "http://127.0.0.1:1") // nothing is sent in these cases
	t.Setenv(envToken, "hm_test")
	long := strings.Repeat("x", store.MaxRegistryPasswordLen) + cliPassword
	cases := []struct {
		name  string
		stdin string
		args  []string
		want  string
	}{
		{"password flag", "", []string{"ghcr.io", "--username", "u", "--password", cliPassword}, "unknown flag: --password"},
		{"no password source", "", []string{"ghcr.io", "--username", "u"}, "--password-stdin or --password-file"},
		{"both sources", cliPassword, []string{"ghcr.io", "--username", "u", "--password-stdin", "--password-file", "/dev/null"}, "not both"},
		{"no username", cliPassword, []string{"ghcr.io", "--password-stdin"}, "--username is required"},
		{"empty stdin", "", []string{"ghcr.io", "--username", "u", "--password-stdin"}, "is empty"},
		{"only a newline", "\n", []string{"ghcr.io", "--username", "u", "--password-stdin"}, "is empty"},
		{"too long", long, []string{"ghcr.io", "--username", "u", "--password-stdin"}, "longer than"},
		{"two lines", cliPassword + "\nmore\n", []string{"ghcr.io", "--username", "u", "--password-stdin"}, "control characters"},
		{"bad host", cliPassword, []string{"https://ghcr.io", "--username", "u", "--password-stdin"}, "without a scheme"},
		{"Docker Hub namespace", cliPassword, []string{"myregistry", "--username", "u", "--password-stdin"}, "Docker Hub"},
		{"missing file", "", []string{"ghcr.io", "--username", "u", "--password-file", filepath.Join(t.TempDir(), "nope")}, "no such file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errb, err := run(t, c.stdin, append([]string{"registry", "login"}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			noPassword(t, cliPassword, out, errb, err.Error())
		})
	}
}
