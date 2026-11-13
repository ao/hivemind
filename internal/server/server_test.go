package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/store"
)

// fakeRuntime implements runtime.Runtime with a fixed container list and a
// scripted log.
type fakeRuntime struct {
	mu         sync.Mutex
	containers []runtime.Container
	listErr    error
	// logLines are written one by one, logEvery apart. With Follow the
	// stream then stays open until ctx is cancelled.
	logLines []string
	logEvery time.Duration
	lastOpts runtime.LogOptions
	lastID   string
}

func (f *fakeRuntime) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{NCPU: 2, MemTotal: 1 << 30}, nil
}
func (f *fakeRuntime) Pull(context.Context, string) error { return nil }
func (f *fakeRuntime) Create(context.Context, runtime.ContainerConfig) (string, error) {
	return "", nil
}
func (f *fakeRuntime) Start(context.Context, string) error                 { return nil }
func (f *fakeRuntime) Stop(context.Context, string, time.Duration) error   { return nil }
func (f *fakeRuntime) Remove(context.Context, string, bool) error          { return nil }
func (f *fakeRuntime) Exec(context.Context, string, []string) (int, error) { return 0, nil }
func (f *fakeRuntime) Inspect(context.Context, string) (runtime.Container, error) {
	return runtime.Container{}, nil
}

