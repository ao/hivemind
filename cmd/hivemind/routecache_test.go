package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/router"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/tlsstore"
)

// tableRead is one read of the routing table by a router: when it started,
// and the endpoints it got for the one host.
type tableRead struct {
	at  time.Time
	eps []string
}

// The server's router, polling the store, which keeps the table between
// reads, sees a replica's ready flag change at its first read after the
// report: ten times over, with a lease renewal and a report the same as the
// last, which the store's kept table outlives, before each change.
func TestRouterSeesAReadyChangeAtItsNextPoll(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	const node = "node-a"
	if err := st.UpsertNode(ctx, store.Node{ID: node, Name: node, CPU: 2, Memory: 1 << 30, Address: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutApp(ctx, spec.App{Name: "web", Image: "nginx", Replicas: 1,
		Routes: []spec.Route{{Host: "www.example.com", Port: 80}}}); err != nil {
		t.Fatal(err)
	}
	report := func(ready bool) {
		t.Helper()
		if err := st.ReportStatus(ctx, node, []store.Status{{Name: "hivemind-web-0-g1", App: "web", Replica: 0, Generation: 1,
			State: "running", Ready: ready, RoutePorts: []store.RoutePort{{Port: 80, HostPort: 32768}}}}); err != nil {
			t.Fatal(err)
		}
	}
	report(true)

	var mu sync.Mutex
	var reads []tableRead
	routes := storeRoutes(st)
	ts, err := tlsstore.New(tlsstore.StoreBackend{St: st, NodeID: node}, tlsstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	const poll = 20 * time.Millisecond
	rt, err := router.New(router.Config{Addr: "127.0.0.1", Storage: ts, Email: "ops@example.com",
		// .invalid never resolves (RFC 6761), so no request leaves the host.
		CA: "https://acme.invalid/directory", Poll: poll, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Routes: func(ctx context.Context) (router.Table, error) {
			at := time.Now()
			tab, err := routes(ctx)
			if err == nil {
				var eps []string
				for _, r := range tab["www.example.com"] {
					eps = append(eps, r.Endpoints...)
				}
				mu.Lock()
				reads = append(reads, tableRead{at, eps})
				mu.Unlock()
			}
			return tab, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	go rt.Serve()
	t.Cleanup(func() { rt.Shutdown() })

	// firstAfter waits for the router's first read that started after t.
	firstAfter := func(t0 time.Time) tableRead {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			for _, r := range reads {
				if r.at.After(t0) {
					mu.Unlock()
					return r
				}
			}
			mu.Unlock()
			time.Sleep(poll / 4)
		}
		t.Fatalf("the router read no table in the 5 s after %s", t0.Format(time.StampMicro))
		return tableRead{}
	}
	want := []string{"10.0.0.1:32768"}
	if r := firstAfter(time.Now()); !slices.Equal(r.eps, want) {
		t.Fatalf("first read: %v, want %v", r.eps, want)
	}
	ready := true
	for i := range 10 {
		if err := st.RenewLease(ctx, node); err != nil {
			t.Fatal(err)
		}
		report(ready)
		firstAfter(time.Now())
		ready = !ready
		report(ready)
		at := time.Now()
		r := firstAfter(at)
		if w := want; ready != (len(r.eps) > 0) || (ready && !slices.Equal(r.eps, w)) {
			t.Fatalf("change %d to ready=%v: the router's first read after the report, %s after it, had endpoints %v",
				i, ready, r.at.Sub(at), r.eps)
		}
	}
}
