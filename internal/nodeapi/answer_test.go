package nodeapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// OnAnswer's function runs after every reply, whatever its status, and not
// for a request that got none.
func TestOnAnswerRunsForEveryReply(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"the store is read-only"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	c, err := NewClient(ts.URL, ts.Client().Transport.(*http.Transport).TLSClientConfig)
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	c.OnAnswer(func() { n.Add(1) })
	ctx := context.Background()
	if _, err := c.AssignmentsForNode(ctx, "node-abc123"); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportStatus(ctx, "node-abc123", nil); err == nil {
		t.Fatal("the refusal was not an error")
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("%d calls after a reply and a refusal, want 2", got)
	}
	ts.Close()
	if _, err := c.AssignmentsForNode(ctx, "node-abc123"); err == nil {
		t.Fatal("a closed server answered")
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("%d calls after a request with no reply, want still 2", got)
	}
	c.OnAnswer(nil)
}