func (f *fakeRuntime) List(_ context.Context, labels map[string]string) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []runtime.Container
	for _, c := range f.containers {
		ok := true
		for k, v := range labels {
			if got, has := c.Labels[k]; !has || (v != "" && got != v) {
				ok = false
			}
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeRuntime) Logs(ctx context.Context, id string, opts runtime.LogOptions, stdout, stderr io.Writer) error {
	f.mu.Lock()
	f.lastOpts, f.lastID = opts, id
	lines, every := f.logLines, f.logEvery
	f.mu.Unlock()
	for i, l := range lines {
		w := stdout
		if i%2 == 1 {
			w = stderr
		}
		if _, err := io.WriteString(w, l+"\n"); err != nil {
			return err
		}
		if every > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(every):
			}
		}
	}
	if opts.Follow {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

type harness struct {
	srv     *Server
	store   *store.Store
	rt      *fakeRuntime
	token   string
	logs    *syncBuffer
	trigger int
	mu      sync.Mutex
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const testNode = "node-1"

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	// The test node is not the cluster's own node, so it would wait MinReady
	// before taking replicas. The harness wants them placed at once.
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertNode(ctx, store.Node{ID: testNode, Name: "test", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewSessions(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.NewAuthenticator(auth.HashToken(tok), sessions)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{store: st, rt: &fakeRuntime{}, token: tok, logs: &syncBuffer{}}
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.srv, err = New(Config{Store: st, Runtime: h.rt, NodeID: testNode, Auth: a, Logger: log,
		Trigger: func() { h.mu.Lock(); h.trigger++; h.mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.srv.CloseStreams)
	return h
}

// do sends a request with the admin token and returns the recorder.
func (h *harness) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status %d, want %d; body %s", rec.Code, code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	logStream := code == http.StatusOK && strings.HasPrefix(ct, "text/plain")
	if !logStream && !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q, want JSON; body %q", ct, rec.Body.String())
	}
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error == "" {
		t.Fatalf("error body %q is not {\"error\": ...}: %v", rec.Body.String(), err)
	}
	return e.Error
}

const nginx2 = `{"name":"web","image":"nginx:alpine","replicas":2,"env":{"SECRET_KEY":"s3cr3t-value"}}`

// TestEveryRouteNeedsToken enumerates the registered routes and checks that
// each one answers 401 without a token, with a malformed token and with a
// wrong token, except GET /healthz.
func TestEveryRouteNeedsToken(t *testing.T) {
	h := newHarness(t)
	pats := h.srv.Patterns()
	if len(pats) < 9 {
		t.Fatalf("only %d routes registered: %v", len(pats), pats)
	}
	other, _ := auth.NewToken()
	for _, p := range pats {
		method, path, ok := strings.Cut(p, " ")
		if !ok {
			t.Fatalf("pattern %q has no method", p)
		}
		path = strings.ReplaceAll(path, "{name}", "web")
		path = strings.ReplaceAll(path, "{id}", testNode)
		for _, hdr := range []string{"", "Bearer nope", "Bearer " + other, "Basic " + h.token} {
			req := httptest.NewRequest(method, path, strings.NewReader(nginx2))
			if hdr != "" {
				req.Header.Set("Authorization", hdr)
			}
			rec := httptest.NewRecorder()
			h.srv.Handler().ServeHTTP(rec, req)
			if p == "GET /healthz" {
				if rec.Code != http.StatusOK {
					t.Errorf("%s with %q: %d, want 200", p, hdr, rec.Code)
				}
				continue
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s with %q: %d, want 401", p, hdr, rec.Code)
				continue
			}
			if got := errorOf(t, rec); got != "unauthorized" {
				t.Errorf("%s: error %q", p, got)
			}
		}
	}
	// Nothing was created by the unauthenticated PUT and POST.
	if apps, _ := h.store.ListApps(context.Background()); len(apps) != 0 {
		t.Fatalf("unauthenticated requests created %d apps", len(apps))
	}
	// Nor was the node removed by the unauthenticated DELETE.
	if removed, _ := h.store.IsNodeRemoved(context.Background(), testNode); removed {
		t.Fatal("an unauthenticated request removed a node")
	}
	// Unknown paths and methods also need the token.
	for _, mp := range [][2]string{{"GET", "/v1/nope"}, {"PATCH", "/v1/apps/web"}, {"HEAD", "/healthz"}, {"POST", "/healthz"}} {
		req := httptest.NewRequest(mp[0], mp[1], nil)
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: %d, want 401", mp[0], mp[1], rec.Code)
		}
	}
}

func TestAppLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	rec := h.do(t, "PUT", "/v1/apps/web", nginx2)
	wantStatus(t, rec, http.StatusCreated)
	var pr PutResult
	if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil {
		t.Fatal(err)
	}
	if !pr.Created || pr.App.Name != "web" || pr.App.Replicas != 2 || pr.App.Generation != 1 {
		t.Fatalf("put result %+v", pr)
	}

	// The same spec again is an update, and keeps the generation.
	rec = h.do(t, "PUT", "/v1/apps/web", nginx2)
	wantStatus(t, rec, http.StatusOK)
	_ = json.Unmarshal(rec.Body.Bytes(), &pr)
	if pr.Created || pr.App.Generation != 1 {
		t.Fatalf("second put %+v", pr)
	}

	// POST takes the name from the body.
	rec = h.do(t, "POST", "/v1/apps", `{"name":"api","image":"ghcr.io/x/api:2"}`)
	wantStatus(t, rec, http.StatusCreated)

	rec = h.do(t, "GET", "/v1/apps", "")
	wantStatus(t, rec, http.StatusOK)
	var list []AppView
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "api" || list[1].Name != "web" {
		t.Fatalf("list %+v", list)
	}

	rec = h.do(t, "GET", "/v1/apps/web", "")
	wantStatus(t, rec, http.StatusOK)
	var v AppView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Spec.Image != "nginx:alpine" || v.Spec.Env["SECRET_KEY"] != "s3cr3t-value" {
		t.Fatalf("get %+v", v)
	}

	// Status before the node reports: both replicas placed and pending.
	rec = h.do(t, "GET", "/v1/apps/web/status", "")
	wantStatus(t, rec, http.StatusOK)
	var as AppStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &as)
	if as.Desired != 2 || as.Running != 0 || len(as.Replicas) != 2 || as.Replicas[0].State != "pending" || as.Replicas[0].NodeID != testNode {
		t.Fatalf("status %+v", as)
	}

	// The node reports one running and one exited container.
	if err := h.store.ReportStatus(ctx, testNode, []store.Status{
		{Name: "hivemind-web-0-g1", ContainerID: "c0", App: "web", Replica: 0, Generation: 1, State: "running"},
		{Name: "hivemind-web-1-g1", ContainerID: "c1", App: "web", Replica: 1, Generation: 1, State: "exited", Error: "exit 1"},
	}); err != nil {
		t.Fatal(err)
	}
	rec = h.do(t, "GET", "/v1/status", "")
	wantStatus(t, rec, http.StatusOK)
	var all []AppStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &all)
	if len(all) != 2 || all[1].Name != "web" || all[1].Running != 1 || all[1].Replicas[1].State != "exited" || all[1].Replicas[1].Error != "exit 1" {
		t.Fatalf("status %+v", all)
	}

	rec = h.do(t, "POST", "/v1/apps/web/scale", `{"replicas":3}`)
	wantStatus(t, rec, http.StatusOK)
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Replicas != 3 || v.Generation != 1 {
		t.Fatalf("scale %+v", v)
	}
	if reps, _ := h.store.Replicas(ctx, "web"); len(reps) != 3 {
		t.Fatalf("%d replicas after scale", len(reps))
	}

	rec = h.do(t, "DELETE", "/v1/apps/web", "")
	wantStatus(t, rec, http.StatusAccepted)
	rec = h.do(t, "GET", "/v1/apps/web", "")
	wantStatus(t, rec, http.StatusNotFound)
	errorOf(t, rec)
	rec = h.do(t, "DELETE", "/v1/apps/web", "")
	wantStatus(t, rec, http.StatusNotFound)

	h.mu.Lock()
	n := h.trigger
	h.mu.Unlock()
	if n != 5 { // put, put, post, scale, delete
		t.Fatalf("agent triggered %d times, want 5", n)
	}

	// The env value and the token never reach the log.
	out := h.logs.String()
	if !strings.Contains(out, "http request") {
		t.Fatalf("no request log: %q", out)
	}
	for _, secret := range []string{"s3cr3t-value", h.token, h.token[len(auth.TokenPrefix):]} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains a secret:\n%s", out)
		}
	}
}

