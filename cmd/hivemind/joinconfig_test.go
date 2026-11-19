package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/cluster"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// joinServer is a cluster listener for join to enroll with: its node port
// is the one join is given, and its join port the one above it.
type joinServer struct {
	ca     *pki.CA
	tokens *pki.Tokens
	cl     *cluster.Cluster
}

func newJoinServer(t *testing.T) *joinServer {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ca, err := pki.CreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tokens := pki.NewTokens(st.JoinTokens())
	// join dials the port above the one it is given, so the two must be
	// a pair; a port whose neighbour is taken is tried again.
	var cl *cluster.Cluster
	for range 5 {
		if cl, err = cluster.Listen(cluster.Config{Store: st, CA: ca, Tokens: tokens, Advertise: "127.0.0.1",
			Port: unusedPort(t), LocalNodeID: "node-server", Logger: quietLog()}); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = cl.Serve() }()
	t.Cleanup(func() { _ = cl.Shutdown() })
	return &joinServer{ca: ca, tokens: tokens, cl: cl}
}

func (s *joinServer) token(t *testing.T) string {
	t.Helper()
	tok, _, err := s.tokens.Issue(s.ca.Fingerprint(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// badDockerHost is a Docker host the worker cannot use, so join returns once
// it has joined and written its settings, before it would run the agent.
const badDockerHost = "tcp://192.0.2.1:2375"

// join writes the worker settings it was given as flags to
// <data-dir>/hivemind.yaml, with mode 0600, once it has joined, so that
// serve --role worker, as the systemd unit and the README's restart run it,
// keeps them: accept4's workers, joined with --route-addr, ran no router
// after that restart. The join token is not in the file, and a flag given to
// serve still overrides it.
func TestJoinWritesTheWorkerSettings(t *testing.T) {
	isolate(t)
	s := newJoinServer(t)
	tok := s.token(t)
	dir := filepath.Join(t.TempDir(), "worker")
	_, _, err := run(t, "", "join", s.cl.NodeAddr(), "--token", tok, "--data-dir", dir,
		"--route-addr", "127.0.0.1", "--route-https-port", "8443", "--acme-email", "ops@example.com",
		"--acme-ca", "https://ca.example.com/dir", "--log-level", "debug", "--docker-host", badDockerHost)
	if err == nil || !strings.Contains(err.Error(), "tcp hosts must be loopback addresses") {
		t.Fatalf("join with a Docker host it cannot use: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, workerStateFile)); err != nil {
		t.Fatalf("join did not join: %v", err)
	}
	file := filepath.Join(dir, config.DefaultConfigName)
	if m := mode(t, file); m != 0o600 {
		t.Errorf("%s mode %04o, want 0600", file, m)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, not := range []string{tok, "token", "data-dir"} {
		if strings.Contains(string(b), not) {
			t.Errorf("%s has %q:\n%s", file, not, b)
		}
	}
	if !strings.HasPrefix(string(b), "# "+joinConfigHeader+"\n") {
		t.Errorf("%s does not start with its header:\n%s", file, b)
	}

	// serve --role worker with only the data directory: the router's
	// settings and the others, from the file.
	cfg := serveConfig(t, "--data-dir", dir)
	if cfg.RouteAddr != "127.0.0.1" || cfg.RouteHTTPSPort != 8443 || cfg.RouteHTTPPort != 80 || cfg.ACMEEmail != "ops@example.com" ||
		cfg.ACMECA != "https://ca.example.com/dir" || cfg.LogLevel != "debug" || cfg.DockerHost != badDockerHost {
		t.Fatalf("serve --role worker's config from the file: %+v", cfg)
	}
	if src := cfg.Source("route-addr"); src != "config file "+file {
		t.Errorf("route-addr comes from %q", src)
	}
	// A flag given to serve overrides the file, setting by setting.
	cfg = serveConfig(t, "--data-dir", dir, "--route-addr", "10.0.0.9")
	if cfg.RouteAddr != "10.0.0.9" || cfg.ACMEEmail != "ops@example.com" {
		t.Fatalf("serve --route-addr over the file: route-addr %q, acme-email %q", cfg.RouteAddr, cfg.ACMEEmail)
	}

	// Without worker flags, join writes no file: a setting from the
	// environment is not written, as with init.
	t.Setenv("HIVEMIND_DOCKER_HOST", badDockerHost)
	plain := filepath.Join(t.TempDir(), "plain")
	if _, _, err := run(t, "", "join", s.cl.NodeAddr(), "--token", s.token(t), "--data-dir", plain); err == nil ||
		!strings.Contains(err.Error(), "tcp hosts must be loopback addresses") {
		t.Fatalf("join with a Docker host it cannot use: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plain, workerStateFile)); err != nil {
		t.Fatalf("join did not join: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plain, config.DefaultConfigName)); err == nil {
		t.Fatal("join without worker flags wrote a config file")
	}
}

// A config file that exists is the user's, and is not changed: join refuses
// worker flags then, as init refuses serve flags, before it uses the token
// or makes the data directory. A directory that holds a joined node gets
// enroll's refusal, which says to run serve --role worker.
func TestJoinRefusesFlagsOverAnExistingConfigFile(t *testing.T) {
	home := isolate(t)
	s := newJoinServer(t)
	tok := s.token(t)
	dir := filepath.Join(home, "worker")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := writeFile(t, filepath.Join(dir, config.DefaultConfigName), []byte("log-level: warn\n"))
	_, _, err := run(t, "", "join", s.cl.NodeAddr(), "--token", tok, "--data-dir", dir, "--route-addr", "127.0.0.1", "--acme-email", "ops@example.com")
	if err == nil || !strings.Contains(err.Error(), file+" exists, and join writes the worker settings it is given (--route-addr, --acme-email) to it") {
		t.Fatalf("join over a config file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, workerStateFile)); err == nil {
		t.Fatal("the refused join joined")
	}
	if b, _ := os.ReadFile(file); string(b) != "log-level: warn\n" {
		t.Fatalf("the config file changed: %q", b)
	}
	// --config names a file elsewhere: the data directory is not made.
	other := filepath.Join(home, "other")
	if _, _, err := run(t, "", "join", s.cl.NodeAddr(), "--token", tok, "--data-dir", other, "--config", file, "--log-level", "debug"); err == nil ||
		!strings.Contains(err.Error(), file+" exists") {
		t.Fatalf("join --config with worker flags: %v", err)
	}
	if _, err := os.Stat(other); err == nil {
		t.Fatal("the refused join made its data directory")
	}
	// The token was not used: a join without the flags takes it, and
	// reads the file as it is.
	t.Setenv("HIVEMIND_DOCKER_HOST", badDockerHost)
	if _, _, err := run(t, "", "join", s.cl.NodeAddr(), "--token", tok, "--data-dir", dir); err == nil ||
		!strings.Contains(err.Error(), "tcp hosts must be loopback addresses") {
		t.Fatalf("join with the file and no flags: %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != "log-level: warn\n" {
		t.Fatalf("the config file changed: %q", b)
	}
	// Joined now: with flags, the refusal is enroll's.
	if _, _, err := run(t, "", "join", s.cl.NodeAddr(), "--token", s.token(t), "--data-dir", dir, "--route-addr", "127.0.0.1",
		"--acme-email", "ops@example.com"); err == nil || !strings.Contains(err.Error(), "already holds a joined node: run hivemind serve --role worker") {
		t.Fatalf("join on a joined directory with flags: %v", err)
	}
}
