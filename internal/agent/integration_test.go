//go:build integration

// These tests need a real Docker daemon. scripts/integration-docker.sh starts
// one (docker:dind) and sets DOCKER_HOST to its socket. Run them only through
// that script.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

const itInterval = DefaultInterval

func itRuntime(t *testing.T) *docker.Client {
	t.Helper()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		t.Fatal("DOCKER_HOST is not set: run scripts/integration-docker.sh")
	}
	c, err := docker.New(host)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// itStartedAt reads State.StartedAt from the engine directly. The runtime's
// Container type does not carry it.
func itStartedAt(t *testing.T, id string) string {
	t.Helper()
	sock := strings.TrimPrefix(os.Getenv("DOCKER_HOST"), "unix://")
	hc := &http.Client{Transport: &http.Transport{Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	resp, err := hc.Get("http://docker/v" + docker.APIVersion + "/containers/" + id + "/json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b struct {
		State struct{ StartedAt string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	return b.State.StartedAt
}

// running is one replica's container: its ID by replica label.
type running map[string]runtime.Container

func itRunning(t *testing.T, rt runtime.Runtime, app string) (all int, byReplica running) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := rt.List(ctx, map[string]string{runtime.LabelApp: app})
	if err != nil {
		t.Fatal(err)
	}
	byReplica = running{}
	for _, c := range cs {
		if c.Running() {
			byReplica[c.Labels[runtime.LabelReplica]] = c
		}
	}
	return len(cs), byReplica
}

func itWait(t *testing.T, what string, timeout time.Duration, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > timeout {
			t.Fatalf("%s: not done after %s", what, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return time.Since(start)
}

// itAgent runs an agent until the returned stop function is called.
func itAgent(t *testing.T, rt runtime.Runtime, st *store.Store) (*Agent, func()) {
	t.Helper()
	a, err := New(Config{NodeID: "node-1", Runtime: rt, Source: st, Interval: itInterval,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	return a, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("agent did not stop")
		}
	}
}

// TestIntegrationSingleNode is PLAN task 4's acceptance on one node: deploy,
// replace a container removed out of band, restart the agent and store
// without touching running containers, replace on a spec change, delete.
func TestIntegrationSingleNode(t *testing.T) {
	rt := itRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var b [4]byte
	_, _ = rand.Read(b[:])
	appName := "web-" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		cs, _ := rt.List(context.Background(), map[string]string{runtime.LabelApp: appName})
		for _, c := range cs {
			_ = rt.Remove(context.Background(), c.ID, true)
		}
	})

	path := filepath.Join(t.TempDir(), "data", "hivemind.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := rt.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, store.Node{ID: "node-1", Name: "local", CPU: info.NCPU, Memory: info.MemTotal}); err != nil {
		t.Fatal(err)
	}
	a, stop := itAgent(t, rt, st)

	// Deploy 2 replicas: 2 running containers within 30 s, the image pull
	// included.
	app := spec.App{Name: appName, Image: "nginx:alpine", Replicas: 2}
	if _, err := st.PutApp(ctx, app); err != nil {
		t.Fatal(err)
	}
	a.Trigger()
	took := itWait(t, "2 replicas running", 30*time.Second, func() bool {
		_, r := itRunning(t, rt, appName)
		return len(r) == 2
	})
	t.Logf("deploy: 2 replicas running after %s", took.Round(time.Millisecond))
	itWait(t, "status reported", 10*time.Second, func() bool {
		ss, _ := st.ListStatus(ctx, appName)
		return len(ss) == 2 && ss[0].State == "running" && ss[1].State == "running"
	})
	_, first := itRunning(t, rt, appName)
	for r, c := range first {
		if want := ContainerName(appName, atoi(r), 1); c.Name != want {
			t.Fatalf("replica %s: name %q, want %q", r, c.Name, want)
		}
	}

	// docker rm -f of replica 0: the agent replaces it and leaves replica 1.
	if err := rt.Remove(ctx, first["0"].ID, true); err != nil {
		t.Fatal(err)
	}
	took = itWait(t, "replica 0 replaced", 30*time.Second, func() bool {
		_, r := itRunning(t, rt, appName)
		return len(r) == 2 && r["0"].ID != first["0"].ID
	})
	t.Logf("rm -f: replica 0 replaced after %s", took.Round(time.Millisecond))
	_, afterRm := itRunning(t, rt, appName)
	if afterRm["1"].ID != first["1"].ID {
		t.Fatal("replica 1 was replaced though only replica 0 was removed")
	}

	// Restart: stop the agent, close the store, open both again on the same
	// file and daemon. Nothing is restarted or duplicated.
	before := map[string]string{}
	for r, c := range afterRm {
		before[r] = c.ID + " " + itStartedAt(t, c.ID)
	}
	stop()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := time.Now()
	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, stop = itAgent(t, rt, st)
	defer func() { stop() }()
	// Wait for a pass by the new agent, then two more intervals.
	itWait(t, "a pass by the new agent", 30*time.Second, func() bool {
		ss, _ := st.ListStatus(ctx, appName)
		return len(ss) == 2 && ss[0].UpdatedAt.After(restarted)
	})
	time.Sleep(2 * itInterval)
	all, afterRestart := itRunning(t, rt, appName)
	if all != 2 || len(afterRestart) != 2 {
		t.Fatalf("after restart: %d containers, %d running, want 2 and 2", all, len(afterRestart))
	}
	for r, c := range afterRestart {
		if got := c.ID + " " + itStartedAt(t, c.ID); got != before[r] {
			t.Fatalf("replica %s: ID and StartedAt changed across the restart: %q, was %q", r, got, before[r])
		}
	}
	t.Logf("restart: same 2 container IDs and StartedAt times after the new agent's passes")

	// A spec change replaces both replicas with generation 2.
	app.Env = map[string]string{"VERSION": "2"}
	if got, err := st.PutApp(ctx, app); err != nil || got.Generation != 2 {
		t.Fatalf("PutApp: %+v %v", got, err)
	}
	a.Trigger()
	took = itWait(t, "generation 2 running", 30*time.Second, func() bool {
		all, r := itRunning(t, rt, appName)
		return all == 2 && len(r) == 2 && r["0"].Labels[runtime.LabelGeneration] == "2" && r["1"].Labels[runtime.LabelGeneration] == "2"
	})
	t.Logf("spec change: both replicas at generation 2 after %s", took.Round(time.Millisecond))

	// Delete: the containers go, then the store forgets the app.
	if err := st.DeleteApp(ctx, appName); err != nil {
		t.Fatal(err)
	}
	a.Trigger()
	took = itWait(t, "containers removed", 30*time.Second, func() bool {
		all, _ := itRunning(t, rt, appName)
		return all == 0
	})
	t.Logf("delete: containers removed after %s", took.Round(time.Millisecond))
	itWait(t, "app purged from the store", 10*time.Second, func() bool {
		reps, _ := st.Replicas(ctx, appName)
		return len(reps) == 0
	})
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