func TestBadInput(t *testing.T) {
	h := newHarness(t)
	big := `{"name":"web","image":"nginx","command":["` + strings.Repeat("a", MaxBodyBytes) + `"]}`
	cases := []struct {
		name, method, path, body string
		code                     int
		want                     string
	}{
		{"unknown field", "PUT", "/v1/apps/web", `{"name":"web","image":"nginx","imagee":"x"}`, 400, `unknown field "imagee"`},
		{"nested unknown field", "PUT", "/v1/apps/web", `{"image":"nginx","ports":[{"host":1,"container":2,"hots":3}]}`, 400, "unknown field"},
		{"oversized body", "PUT", "/v1/apps/web", big, 413, "larger than"},
		{"oversized post", "POST", "/v1/apps", big, 413, "larger than"},
		{"invalid spec", "PUT", "/v1/apps/web", `{"image":"nginx","replicas":-1}`, 400, "replicas"},
		{"bad image", "POST", "/v1/apps", `{"name":"web","image":"Nginx:"}`, 400, "image"},
		{"missing name", "POST", "/v1/apps", `{"image":"nginx"}`, 400, "name"},
		{"name mismatch", "PUT", "/v1/apps/web", `{"name":"api","image":"nginx"}`, 400, "path names"},
		{"bad path name", "PUT", "/v1/apps/Web", `{"image":"nginx"}`, 400, "app name"},
		{"trailing data", "PUT", "/v1/apps/web", `{"image":"nginx"} {"x":1}`, 400, "after the first value"},
		{"not JSON", "PUT", "/v1/apps/web", `image: nginx`, 400, "invalid JSON"},
		{"empty body", "PUT", "/v1/apps/web", ``, 400, "empty"},
		{"array body", "POST", "/v1/apps", `[1]`, 400, "invalid JSON"},
		{"scale unknown field", "POST", "/v1/apps/web/scale", `{"replica":2}`, 400, "unknown field"},
		{"scale zero", "POST", "/v1/apps/web/scale", `{"replicas":0}`, 400, "between 1"},
		{"scale too many", "POST", "/v1/apps/web/scale", `{"replicas":101}`, 400, "between 1"},
		{"scale missing app", "POST", "/v1/apps/nope/scale", `{"replicas":2}`, 404, "not found"},
		{"status missing app", "GET", "/v1/apps/nope/status", ``, 404, "not found"},
		{"logs missing app", "GET", "/v1/apps/nope/logs", ``, 404, "not found"},
		{"logs bad tail", "GET", "/v1/apps/web/logs?tail=-1", ``, 400, "tail"},
		{"logs bad replica", "GET", "/v1/apps/web/logs?replica=x", ``, 400, "replica"},
		{"logs bad follow", "GET", "/v1/apps/web/logs?follow=maybe", ``, 400, "follow"},
		{"unknown route", "GET", "/v1/nope", ``, 404, "not found"},
		{"wrong method", "PATCH", "/v1/apps/web", ``, 405, "method not allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := h.do(t, c.method, c.path, c.body)
			wantStatus(t, rec, c.code)
			if got := errorOf(t, rec); !strings.Contains(got, c.want) {
				t.Fatalf("error %q does not contain %q", got, c.want)
			}
		})
	}
	if apps, _ := h.store.ListApps(context.Background()); len(apps) != 0 {
		t.Fatalf("bad requests created %d apps", len(apps))
	}
	// The body of an error is never echoed whole, and a big body is not logged.
	if strings.Contains(h.logs.String(), strings.Repeat("a", 100)) {
		t.Fatal("request body reached the log")
	}
}

