//go:build unix

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/cluster"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/store"
)

// deniedDocker is the error the Docker client returns when the socket is
// there but this user may not open it.
func deniedDocker() error {
	return &runtime.Error{Op: "info", Err: runtime.ErrUnavailable, Message: "lost the connection to the Docker daemon",
		Cause: &url.Error{Op: "Get", URL: "http://docker/v1.44/info",
			Err: &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.EACCES)}}}
}

// deniedRuntime is a node whose Docker socket this user may not open.
type deniedRuntime struct{ *memRuntime }

func (deniedRuntime) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{}, deniedDocker()
}

func TestDockerReachErrorSaysToJoinTheDockerGroup(t *testing.T) {
	err := dockerReachError("unix:///var/run/docker.sock", deniedDocker())
	if !strings.Contains(err.Error(), "cannot reach Docker at unix:///var/run/docker.sock: info: lost the connection") ||
		!strings.Contains(err.Error(), "add it to the docker group (sudo usermod -aG docker $USER, then log in again), or run hivemind as root") {
		t.Fatalf("permission denied: %v", err)
	}
	other := &runtime.Error{Op: "info", Err: runtime.ErrUnavailable, Cause: syscall.ECONNREFUSED}
	if err := dockerReachError("unix:///x.sock", other); strings.Contains(err.Error(), "docker group") {
		t.Fatalf("connection refused: %v", err)
	}
}

// A worker that may not open the Docker socket stops at once with what to
// do, rather than retrying every 5 s: a group it joins takes effect only for
// a new login.
func TestWorkerWithoutDockerAccessStops(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ca, err := pki.CreateCA(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cl, err := cluster.Listen(cluster.Config{Store: st, CA: ca, Tokens: pki.NewTokens(st.JoinTokens()),
		Advertise: "127.0.0.1", Port: 0, LocalNodeID: "node-server", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = cl.Serve() }()
	defer cl.Shutdown()
	wdir := filepath.Join(dir, "worker")
	key, der := issueForNode(t, ca, "node-w1")
	if err := saveWorkerState(wdir, workerState{NodeID: "node-w1", Server: cl.NodeAddr(), Joined: time.Now()}, key, pemEncodeCert(der), ca.CertPEM); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = runWorker(ctx, &config.Config{DataDir: wdir, DockerHost: "unix:///var/run/docker.sock", ReconcileInterval: time.Second},
		quiet, deniedRuntime{newMemRuntime()})
	if err == nil || !strings.Contains(err.Error(), "docker group") || time.Since(start) > 5*time.Second {
		t.Fatalf("worker without Docker access: %v after %s", err, time.Since(start))
	}
}
