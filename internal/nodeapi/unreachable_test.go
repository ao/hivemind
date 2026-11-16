package nodeapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// A request that gets no reply, as when the server is down, is an
// UnreachableError, which the agent backs off on. A refusal from a server
// that answers is not.
func TestUnreachableServer(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"the store is read-only"}`))
	}))
	c, err := NewClient(ts.URL, ts.Client().Transport.(*http.Transport).TLSClientConfig)
	if err != nil {
		t.Fatal(err)
	}
	var u interface{ Unreachable() bool }
	_, err = c.AssignmentsForNode(context.Background(), "node-abc123")
	if err == nil || errors.As(err, &u) {
		t.Fatalf("a refusal: %v, want an error that is not unreachable", err)
	}

	ts.Close()
	_, err = c.AssignmentsForNode(context.Background(), "node-abc123")
	var ue *UnreachableError
	if !errors.As(err, &u) || !u.Unreachable() || !errors.As(err, &ue) {
		t.Fatalf("a server that is down: %v (%T), want an UnreachableError", err, err)
	}
	if !strings.HasPrefix(err.Error(), "calling the server: ") || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("a server that is down: %q, want the transport's error, connection refused", err)
	}
	if err := c.ReportStatus(context.Background(), "node-abc123", nil); !errors.As(err, &u) {
		t.Fatalf("a report to a server that is down: %v", err)
	}
}

// The log poll leaves a server that does not answer to the agent, which
// logs the outage once a minute: it logs its own retries at debug level. A
// refusal from a server that answers is still a warning.
func TestLogPollRetriesQuietlyWhileTheServerIsDown(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"the server is shutting down"}`))
	}))
	c, err := NewClient(ts.URL, ts.Client().Transport.(*http.Transport).TLSClientConfig)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	ls := &LogServer{Client: c, NodeID: "node-a", Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ls.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	waitFor(t, "the refusal's warning", func() bool { return strings.Contains(logs.String(), "level=WARN") })
	ts.Close()
	waitFor(t, "a retry against the closed server", func() bool {
		return strings.Contains(logs.String(), "level=DEBUG msg=\"polling the server for log requests; retrying\"")
	})
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "connection refused") {
			t.Fatalf("a retry against a server that is down was a warning:\n%s", logs)
		}
	}
	if !strings.Contains(logs.String(), "the server is shutting down") {
		t.Fatalf("the refusal was not logged:\n%s", logs)
	}
}
