package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// tableRows reads a table that tabwriter printed: each column starts where
// its header starts, and a header is the text up to two spaces.
func tableRows(t *testing.T, out string) []map[string]string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	header := lines[0]
	var names []string
	var starts []int
	for i := 0; i < len(header); {
		if header[i] == ' ' {
			i++
			continue
		}
		end := strings.Index(header[i:], "  ")
		if end < 0 {
			end = len(header) - i
		}
		names = append(names, header[i:i+end])
		starts = append(starts, i)
		i += end
	}
	var rows []map[string]string
	for _, l := range lines[1:] {
		row := map[string]string{}
		for k, s := range starts {
			e := len(l)
			if k+1 < len(starts) && starts[k+1] < e {
				e = starts[k+1]
			}
			if s < e {
				row[names[k]] = strings.TrimSpace(l[s:e])
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// hasRow reports whether a row has every value in want.
func hasRow(rows []map[string]string, want map[string]string) bool {
	for _, r := range rows {
		ok := true
		for k, v := range want {
			if r[k] != v {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TestNodesCommands drives hivemind nodes and hivemind node remove against
// the real API handler.
func TestNodesCommands(t *testing.T) {
	home := isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(home, "d", storeFileName), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.InitCluster(ctx, store.Cluster{AdminTokenHash: [32]byte{1}, NodeID: "node-s"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []store.Node{{ID: "node-s", Name: "srv", CPU: 4, Memory: 8 << 30, Version: "v2.2.0", Address: "10.0.0.5"},
		{ID: "node-w", Name: "wrk", CPU: 2, Memory: 1 << 30, Dials: "hivemind.example.internal:4484"}} {
		if err := st.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	// One replica per node: the scheduler spreads an app's replicas.
	if _, err := st.PutApp(ctx, spec.App{Name: "web", Image: "nginx:alpine", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	tok, _ := auth.NewToken()
	sess, _ := auth.NewSessions(time.Hour)
	authn, _ := auth.NewAuthenticator(auth.HashToken(tok), sess)
	api, err := server.New(server.Config{Store: st, Runtime: fakeRuntime{}, NodeID: "node-s", Auth: authn, Advertise: "hivemind.example.internal",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, tok)

	out, _, err := run(t, "", "nodes")
	if err != nil {
		t.Fatal(err)
	}
	rows := tableRows(t, out)
	if len(rows) != 2 {
		t.Fatalf("nodes table:\n%s", out)
	}
	for _, want := range []map[string]string{
		{"ID": "node-s", "NAME": "srv", "ROLE": "server", "ADDRESS": "10.0.0.5 (hivemind.example.internal)", "VERSION": "v2.2.0", "CPU": "4", "MEMORY": "8.0Gi", "STATE": "ready",
			"ELIGIBILITY": "eligible", "REPLICAS": "1", "ROUTER": "no", "CERT EXPIRES": "-"},
		{"ID": "node-w", "NAME": "wrk", "ROLE": "worker", "ADDRESS": "-", "VERSION": "-", "CPU": "2", "MEMORY": "1.0Gi", "STATE": "ready",
			"ELIGIBILITY": "eligible", "REPLICAS": "1", "ROUTER": "no", "CERT EXPIRES": "-"},
	} {
		if !hasRow(rows, want) {
			t.Errorf("no row %v in:\n%s", want, out)
		}
	}
	out, _, err = run(t, "", "nodes", "--json")
	var views []server.NodeView
	if err != nil || json.Unmarshal([]byte(out), &views) != nil || len(views) != 2 {
		t.Fatalf("nodes --json: %q, %v", out, err)
	}
	// What the workers dial: the server's --advertise on its node, and
	// what each worker registered.
	for _, v := range views {
		switch {
		case v.ID == "node-s" && (v.Advertise != "hivemind.example.internal" || v.Dials != ""):
			t.Errorf("the server's node in --json: %+v", v)
		case v.ID == "node-w" && (v.Dials != "hivemind.example.internal:4484" || v.Advertise != ""):
			t.Errorf("the worker in --json: %+v", v)
		}
	}
	if !strings.Contains(out, `"advertise": "hivemind.example.internal"`) || !strings.Contains(out, `"dials": "hivemind.example.internal:4484"`) {
		t.Errorf("nodes --json lacks advertise or dials:\n%s", out)
	}

	// The worker still renews its lease, so it needs --force.
	if _, _, err := run(t, "", "node", "remove", "node-w"); err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("remove a live node: %v", err)
	}
	if _, _, err := run(t, "", "node", "remove", "node-s", "--force"); err == nil || !strings.Contains(err.Error(), "runs the server") {
		t.Fatalf("remove the server's node: %v", err)
	}
	if _, _, err := run(t, "", "node", "remove"); err == nil {
		t.Fatal("node remove with no ID succeeded")
	}
	out, _, err = run(t, "", "node", "remove", "node-w", "--force")
	if err != nil || out != "node-w (wrk): removed; 1 replica placed again\n" {
		t.Fatalf("node remove --force: %q, %v", out, err)
	}
	if _, _, err := run(t, "", "node", "remove", "node-w", "--force"); err == nil || !strings.Contains(err.Error(), "already removed") {
		t.Fatalf("second remove: %v", err)
	}
	out, _, err = run(t, "", "nodes")
	if err != nil || strings.Contains(out, "node-w") || !strings.Contains(out, "node-s") {
		t.Fatalf("nodes after removal: %q, %v", out, err)
	}
	// Both replicas of web are now on the server's node.
	reps, _ := st.Replicas(ctx, "web")
	for _, r := range reps {
		if r.NodeID != "node-s" {
			t.Fatalf("replica %d on %q after the removal", r.Index, r.NodeID)
		}
	}
}

func TestCertExpiry(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	for _, c := range []struct {
		t           *time.Time
		notRenewing bool
		want        string
	}{
		{nil, false, "-"},
		{at(365 * 24 * time.Hour), false, "2027-11-14 (in 365d)"},
		{at(90 * time.Minute), false, "2026-11-14 (in 1h30m)"},
		{at(9*time.Minute + 50*time.Second), false, "2026-11-14 (in 10m)"},
		{at(30 * time.Second), false, "2026-11-14 (in 30s)"},
		{at(-time.Hour), false, "2026-11-14 (expired)"},
		{at(120*24*time.Hour + time.Hour), true, "2027-03-14 (in 120d, not renewing)"},
		{at(-time.Hour), true, "2026-11-14 (expired)"},
	} {
		if got := certExpiry(c.t, c.notRenewing, now); got != c.want {
			t.Errorf("certExpiry = %q, want %q", got, c.want)
		}
	}
}

// hivemind nodes marks a worker whose certificate is past its renewal time,
// from the server's record of when it was issued, and only that worker.
func TestNodesMarksANodeThatIsNotRenewing(t *testing.T) {
	st := nodesAPI(t)
	ctx := context.Background()
	day := 24 * time.Hour
	issued := time.Now().Add(-245*day + time.Hour) // 120 days and an hour left
	if err := st.RecordNodeCertValidity(ctx, "node-w", issued, issued.Add(365*day)); err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().Add(-10 * day)
	if err := st.RecordNodeCertValidity(ctx, "node-s", fresh, fresh.Add(365*day)); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, "", "nodes")
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		rows[strings.Fields(l)[0]] = l
	}
	if !strings.HasSuffix(rows["node-w"], "(in 120d, not renewing)") || strings.Contains(rows["node-s"], "not renewing") {
		t.Fatalf("nodes:\n%s\nwant node-w marked not renewing, and node-s not", out)
	}
	out, _, err = run(t, "", "nodes", "--json")
	var nodes []server.NodeView
	if err != nil || json.Unmarshal([]byte(out), &nodes) != nil {
		t.Fatalf("nodes --json: %q, %v", out, err)
	}
	for _, n := range nodes {
		if n.CertNotRenewing != (n.ID == "node-w") {
			t.Fatalf("nodes --json: %s has cert_not_renewing %v", n.ID, n.CertNotRenewing)
		}
	}
}

// nodesAPI starts the real API handler on a store with the server's node
// node-s and a worker node-w, and points the client commands at it. web has
// one replica on each node.
func nodesAPI(t *testing.T) *store.Store {
	t.Helper()
	home := isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(home, "d", storeFileName), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.InitCluster(ctx, store.Cluster{AdminTokenHash: [32]byte{1}, NodeID: "node-s"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []store.Node{{ID: "node-s", Name: "srv", CPU: 4, Memory: 8 << 30}, {ID: "node-w", Name: "wrk", CPU: 2, Memory: 1 << 30}} {
		if err := st.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.PutApp(ctx, spec.App{Name: "web", Image: "nginx:alpine", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	tok, _ := auth.NewToken()
	sess, _ := auth.NewSessions(time.Hour)
	authn, _ := auth.NewAuthenticator(auth.HashToken(tok), sess)
	api, err := server.New(server.Config{Store: st, Runtime: fakeRuntime{}, NodeID: "node-s", Auth: authn,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, tok)
	return st
}

// eligibilityOf returns the ELIGIBILITY column of each row of hivemind nodes.
func eligibilityOf(t *testing.T) map[string]string {
	t.Helper()
	out, _, err := run(t, "", "nodes")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range tableRows(t, out) {
		got[r["ID"]] = r["ELIGIBILITY"]
	}
	return got
}

// TestNodeEligibilityAndDrainCommands drives hivemind node eligibility and
// hivemind node drain against the real API handler. The test plays the
// agents' reports and the server's drain steps.
func TestNodeEligibilityAndDrainCommands(t *testing.T) {
	st := nodesAPI(t)
	ctx := context.Background()
	old := drainPoll
	drainPoll = 10 * time.Millisecond
	t.Cleanup(func() { drainPoll = old })
	running := func(app string, idx int) store.Status {
		return store.Status{Name: fmt.Sprintf("hivemind-%s-%d-g1", app, idx), App: app, Replica: idx, Generation: 1, State: "running", Ready: true}
	}
	report := func(node string, sts ...store.Status) {
		t.Helper()
		if err := st.ReportStatus(ctx, node, sts); err != nil {
			t.Fatal(err)
		}
	}
	step := func() {
		t.Helper()
		if _, err := st.AdvanceDrains(ctx, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{{"node", "eligibility", "node-w"}, {"node", "eligibility", "node-w", "--enable", "--disable"}} {
		if _, _, err := run(t, "", args...); err == nil || !strings.Contains(err.Error(), "one of --enable and --disable") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, _, err := run(t, "", "node", "eligibility", "node-x", "--disable"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown node: %v", err)
	}
	out, _, err := run(t, "", "node", "eligibility", "node-w", "--disable")
	if err != nil || out != "node-w (wrk): ineligible; it keeps its replicas and takes no new ones\n" {
		t.Fatalf("disable: %q, %v", out, err)
	}
	if got := eligibilityOf(t); got["node-w"] != "ineligible" || got["node-s"] != "eligible" {
		t.Fatalf("nodes: %v", got)
	}

	for _, d := range []string{"0s", "-1m", "200h"} {
		if _, _, err := run(t, "", "node", "drain", "node-w", "--deadline="+d); err == nil || !strings.Contains(err.Error(), "--deadline must be") {
			t.Fatalf("--deadline %s: %v", d, err)
		}
	}
	// --no-wait starts the drain and prints its first step. web/1 is on
	// node-w, and its copy starts on node-s.
	out, _, err = run(t, "", "node", "drain", "node-w", "--no-wait", "--deadline", "5m")
	if err != nil || !strings.HasPrefix(out, "node-w: draining 1 replica; deadline ") || !strings.Contains(out, "\n  moving web/1 to node-s\n") ||
		!strings.Contains(out, "Not waiting.") {
		t.Fatalf("drain --no-wait: %q, %v", out, err)
	}
	if got := eligibilityOf(t); got["node-w"] != "draining" {
		t.Fatalf("nodes during the drain: %v", got)
	}
	out, _, err = run(t, "", "node", "drain", "node-w", "--no-wait", "--json")
	var v server.DrainView
	if err != nil || json.Unmarshal([]byte(out), &v) != nil || v.State != "draining" || v.Deadline.Sub(v.StartedAt) != 5*time.Minute {
		t.Fatalf("drain --no-wait --json on a draining node, which keeps its deadline: %q, %v", out, err)
	}

	// waitFor polls the store until ok is true.
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Without --no-wait the command waits for the drain to finish. Its new
	// deadline shows that its request has arrived.
	type result struct {
		out string
		err error
	}
	waited := make(chan result, 1)
	go func() {
		out, _, err := run(t, "", "node", "drain", "node-w", "--deadline", "6m")
		waited <- result{out, err}
	}()
	waitFor("the new deadline", func() bool {
		d, err := st.GetDrain(ctx, "node-w")
		return err == nil && d.Deadline.Sub(d.StartedAt) > 5*time.Minute
	})
	report("node-s", running("web", 0), running("web", 1))
	step()
	report("node-w")
	step()
	var r result
	select {
	case r = <-waited:
	case <-time.After(20 * time.Second):
		t.Fatal("node drain did not return after the drain finished")
	}
	if r.err != nil || !strings.HasSuffix(r.out, "node-w: drained; 1 moved, 0 stopped at the deadline and placed again, 0 left on the node\n") {
		t.Fatalf("waiting drain: %q, %v", r.out, r.err)
	}
	if got := eligibilityOf(t); got["node-w"] != "drained" {
		t.Fatalf("nodes after the drain: %v", got)
	}

	// Nothing runs on the drained node, so it is removed without --force.
	out, _, err = run(t, "", "node", "remove", "node-w")
	if err != nil || out != "node-w (wrk): removed; 0 replicas placed again\n" {
		t.Fatalf("remove the drained node: %q, %v", out, err)
	}

	// The server's own node can be drained. With no other node, its replicas
	// stay, and the waiting command ends when the drain is cancelled.
	go func() {
		out, _, err := run(t, "", "node", "drain", "node-s")
		waited <- result{out, err}
	}()
	waitFor("the drain of node-s", func() bool { _, err := st.GetDrain(ctx, "node-s"); return err == nil })
	out, _, err = run(t, "", "node", "eligibility", "node-s", "--enable")
	if err != nil || out != "node-s (srv): eligible; its drain was cancelled, and the replicas still on it stay\n" {
		t.Fatalf("enable: %q, %v", out, err)
	}
	select {
	case r = <-waited:
	case <-time.After(20 * time.Second):
		t.Fatal("node drain did not return after the drain was cancelled")
	}
	if r.err == nil || !strings.Contains(r.err.Error(), "ended before it finished") ||
		!strings.Contains(r.out, "  web/0 stays: no other node is ready and eligible\n") {
		t.Fatalf("cancelled drain: %q, %v", r.out, r.err)
	}
}

func TestBytesIEC(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 1023: "1023B", 1024: "1.0Ki", 1536: "1.5Ki", 8 << 30: "8.0Gi", 3 << 40: "3.0Ti"} {
		if got := bytesIEC(n); got != want {
			t.Errorf("bytesIEC(%d) = %q, want %q", n, got, want)
		}
	}
}

// A drain whose node was lost says so, once, and counts what the loss took.
func TestDrainSummarySaysTheNodeWasLost(t *testing.T) {
	at := time.Date(2026, 11, 16, 1, 28, 7, 0, time.UTC)
	var b strings.Builder
	p := &drainPrinter{out: &b, seen: map[string]bool{}}
	d := server.DrainView{NodeID: "node-w", State: "draining", Moved: 4}
	p.update(d)
	d.Lost, d.LostAt = 1, &at
	p.update(d)
	d.State = "done"
	p.finish(d)
	lost := "  the node was lost at 2026-11-16T01:28:07Z, during the drain: its lease was not renewed for 15 s, " +
		"so its replicas without a volume were placed again at once, as on any node loss\n"
	want := "  4 moved, 0 left on the node\n" + lost +
		"node-w: drained; 4 moved, 0 stopped at the deadline and placed again, 1 placed again after the node was lost, 0 left on the node\n"
	if b.String() != want {
		t.Fatalf("drain output:\n%s\nwant:\n%s", b.String(), want)
	}

	b.Reset()
	p = &drainPrinter{out: &b, seen: map[string]bool{}}
	p.finish(server.DrainView{NodeID: "node-w", State: "done", Moved: 1, Stopped: 1})
	if want := "node-w: drained; 1 moved, 1 stopped at the deadline and placed again, 0 left on the node\n"; b.String() != want {
		t.Fatalf("drain output without a loss: %q, want %q", b.String(), want)
	}
}

func TestLeaseAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0s", 1400 * time.Millisecond: "1s", 3*time.Minute + 8*time.Second: "3m8s",
		47*time.Hour + 59*time.Minute: "47h59m0s", 48 * time.Hour: "2d", 2952*time.Hour + 46*time.Second: "123d",
	} {
		if got := leaseAge(d); got != want {
			t.Errorf("leaseAge(%s) = %q, want %q", d, got, want)
		}
	}
}

// While workers wait for their first report since the server started,
// hivemind nodes and hivemind status say so in a line after the tables, as
// their replicas show as unknown and count as neither running nor ready: in
// the fourth soak status showed 0 of 18 running for a minute while every
// app served. Once the start grace has ended with no report from any of
// them, the line says that the server keeps them waiting, and what to do.
// Once one reports, the others are lost and there is no line.
func TestStatusAndNodesSayWhyWorkersWait(t *testing.T) {
	home := isolate(t)
	ctx := context.Background()
	var mu sync.Mutex
	clock := time.Now().Add(-10 * time.Minute) // the workers' last leases
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if clock.IsZero() {
			return time.Now()
		}
		return clock
	}
	st, err := store.Open(ctx, filepath.Join(home, "d", storeFileName), store.WithMinReady(0), store.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.InitCluster(ctx, store.Cluster{AdminTokenHash: [32]byte{1}, NodeID: "node-s"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"node-s", "node-1", "node-2"} {
		if err := st.UpsertNode(ctx, store.Node{ID: id, Name: id, CPU: 2, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.PutApp(ctx, spec.App{Name: "web", Image: "nginx:alpine", Replicas: 3}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clock = time.Time{} // the real time from here on
	mu.Unlock()
	if err := st.UpsertNode(ctx, store.Node{ID: "node-s", Name: "node-s", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	tok, _ := auth.NewToken()
	sess, _ := auth.NewSessions(time.Hour)
	authn, _ := auth.NewAuthenticator(auth.HashToken(tok), sess)
	api, err := server.New(server.Config{Store: st, Runtime: fakeRuntime{}, NodeID: "node-s", Auth: authn,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, tok)
	both := func(want ...string) {
		t.Helper()
		for _, cmd := range []string{"nodes", "status"} {
			out, _, err := run(t, "", cmd)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range want {
				if !strings.Contains(out, w) {
					t.Errorf("hivemind %s does not say %q:\n%s", cmd, w, out)
				}
			}
			line := ""
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(l, " waiting") && (strings.HasPrefix(l, "1 worker ") || strings.Contains(l, " workers are ")) {
					line = l
				}
			}
			if (line != "") != (len(want) > 0) {
				t.Errorf("hivemind %s, want a waiting line %v:\n%s", cmd, len(want) > 0, out)
			}
			if strings.Contains(line, " lost ") || strings.Contains(line, " ready ") {
				t.Errorf("the line has a word that scripts look for in the table: %q", line)
			}
		}
	}

	// In the grace, 30 s after the start.
	if err := st.SetStarted(ctx, time.Now().Add(-30*time.Second), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	both("2 workers are waiting for their first report since the server started (start grace: 1m",
		" left); their replicas show as unknown until they report.")
	out, _, err := run(t, "", "nodes", "--json")
	if err != nil || !strings.Contains(out, `"start_grace_left_seconds": `) || strings.Contains(out, "start_held") {
		t.Fatalf("nodes --json in the grace: %v\n%s", err, out)
	}

	// The grace ended a minute ago, and neither worker has reported.
	if err := st.SetStarted(ctx, time.Now().Add(-3*time.Minute), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	both("2 workers are waiting: none of them has reported since the server started, and its start grace has ended",
		"so the server keeps them waiting and places none of their replicas again",
		"stop this server", "hivemind node remove <id> (docs/backup.md)")
	out, _, err = run(t, "", "nodes", "--json")
	if err != nil || strings.Count(out, `"start_held": true`) != 2 || strings.Contains(out, "start_grace_left_seconds") {
		t.Fatalf("nodes --json past the grace: %v\n%s", err, out)
	}
	for _, l := range []string{waitingLine([]nodeRow{{NodeView: server.NodeView{State: "waiting", StartHeld: true}}}),
		waitingLine([]nodeRow{{NodeView: server.NodeView{State: "waiting", StartGraceLeftSeconds: 61}}})} {
		if strings.Contains(l, " lost ") || strings.Contains(l, " ready ") || !strings.HasPrefix(l, "1 worker is waiting") {
			t.Errorf("one worker: %q", l)
		}
	}

	// node-1 reports: the hold is over; node-2 is lost, and there is no
	// line.
	if err := st.RenewLease(ctx, "node-1"); err != nil {
		t.Fatal(err)
	}
	both()
}

// ROUTER says which nodes run a router, from the server's "router": "yes"
// and "no" for a ready node, and "-" where the server cannot tell, for a
// node that is not ready and has not been heard from, or does not say, as a
// server older than 2.2 does not.
func TestNodesRouterColumn(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		router *bool
		state  string
		want   string
	}{
		{&yes, "ready", "yes"},
		{&yes, "waiting", "yes"},
		{&no, "ready", "no"},
		{&no, "lost", "-"},
		{&no, "waiting", "-"},
		{nil, "ready", "-"},
	} {
		n := nodeRow{Router: c.router}
		n.State = c.state
		if got := routerColumn(n); got != c.want {
			t.Errorf("router %v, state %s: %q, want %q", c.router, c.state, got, c.want)
		}
	}

	// From the real API: the server's node runs a router, and so does the
	// worker whose router read the routing table.
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), storeFileName), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, n := range []store.Node{{ID: "node-s", Name: "srv"}, {ID: "node-r", Name: "routes"}, {ID: "node-n", Name: "plain"}} {
		if err := st.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	readers := nodeapi.NewRouteReaders()
	readers.Seen("node-r", nodeapi.RouteRead{Paths: true, Protocols: true}, time.Now())
	tok, _ := auth.NewToken()
	sess, _ := auth.NewSessions(time.Hour)
	authn, _ := auth.NewAuthenticator(auth.HashToken(tok), sess)
	api, err := server.New(server.Config{Store: st, Runtime: fakeRuntime{}, NodeID: "node-s", Auth: authn, RouteReaders: readers,
		RouterReport: func() nodeapi.RouterReport { return nodeapi.RouterReport{} }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, tok)
	out, _, err := run(t, "", "nodes")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range tableRows(t, out) {
		got[r["ID"]] = r["ROUTER"]
	}
	if got["node-s"] != "yes" || got["node-r"] != "yes" || got["node-n"] != "no" {
		t.Fatalf("ROUTER column %v in:\n%s", got, out)
	}
	out, _, err = run(t, "", "nodes", "--json")
	if err != nil || strings.Count(out, `"router": true`) != 2 || strings.Count(out, `"router": false`) != 1 {
		t.Fatalf("nodes --json: %v\n%s", err, out)
	}
}
