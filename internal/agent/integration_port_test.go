//go:build integration

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// Two apps that publish the same host port on one node: the second must stay
// pending with the engine's error, and never run without its port. Once the
// first app is deleted, the second starts with the port published.
func TestIntegrationHostPortClashStaysPending(t *testing.T) {
	rt := itRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	var b [4]byte
	_, _ = rand.Read(b[:])
	sfx := hex.EncodeToString(b[:])
	first, second := "pa-"+sfx, "pb-"+sfx
	t.Cleanup(func() {
		for _, app := range []string{first, second} {
			cs, _ := rt.List(context.Background(), map[string]string{runtime.LabelApp: app})
			for _, c := range cs {
				_ = rt.Remove(context.Background(), c.ID, true)
			}
		}
	})

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertNode(ctx, store.Node{ID: "node-1", Name: "local", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	a, stop := itAgent(t, rt, st)
	defer stop()

	const port = 18081
	ports := []spec.Port{{Host: port, Container: 80}}
	if _, err := st.PutApp(ctx, spec.App{Name: first, Image: "nginx:alpine", Ports: ports}); err != nil {
		t.Fatal(err)
	}
	a.Trigger()
	itWait(t, "the first app running", time.Minute, func() bool {
		_, r := itRunning(t, rt, first)
		return len(r) == 1
	})

	if _, err := st.PutApp(ctx, spec.App{Name: second, Image: "nginx:alpine", Ports: ports}); err != nil {
		t.Fatal(err)
	}
	a.Trigger()
	// The clash is caught by placement (the replica stays unassigned with
	// the scheduler's reason) or, if it got through, by the engine (the agent
	// reports "port is already allocated" and removes the container). Either
	// way the second app must never run.
	itWait(t, "the clash reported", time.Minute, func() bool {
		reps, err := st.Replicas(ctx, second)
		if err == nil && len(reps) == 1 && strings.Contains(reps[0].PendingReason, "in use") {
			return true
		}
		sts, err := st.ListStatus(ctx, second)
		return err == nil && len(sts) == 1 && strings.Contains(sts[0].Error, "already")
	})
	deadline := time.Now().Add(5 * itInterval)
	for time.Now().Before(deadline) {
		if _, r := itRunning(t, rt, second); len(r) != 0 {
			t.Fatalf("the second app runs while its host port belongs to the first: %v", r)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if sts, _ := st.ListStatus(ctx, second); len(sts) > 0 && (sts[0].State != "pending" || sts[0].ContainerID != "") {
		t.Fatalf("status of the second app during the clash: %+v", sts)
	}

	// Free the port: the second app starts, with the port published.
	if err := st.DeleteApp(ctx, first); err != nil {
		t.Fatal(err)
	}
	a.Trigger()
	itWait(t, "the second app running", time.Minute, func() bool {
		_, r := itRunning(t, rt, second)
		return len(r) == 1
	})
	itWait(t, "the second app's port answering", 30*time.Second, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}
