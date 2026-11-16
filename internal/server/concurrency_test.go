package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/store"
)

// write is one API write of the concurrency test, with the time it was sent
// and the time its answer came.
type write struct {
	op         string // deploy, scale or delete
	app        string
	replicas   int
	seq        int // the SEQ env value of a deploy
	sent, done time.Time
	code       int
}

// effects are the parts of an app's state a write sets: whether the app
// exists, its replica count, and the SEQ of its spec.
func (w write) effects() map[string]string {
	switch w.op {
	case "deploy":
		return map[string]string{"exists": "true", "replicas": strconv.Itoa(w.replicas), "seq": strconv.Itoa(w.seq)}
	case "scale":
		return map[string]string{"replicas": strconv.Itoa(w.replicas)}
	case "delete":
		return map[string]string{"exists": "false"}
	}
	return nil
}

// Fifty goroutines deploy, scale and delete the same apps and their own, and
// drain nodes, through the HTTP API for 60 s (5 s with -short), while a
// sweeper and three node agents work on the store as serve and its workers
// do. Every answer is a 2xx, or a 404 or 409 that the API documents for the
// state the app or node was in; no 5xx. At the end the store's invariants
// hold, and each part of each app's state is the value of an acknowledged
// write that no later write, sent after it was acknowledged, overwrote.
func TestConcurrentWriters(t *testing.T) {
	dur := 60 * time.Second
	if testing.Short() {
		dur = 5 * time.Second
	}
	if v := os.Getenv("HIVEMIND_CONCURRENCY_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		dur = time.Duration(n) * time.Second
	}
	h := newHarness(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil)) })
	ctx := context.Background()
	nodes := []string{testNode, "node-2", "node-3"}
	for _, id := range nodes[1:] {
		if err := h.store.UpsertNode(ctx, store.Node{ID: id, Name: id, CPU: 2, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(h.srv.Handler())
	defer ts.Close()
	tr := &http.Transport{MaxIdleConnsPerHost: 64}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	send := func(method, path string, body any) (int, []byte, error) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+h.token)
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(resp.Body)
		return resp.StatusCode, out, err
	}

	stop := make(chan struct{})
	var bg sync.WaitGroup
	started := time.Now()
	// The sweeper: what serve does every few seconds, here all the time.
	bg.Add(1)
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = h.store.ReconcileNodes(ctx, started)
			_ = h.store.AdvanceRollouts(ctx)
			_, _ = h.store.AdvanceDrains(ctx, started)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	// The agents: each renews its lease and reports a running, ready
	// container for each replica assigned to it, and none for the rest, so
	// removed replicas are purged and rollouts and drains move.
	for _, id := range nodes {
		bg.Add(1)
		go func() {
			defer bg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = h.store.RenewLease(ctx, id)
				as, err := h.store.AssignmentsForNode(ctx, id)
				if err == nil {
					var sts []store.Status
					for _, a := range as {
						if a.Desired == store.Running && a.NodeID == id {
							sts = append(sts, store.Status{Name: fmt.Sprintf("hivemind-%s-%d-g%d", a.App, a.Index, a.Generation),
								ContainerID: fmt.Sprintf("c-%s-%d-%d", a.App, a.Index, a.Generation), App: a.App, Replica: a.Index,
								Generation: a.Generation, State: "running", Ready: true})
						}
					}
					_ = h.store.ReportStatus(ctx, id, sts)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}

	var mu sync.Mutex
	var writes []write
	var problems []string
	problem := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(problems) < 20 {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	seq := 0
	nextSeq := func() int {
		mu.Lock()
		defer mu.Unlock()
		seq++
		return seq
	}
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	const writers = 50
	for g := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 1))
			for time.Now().Before(deadline) {
				if g >= 45 {
					// Five drainers: drain a node, then make it eligible
					// again, which cancels a drain that has not finished.
					node := nodes[rng.IntN(len(nodes))]
					for _, req := range []struct {
						path string
						body any
					}{
						{"/v1/nodes/" + node + "/drain", map[string]string{"deadline": "2s"}},
						{"/v1/nodes/" + node + "/eligibility", map[string]bool{"eligible": true}},
					} {
						code, out, err := send("POST", req.path, req.body)
						switch {
						case err != nil:
							problem("POST %s: %v", req.path, err)
						case code >= 500 || (code/100 != 2 && code != http.StatusConflict):
							problem("POST %s: %d %s", req.path, code, out)
						}
					}
					time.Sleep(time.Duration(rng.IntN(20)) * time.Millisecond)
					continue
				}
				// Twenty writers share three apps; twenty-five own one each.
				app := fmt.Sprintf("own-%d", g)
				if g < 20 {
					app = fmt.Sprintf("shared-%d", rng.IntN(3))
				}
				w := write{app: app}
				var method, path string
				var body any
				switch r := rng.IntN(10); {
				case r < 5:
					w.op, w.replicas, w.seq = "deploy", 1+rng.IntN(3), nextSeq()
					method, path = "PUT", "/v1/apps/"+app
					body = map[string]any{"image": "busybox", "replicas": w.replicas,
						"env": map[string]string{"SEQ": strconv.Itoa(w.seq)}}
				case r < 8:
					w.op, w.replicas = "scale", 1+rng.IntN(3)
					method, path = "POST", "/v1/apps/"+app+"/scale"
					body = map[string]int{"replicas": w.replicas}
				default:
					w.op = "delete"
					method, path = "DELETE", "/v1/apps/"+app
				}
				w.sent = time.Now()
				code, out, err := send(method, path, body)
				w.done, w.code = time.Now(), code
				switch {
				case err != nil:
					problem("%s %s: %v", method, path, err)
					continue
				case code >= 500:
					problem("%s %s: %d %s", method, path, code, out)
				case code == http.StatusNotFound && w.op != "deploy":
					// Scale or delete of an app that is not there.
				case code/100 != 2:
					problem("%s %s: %d %s", method, path, code, out)
				}
				mu.Lock()
				writes = append(writes, w)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(stop)
	bg.Wait()
	for _, p := range problems {
		t.Error(p)
	}
	t.Logf("%d app writes in %s from %d goroutines", len(writes), dur, writers)

	if err := h.store.CheckInvariants(ctx); err != nil {
		t.Fatalf("store invariants after the run: %v", err)
	}

	// Each part of each app's state must be the value of a write that
	// could be the last: acknowledged with 2xx, and with no other
	// acknowledged write to that part sent after it was acknowledged.
	byApp := map[string][]write{}
	for _, w := range writes {
		if w.code/100 == 2 {
			byApp[w.app] = append(byApp[w.app], w)
		}
	}
	apps := make([]string, 0, len(byApp))
	for a := range byApp {
		apps = append(apps, a)
	}
	sort.Strings(apps)
	for _, app := range apps {
		got := map[string]string{"exists": "false"}
		code, out, err := send("GET", "/v1/apps/"+app, nil)
		if err != nil {
			t.Fatal(err)
		}
		if code == http.StatusOK {
			var v AppView
			if err := json.Unmarshal(out, &v); err != nil {
				t.Fatal(err)
			}
			got = map[string]string{"exists": "true", "replicas": strconv.Itoa(v.Replicas), "seq": v.Spec.Env["SEQ"]}
		}
		parts := []string{"exists"}
		if got["exists"] == "true" {
			parts = append(parts, "replicas", "seq")
		}
		for _, part := range parts {
			var last time.Time
			for _, w := range byApp[app] {
				if _, ok := w.effects()[part]; ok && w.sent.After(last) {
					last = w.sent
				}
			}
			allowed := map[string]bool{}
			for _, w := range byApp[app] {
				v, ok := w.effects()[part]
				if ok && !w.done.Before(last) {
					allowed[v] = true
				}
			}
			if len(allowed) > 0 && !allowed[got[part]] {
				t.Errorf("app %s: %s is %q, which no write that could be the last one set (%v)", app, part, got[part], allowed)
			}
		}
	}
}