func TestLogs(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", nginx2), http.StatusCreated)

	rec := h.do(t, "GET", "/v1/apps/web/logs?replica=1", "")
	wantStatus(t, rec, http.StatusNotFound)
	if got := errorOf(t, rec); !strings.Contains(got, "no container") {
		t.Fatalf("error %q", got)
	}

	lbl := func(rep, gen string) map[string]string {
		return map[string]string{runtime.LabelApp: "web", runtime.LabelReplica: rep, runtime.LabelGeneration: gen}
	}
	h.rt.containers = []runtime.Container{
		{ID: "old", Name: "hivemind-web-1-g1", Labels: lbl("1", "1"), State: "exited"},
		{ID: "new", Name: "hivemind-web-1-g2", Labels: lbl("1", "2"), State: "running"},
		{ID: "zero", Name: "hivemind-web-0-g2", Labels: lbl("0", "2"), State: "running"},
	}
	h.rt.logLines = []string{"line one", "line two"}
	rec = h.do(t, "GET", "/v1/apps/web/logs?replica=1&tail=5", "")
	wantStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "line one\nline two\n" {
		t.Fatalf("body %q", rec.Body.String())
	}
	if h.rt.lastID != "new" || h.rt.lastOpts.Tail != 5 || h.rt.lastOpts.Follow {
		t.Fatalf("logs called with %q %+v", h.rt.lastID, h.rt.lastOpts)
	}
	rec = h.do(t, "GET", "/v1/apps/web/logs", "")
	wantStatus(t, rec, http.StatusOK)
	if h.rt.lastID != "zero" {
		t.Fatalf("default replica read %q", h.rt.lastID)
	}

	h.rt.listErr = &runtime.Error{Op: "list", Err: runtime.ErrUnavailable}
	rec = h.do(t, "GET", "/v1/apps/web/logs", "")
	wantStatus(t, rec, http.StatusServiceUnavailable)
}

// TestLogsFollowOutlivesWriteTimeout runs a real HTTP server with a short
// WriteTimeout and checks that a followed stream keeps delivering lines past
// it, and that CloseStreams ends the stream.
func TestLogsFollowOutlivesWriteTimeout(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", nginx2), http.StatusCreated)
	h.rt.containers = []runtime.Container{{ID: "c0", Name: "hivemind-web-0-g1", State: "running",
		Labels: map[string]string{runtime.LabelApp: "web", runtime.LabelReplica: "0", runtime.LabelGeneration: "1"}}}
	const n = 8
	for i := range n {
		h.rt.logLines = append(h.rt.logLines, fmt.Sprintf("line %d", i))
	}
	h.rt.logEvery = 100 * time.Millisecond

	ts := httptest.NewUnstartedServer(h.srv.Handler())
	ts.Config = NewHTTPServer(h.srv.Handler(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts.Config.WriteTimeout = 300 * time.Millisecond
	ts.Start()
	defer ts.Close()

	// A plain request still gets the timeout: an unfollowed stream that runs
	// longer than WriteTimeout is cut short.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/apps/web/logs", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Count(string(b), "\n") >= n {
			t.Fatalf("unfollowed stream outlived the write timeout: %q", b)
		}
	}

	req, _ = http.NewRequest("GET", ts.URL+"/v1/apps/web/logs?follow=true", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for i := range n {
		if !sc.Scan() {
			t.Fatalf("stream ended after %d lines: %v", i, sc.Err())
		}
		if sc.Text() != fmt.Sprintf("line %d", i) {
			t.Fatalf("line %d is %q", i, sc.Text())
		}
	}
	done := make(chan struct{})
	go func() {
		for sc.Scan() {
		}
		close(done)
	}()
	h.srv.CloseStreams()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CloseStreams did not end the followed stream")
	}
}

