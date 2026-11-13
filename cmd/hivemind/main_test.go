package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// run executes the CLI in-process and returns stdout, stderr and the error
// that main would print before exiting 1.
func run(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	root := newRoot(strings.NewReader(stdin), &out, &errb)
	root.SetArgs(args)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := root.ExecuteContext(ctx)
	return out.String(), errb.String(), err
}

// isolate points every default path at a temp dir and clears the client
// environment, so a test never reads or writes the real home directory.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	for _, k := range []string{envServer, envToken, envClientConfig, "HIVEMIND_DATA_DIR", "HIVEMIND_CONFIG", "HIVEMIND_API_ADDR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return home
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestInit(t *testing.T) {
	home := isolate(t)
	data := filepath.Join(home, "data")
	cc := filepath.Join(home, ".config", "hivemind", "config.yaml")

	out, _, err := run(t, "", "init", "--data-dir", data)
	if err != nil {
		t.Fatal(err)
	}
	if m := mode(t, data); m != 0o700 {
		t.Errorf("data dir mode %04o, want 0700", m)
	}
	tokPath := filepath.Join(data, auth.TokenFileName)
	if m := mode(t, tokPath); m != 0o600 {
		t.Errorf("token file mode %04o, want 0600", m)
	}
	token, err := auth.ReadAdminToken(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, token) != 1 {
		t.Errorf("token printed %d times, want once:\n%s", strings.Count(out, token), out)
	}

	// The store keeps the digest and never the token.
	st, err := store.Open(context.Background(), filepath.Join(data, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	cl, err := st.GetCluster(context.Background())
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if cl.AdminTokenHash != auth.HashToken(token) || !strings.HasPrefix(cl.NodeID, "node-") {
		t.Errorf("cluster record %+v does not match the token", cl)
	}
	db, _ := os.ReadFile(filepath.Join(data, storeFileName))
	wal, _ := os.ReadFile(filepath.Join(data, storeFileName+"-wal"))
	if bytes.Contains(append(db, wal...), []byte(token)) {
		t.Error("the database contains the token")
	}

	// The client config names the server and the token file, not the token.
	if m := mode(t, cc); m != 0o600 {
		t.Errorf("client config mode %04o, want 0600", m)
	}
	if m := mode(t, filepath.Dir(cc)); m != 0o700 {
		t.Errorf("client config dir mode %04o, want 0700", m)
	}
	b, _ := os.ReadFile(cc)
	if !strings.Contains(string(b), "server: http://127.0.0.1:4483") || !strings.Contains(string(b), "token-file: "+tokPath) {
		t.Errorf("client config:\n%s", b)
	}
	if strings.Contains(string(b), token) {
		t.Error("client config contains the token")
	}
	if c, err := (&clientFlags{}).client(); err != nil || c.token != token || c.base != "http://127.0.0.1:4483" {
		t.Errorf("client from the init config: %+v, %v", c, err)
	}

	// A second init refuses and leaves the token as it was.
	_, _, err = run(t, "", "init", "--data-dir", data)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second init: %v", err)
	}
	if again, _ := auth.ReadAdminToken(data); again != token {
		t.Fatal("second init changed the token")
	}

	// With the token file gone, the database record still refuses a new cluster.
	if err := os.Remove(tokPath); err != nil {
		t.Fatal(err)
	}
	_, _, err = run(t, "", "init", "--data-dir", data)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("init over a database with a cluster: %v", err)
	}
	if _, err := os.Stat(tokPath); err == nil {
		t.Fatal("refused init left a new token file behind")
	}
}

