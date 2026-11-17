package cluster

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
)

// Every node API reply carries the server's epoch. A request from a node
// that has seen a higher epoch is refused with 409 before the store is
// touched, and the server logs once that a newer copy of the cluster
// exists. A node that has seen the same epoch or a lower one, or sends
// none, is served. A guarded client refuses the server itself.
func TestEpochOnTheNodeAPI(t *testing.T) {
	logs := &lockedBuffer{}
	fx := newFixtureWith(t, func(c *Config) {
		c.Epoch = 2
		c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	ctx := context.Background()
	c, id, cert := joinedClient(t, fx)
	if err := c.Register(ctx, id, nodeapi.Register{Name: "w1", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	hc := fx.nodeClient(cert)
	send := func(method, path, seen string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(method, fx.nodeURL(path), nil)
		if err != nil {
			t.Fatal(err)
		}
		if seen != "" {
			req.Header.Set(nodeapi.EpochHeader, seen)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		b.Write(buf[:n])
		return resp, b.String()
	}
	lease := func() time.Time {
		t.Helper()
		n, err := fx.st.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return n.LastLease
	}

	for _, seen := range []string{"", "0", "1", "2", "x", "-3"} {
		resp, body := send(http.MethodGet, nodeapi.AssignmentsPath(id), seen)
		if resp.StatusCode != http.StatusOK || resp.Header.Get(nodeapi.EpochHeader) != "2" {
			t.Errorf("seen %q: %d %q, epoch %q; want 200 and epoch 2", seen, resp.StatusCode, body, resp.Header.Get(nodeapi.EpochHeader))
		}
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("logged an error for nodes at or below the epoch:\n%s", logs.String())
	}

	before := lease()
	time.Sleep(10 * time.Millisecond)
	for range 3 {
		resp, body := send(http.MethodPost, nodeapi.LeasePath(id), "3")
		if resp.StatusCode != http.StatusConflict || !strings.Contains(body, staleMessage) || resp.Header.Get(nodeapi.EpochHeader) != "2" {
			t.Fatalf("a node that has seen epoch 3: %d %q, epoch %q", resp.StatusCode, body, resp.Header.Get(nodeapi.EpochHeader))
		}
	}
	if after := lease(); !after.Equal(before) {
		t.Errorf("a refused lease renewed the lease: %s, then %s", before, after)
	}
	l := logs.String()
	if strings.Count(l, "level=ERROR") != 1 || !strings.Contains(l, "a newer copy of this cluster exists: this server was replaced by a restore") ||
		!strings.Contains(l, "node="+id+" node_epoch=3 epoch=2") || !strings.Contains(l, "Stop this server and disable its service") {
		t.Fatalf("the server's log after three refusals:\n%s", l)
	}

	// A worker's guarded client, having seen epoch 3, refuses the server
	// at 2 itself.
	g := nodeapi.NewEpochGuard(3)
	g.Log = slog.New(slog.DiscardHandler)
	gc, err := nodeapi.NewClient("https://"+fx.nodeAddr, mustClientTLS(t, fx, cert), nodeapi.WithEpochGuard(g))
	if err != nil {
		t.Fatal(err)
	}
	var stale *nodeapi.StaleServerError
	if err := gc.RenewLease(ctx, id); !errors.As(err, &stale) || stale.Epoch != 2 || stale.Seen != 3 {
		t.Fatalf("a guarded client that has seen epoch 3: %v", err)
	}
	// One that has seen 1 takes the server, and raises its epoch to 2.
	g1 := nodeapi.NewEpochGuard(1)
	gc1, err := nodeapi.NewClient("https://"+fx.nodeAddr, mustClientTLS(t, fx, cert), nodeapi.WithEpochGuard(g1))
	if err != nil {
		t.Fatal(err)
	}
	if err := gc1.RenewLease(ctx, id); err != nil || g1.Highest() != 2 {
		t.Fatalf("a guarded client that has seen epoch 1: %v, highest %d", err, g1.Highest())
	}
}