func TestHealthz(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestNewRequiresAuth(t *testing.T) {
	if _, err := New(Config{Runtime: &fakeRuntime{}, NodeID: "n"}); err == nil {
		t.Fatal("New accepted a config without a store and authenticator")
	}
}

func TestHTTPServerTimeouts(t *testing.T) {
	s := NewHTTPServer(http.NotFoundHandler(), slog.Default())
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.IdleTimeout <= 0 || s.MaxHeaderBytes <= 0 {
		t.Fatalf("missing timeout: %+v", s)
	}
}

// A spec change shows an unfinished rollout in status until every replica
// runs the new generation and is ready.
func TestStatusShowsRollout(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", `{"image":"nginx:1.27","replicas":1}`), http.StatusCreated)
	report := func(gen int) {
		t.Helper()
		name := fmt.Sprintf("hivemind-web-0-g%d", gen)
		if err := h.store.ReportStatus(ctx, testNode, []store.Status{
			{Name: name, ContainerID: name, App: "web", Replica: 0, Generation: gen, State: "running", Ready: true},
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.AdvanceRollouts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	status := func() AppStatus {
		t.Helper()
		rec := h.do(t, "GET", "/v1/apps/web/status", "")
		wantStatus(t, rec, http.StatusOK)
		var as AppStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &as); err != nil {
			t.Fatal(err)
		}
		return as
	}
	report(1)
	if as := status(); as.Rollout != nil {
		t.Fatalf("rollout before any change: %+v", as.Rollout)
	}

	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", `{"image":"nginx:1.28","replicas":1}`), http.StatusOK)
	report(1)
	as := status()
	if as.Rollout == nil || as.Rollout.Target != 2 || as.Rollout.State != "in_progress" {
		t.Fatalf("rollout during the update: %+v", as.Rollout)
	}

	report(2)
	if as := status(); as.Rollout != nil {
		t.Fatalf("rollout after every replica moved: %+v", as.Rollout)
	}
}

// Restart count, health and crash-loop state reach the status API.
func TestStatusShowsHealthAndRestarts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", `{"image":"nginx:1.27","replicas":2}`), http.StatusCreated)
	next := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := h.store.ReportStatus(ctx, testNode, []store.Status{
		{Name: "hivemind-web-0-g1", ContainerID: "c0", App: "web", Replica: 0, Generation: 1, State: "running",
			Health: "healthy", Ready: true, RestartCount: 1},
		{Name: "hivemind-web-1-g1", ContainerID: "c1", App: "web", Replica: 1, Generation: 1, State: "exited",
			RestartCount: 4, CrashLooping: true, NextRestart: next},
	}); err != nil {
		t.Fatal(err)
	}
	rec := h.do(t, "GET", "/v1/apps/web/status", "")
	wantStatus(t, rec, http.StatusOK)
	var as AppStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &as); err != nil {
		t.Fatal(err)
	}
	r0, r1 := as.Replicas[0], as.Replicas[1]
	if r0.Health != "healthy" || !r0.Ready || r0.Restarts != 1 || r0.CrashLooping || r0.NextRestart != nil {
		t.Fatalf("replica 0: %+v", r0)
	}
	if r1.Ready || r1.Restarts != 4 || !r1.CrashLooping || r1.NextRestart == nil || !r1.NextRestart.Equal(next) {
		t.Fatalf("replica 1: %+v", r1)
	}
}
