package server

// Scale benchmarks for docs/scale.md. The file lives in
// scripts/soak/testdata, which the go command ignores, because it uses the
// server package's test fakes. To run it:
//
//	cp scripts/soak/testdata/scale_bench_test.go internal/server/
//	go test -run '^$' -bench Scale -benchtime 50x ./internal/server
//	rm internal/server/scale_bench_test.go
//
// Each benchmark builds a store with N apps of 5 replicas on 10 ready nodes,
// every replica reported running by its node, as the cluster in the scale
// run is after a deploy.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

const benchNodes = 10

type benchEnv struct {
	st      *store.Store
	h       http.Handler
	token   string
	nodes   []string
	reports map[string][]store.Status
}

func benchStore(b *testing.B, apps int, pendingApp bool) *benchEnv {
	b.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(b.TempDir(), "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	var nodes []string
	for i := 0; i < benchNodes; i++ {
		id := fmt.Sprintf("node-%012x", i+1)
		nodes = append(nodes, id)
		if err := st.UpsertNode(ctx, store.Node{ID: id, Name: id, CPU: 2, Memory: 4 << 30}); err != nil {
			b.Fatal(err)
		}
		if err := st.RenewLease(ctx, id); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < apps; i++ {
		a := spec.App{Name: fmt.Sprintf("app-%04d", i), Image: "nginx:alpine", Replicas: 5}
		if i%3 == 0 {
			a.Health = &spec.Health{HTTP: &spec.HTTPCheck{Path: "/", Port: 80}}
		}
		if _, err := st.PutApp(ctx, a); err != nil {
			b.Fatal(err)
		}
	}
	if pendingApp {
		// One replica that no node can take: 11 replicas of one host port on 10 nodes.
		a := spec.App{Name: "unplaceable", Image: "nginx:alpine", Replicas: benchNodes + 1,
			Ports: []spec.Port{{Host: 9999, Container: 80}}}
		if _, err := st.PutApp(ctx, a); err != nil {
			b.Fatal(err)
		}
	}
	reports := map[string][]store.Status{}
	for _, id := range nodes {
		reports[id] = reportFor(b, st, id)
		if err := st.ReportStatus(ctx, id, reports[id]); err != nil {
			b.Fatal(err)
		}
	}
	tok, _ := auth.NewToken()
	sessions, _ := auth.NewSessions(time.Hour)
	au, err := auth.NewAuthenticator(auth.HashToken(tok), sessions)
	if err != nil {
		b.Fatal(err)
	}
	srv, err := New(Config{Store: st, Runtime: &fakeRuntime{}, NodeID: nodes[0], Auth: au,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(srv.CloseStreams)
	return &benchEnv{st: st, h: srv.Handler(), token: tok, nodes: nodes, reports: reports}
}

// reportFor is what node id reports: every replica assigned to it, running.
func reportFor(b *testing.B, st *store.Store, id string) []store.Status {
	as, err := st.AssignmentsForNode(context.Background(), id)
	if err != nil {
		b.Fatal(err)
	}
	var out []store.Status
	for _, a := range as {
		if a.NodeID != id || a.Desired != store.Running {
			continue
		}
		out = append(out, store.Status{Name: fmt.Sprintf("hm-%s-%d-%d", a.App, a.Index, a.Generation), App: a.App,
			Replica: a.Index, Generation: a.Generation, ContainerID: fmt.Sprintf("%064x", len(out)+1), State: "running", Ready: true})
	}
	return out
}

var benchSizes = []int{20, 100, 200, 400}

func (e *benchEnv) get(b *testing.B, path string) int {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer "+e.token)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		b.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Len()
}

func BenchmarkScaleStatus(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("replicas=%d", n*5), func(b *testing.B) {
			e := benchStore(b, n, false)
			b.ResetTimer()
			size := 0
			for i := 0; i < b.N; i++ {
				size = e.get(b, "/v1/status")
			}
			b.ReportMetric(float64(size), "bytes")
		})
	}
}

func BenchmarkScaleApps(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("replicas=%d", n*5), func(b *testing.B) {
			e := benchStore(b, n, false)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e.get(b, "/v1/apps")
			}
		})
	}
}

func BenchmarkScaleNodes(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("replicas=%d", n*5), func(b *testing.B) {
			e := benchStore(b, n, false)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e.get(b, "/v1/nodes")
			}
		})
	}
}

func BenchmarkScaleSweep(b *testing.B) {
	for _, pend := range []bool{false, true} {
		for _, n := range benchSizes {
			b.Run(fmt.Sprintf("pending=%v/replicas=%d", pend, n*5), func(b *testing.B) {
				e := benchStore(b, n, pend)
				ctx := context.Background()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := e.st.ReconcileNodes(ctx); err != nil {
						b.Fatal(err)
					}
					if err := e.st.AdvanceRollouts(ctx); err != nil {
						b.Fatal(err)
					}
					if _, err := e.st.AdvanceDrains(ctx, time.Time{}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// One node's report (its fetch of assignments and its status), as each agent
// sends every 2 s.
func BenchmarkScaleReport(b *testing.B) {
	for _, pend := range []bool{false, true} {
		for _, n := range benchSizes {
			b.Run(fmt.Sprintf("pending=%v/replicas=%d", pend, n*5), func(b *testing.B) {
				e := benchStore(b, n, pend)
				ctx := context.Background()
				id := e.nodes[3]
				rep := reportFor(b, e.st, id)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := e.st.AssignmentsForNode(ctx, id); err != nil {
						b.Fatal(err)
					}
					if err := e.st.ReportStatus(ctx, id, rep); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// One more app of 5 replicas into a store that holds N apps.
func BenchmarkScalePut(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("replicas=%d", n*5), func(b *testing.B) {
			e := benchStore(b, n, false)
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				name := fmt.Sprintf("new-%06d", i)
				b.StartTimer()
				if _, err := e.st.PutApp(ctx, spec.App{Name: name, Image: "busybox:stable", Replicas: 5, Command: []string{"sleep", "1"}}); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := e.st.DeleteApp(ctx, name); err != nil {
					b.Fatal(err)
				}
				// The nodes report without the new app's containers, so its
				// replicas, marked for removal, are deleted.
				for _, id := range e.nodes {
					if err := e.st.ReportStatus(ctx, id, e.reports[id]); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
			}
		})
	}
}

// One node's report when it changed since the last: one container's
// readiness flips at every report, so every report writes its rows.
func BenchmarkScaleReportChanged(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("replicas=%d", n*5), func(b *testing.B) {
			e := benchStore(b, n, false)
			ctx := context.Background()
			id := e.nodes[3]
			rep := [2][]store.Status{reportFor(b, e.st, id), reportFor(b, e.st, id)}
			rep[1][0].Ready = false
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := e.st.ReportStatus(ctx, id, rep[i%2]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// GET /v1/status while the ten nodes report, one after another as fast as
// the store takes them, each report changed from the node's last.
func BenchmarkScaleStatusUnderReports(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("replicas=%d", n*5), func(b *testing.B) {
			e := benchStore(b, n, false)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				for flip := true; ctx.Err() == nil; flip = !flip {
					for _, id := range e.nodes {
						rep := slices.Clone(e.reports[id])
						if flip && len(rep) > 0 {
							rep[0].Ready = false
						}
						_ = e.st.ReportStatus(ctx, id, rep)
					}
				}
			}()
			b.Cleanup(func() {
				cancel()
				<-done
			})
			time.Sleep(100 * time.Millisecond)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e.get(b, "/v1/status")
			}
			b.StopTimer()
		})
	}
}