func TestInitKeepsExistingClientConfig(t *testing.T) {
	home := isolate(t)
	cc := filepath.Join(home, "client.yaml")
	if err := os.WriteFile(cc, []byte("server: http://example.invalid:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, "", "init", "--data-dir", filepath.Join(home, "d"), "--client-config", cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "left as it is") {
		t.Errorf("output does not say the client config was kept:\n%s", out)
	}
	if b, _ := os.ReadFile(cc); string(b) != "server: http://example.invalid:1\n" {
		t.Errorf("client config overwritten: %q", b)
	}
}

func TestInitRefusesWildcardWithoutTLS(t *testing.T) {
	home := isolate(t)
	_, _, err := run(t, "", "init", "--data-dir", filepath.Join(home, "d"), "--api-addr", "0.0.0.0:4483")
	if err == nil || !strings.Contains(err.Error(), "api-tls-cert") {
		t.Fatalf("err = %v", err)
	}
}

func TestServeWithoutInit(t *testing.T) {
	home := isolate(t)
	_, _, err := run(t, "", "serve", "--data-dir", filepath.Join(home, "d"))
	if err == nil || !strings.Contains(err.Error(), "hivemind init") {
		t.Fatalf("err = %v", err)
	}
}

// join refuses bad input before it opens a connection.
func TestJoinRejectsBadInputBeforeTheNetwork(t *testing.T) {
	isolate(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"join", "10.0.0.1"}, "--token is required"},
		{[]string{"join", "--token", "x", "10.0.0.1"}, "malformed"},
		{[]string{"join", "--token", "x", "--advertise", "10.0.0.2", "10.0.0.1"}, "does not apply"},
		{[]string{"join", "--token", "x", "10.0.0.1:65535"}, "join port"},
	}
	for _, c := range cases {
		_, _, err := run(t, "", c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
}

func TestNoServer(t *testing.T) {
	isolate(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + ln.Addr().String()
	ln.Close()
	tok, _ := auth.NewToken()
	for _, args := range [][]string{{"status"}, {"deploy", "--name", "web", "--image", "nginx"}, {"logs", "web"}, {"scale", "web", "2"}, {"delete", "web"}} {
		_, _, err := run(t, "", append(args, "--server", addr, "--token", tok)...)
		want := "cannot reach hivemind at " + addr + ": connection refused"
		if err == nil || err.Error() != want {
			t.Errorf("%v: err = %v, want %q", args, err, want)
		}
	}
}

func TestNoToken(t *testing.T) {
	isolate(t)
	_, _, err := run(t, "", "status")
	if err == nil || !strings.Contains(err.Error(), "no API token") {
		t.Fatalf("err = %v", err)
	}
}

func TestParsePort(t *testing.T) {
	cases := map[string]spec.Port{
		"8080:80":           {Host: 8080, Container: 80},
		"8080:80/udp":       {Host: 8080, Container: 80, Protocol: "udp"},
		"0.0.0.0:8080:80":   {Bind: "0.0.0.0", Host: 8080, Container: 80},
		"[::1]:53:53/udp":   {Bind: "::1", Host: 53, Container: 53, Protocol: "udp"},
		"10.0.0.2:443:8443": {Bind: "10.0.0.2", Host: 443, Container: 8443},
	}
	for in, want := range cases {
		got, err := parsePort(in)
		if err != nil || got != want {
			t.Errorf("%q -> %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"80", "a:b", "80:", ":80"} {
		if _, err := parsePort(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// fakeRuntime is enough of a runtime for the API: one container per app
// replica 0, and a two-line log.
type fakeRuntime struct{ runtime.Runtime }

func (fakeRuntime) List(_ context.Context, labels map[string]string) ([]runtime.Container, error) {
	return []runtime.Container{{ID: "c0", Name: "hivemind-web-0-g1", State: "running",
		Labels: map[string]string{runtime.LabelApp: labels[runtime.LabelApp], runtime.LabelReplica: "0", runtime.LabelGeneration: "1"}}}, nil
}

func (fakeRuntime) Logs(_ context.Context, _ string, _ runtime.LogOptions, stdout, stderr io.Writer) error {
	io.WriteString(stdout, "hello from stdout\n")
	io.WriteString(stderr, "hello from stderr\n")
	return nil
}

// TestClientAgainstAPI drives every client command against the real API
// handler with a fake runtime.
func TestClientAgainstAPI(t *testing.T) {
	home := isolate(t)
	ctx := context.Background()
	// n1 is not the cluster's own node, so it would wait MinReady before it
	// takes a replica. This test needs the replica placed on n1 at once.
	st, err := store.Open(ctx, filepath.Join(home, "d", storeFileName), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertNode(ctx, store.Node{ID: "n1", Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	tok, _ := auth.NewToken()
	sess, _ := auth.NewSessions(time.Hour)
	authn, _ := auth.NewAuthenticator(auth.HashToken(tok), sess)
	api, err := server.New(server.Config{Store: st, Runtime: fakeRuntime{}, NodeID: "n1", Auth: authn,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, tok)

	out, _, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--replicas", "2",
		"--port", "8080:80", "--env", "A=b=c")
	if err != nil || out != "web: created, generation 1, 2 replicas\n" {
		t.Fatalf("deploy: %q, %v", out, err)
	}
	a, _ := st.GetApp(ctx, "web")
	if a.Spec.Env["A"] != "b=c" || a.Spec.Ports[0].Host != 8080 || a.Spec.Ports[0].Bind != "127.0.0.1" {
		t.Fatalf("stored spec %+v", a.Spec)
	}

	file := filepath.Join(home, "app.yaml")
	os.WriteFile(file, []byte("name: api\nimage: ghcr.io/x/api:2\nreplicas: 1\nresources: {memory: 64Mi}\n"), 0o600)
	if out, _, err = run(t, "", "deploy", "-f", file); err != nil || !strings.HasPrefix(out, "api: created") {
		t.Fatalf("deploy -f: %q, %v", out, err)
	}
	if out, _, err = run(t, "name: api\nimage: ghcr.io/x/api:3\n", "deploy", "-f", "-", "--json"); err != nil {
		t.Fatalf("deploy -f -: %v", err)
	}
	var pr server.PutResult
	if err := json.Unmarshal([]byte(out), &pr); err != nil || pr.Created || pr.App.Generation != 2 {
		t.Fatalf("deploy --json: %q %+v %v", out, pr, err)
	}
	os.WriteFile(file, []byte("name: api\nimage: x\nimagee: y\n"), 0o600)
	if _, _, err = run(t, "", "deploy", "-f", file); err == nil || !strings.Contains(err.Error(), "imagee") {
		t.Fatalf("unknown key in -f: %v", err)
	}
	if _, _, err = run(t, "", "deploy", "-f", file, "--image", "x"); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("-f with --image: %v", err)
	}
	if _, _, err = run(t, "", "deploy", "--name", "bad", "--image", "Nginx:"); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("invalid image: %v", err)
	}

	if err := st.ReportStatus(ctx, "n1", []store.Status{{Name: "hivemind-web-0-g1", ContainerID: "0123456789abcdef", App: "web", Replica: 0, Generation: 1, State: "running"}}); err != nil {
		t.Fatal(err)
	}
	out, _, err = run(t, "", "status")
	if err != nil || !strings.Contains(out, "APP") || !strings.Contains(out, "0123456789ab ") || !strings.Contains(out, "pending") {
		t.Fatalf("status: %q, %v", out, err)
	}
	out, _, err = run(t, "", "status", "web", "--json")
	var as server.AppStatus
	if err != nil || json.Unmarshal([]byte(out), &as) != nil || as.Running != 1 || as.Desired != 2 {
		t.Fatalf("status web --json: %q, %v", out, err)
	}
	if _, _, err = run(t, "", "status", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("status nope: %v", err)
	}

	out, _, err = run(t, "", "logs", "web", "0", "--tail", "10")
	if err != nil || out != "hello from stdout\nhello from stderr\n" {
		t.Fatalf("logs: %q, %v", out, err)
	}

	out, _, err = run(t, "", "scale", "web", "3")
	if err != nil || out != "web: 3 replicas\n" {
		t.Fatalf("scale: %q, %v", out, err)
	}
	if _, _, err = run(t, "", "scale", "web", "0"); err == nil || !strings.Contains(err.Error(), "between 1") {
		t.Fatalf("scale 0: %v", err)
	}

	if out, _, err = run(t, "", "delete", "web"); err != nil || !strings.HasPrefix(out, "web: deleting") {
		t.Fatalf("delete: %q, %v", out, err)
	}
	if _, _, err = run(t, "", "delete", "web"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("second delete: %v", err)
	}

	// A wrong token is "unauthorized" for every command.
	other, _ := auth.NewToken()
	for _, args := range [][]string{{"status"}, {"deploy", "--name", "web", "--image", "nginx"}, {"logs", "api"}, {"scale", "api", "2"}, {"delete", "api"}} {
		_, _, err := run(t, "", append(args, "--token", other)...)
		if err == nil || err.Error() != "unauthorized" {
			t.Errorf("%v with a wrong token: %v", args, err)
		}
	}
	if _, _, err := run(t, "", "status", "--token", "hm_wrong"); err == nil || err.Error() != "unauthorized" {
		t.Errorf("malformed token: %v", err)
	}
}
