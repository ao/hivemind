package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

// A log response says in its trailer why it ended: "done" when the log
// ended, "shutdown" when the server closed the stream to shut down. A
// client can tell the two apart; without it, both are a clean end of body.
func TestLogTrailerSaysWhyTheStreamEnded(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", nginx2), http.StatusCreated)
	h.rt.containers = []runtime.Container{{ID: "c0", Name: "hivemind-web-0-g1", State: "running",
		Labels: map[string]string{runtime.LabelApp: "web", runtime.LabelReplica: "0", runtime.LabelGeneration: "1"}}}
	h.rt.logLines = []string{"one", "two"}
	ts := httptest.NewUnstartedServer(h.srv.Handler())
	ts.Config = NewHTTPServer(h.srv.Handler(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts.Start()
	defer ts.Close()
	get := func(q string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/apps/web/logs"+q, nil)
		req.Header.Set("Authorization", "Bearer "+h.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := get("")
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(b) != "one\ntwo\n" {
		t.Fatalf("body %q, %v", b, err)
	}
	if got := resp.Trailer.Get(LogEndTrailer); got != LogEndDone {
		t.Fatalf("trailer of a finished log %q, want %q", got, LogEndDone)
	}

	resp = get("?follow=true")
	defer resp.Body.Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	h.srv.CloseStreams()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("body of a stream the server closed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseStreams did not end the followed stream")
	}
	if got := resp.Trailer.Get(LogEndTrailer); got != LogEndShutdown {
		t.Fatalf("trailer of a stream ended by a shutdown %q, want %q", got, LogEndShutdown)
	}
}
