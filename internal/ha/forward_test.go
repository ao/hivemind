package ha

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestForwardFailureSaysWhetherToTryElsewhere: a request the leader did not
// answer is marked no leader, for a client to send to another server, only
// when the leader cannot have acted on it: when it could not be sent, and
// for a GET. A write that was sent and got no answer may have been applied,
// and is not marked (the eighth soak's seventh finding: every such answer
// was marked, writes included).
func TestForwardFailureSaysWhetherToTryElsewhere(t *testing.T) {
	plain := func(ctx context.Context, _, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	n := &Node{fwdClient: &http.Client{Transport: &http.Transport{DialTLSContext: plain}}}

	// A leader that reads the request and closes the connection unanswered,
	// as one whose process stops.
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			c.Close()
		}
	}))
	defer cut.Close()
	// A leader where nothing listens.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := ln.Addr().String()
	ln.Close()

	for _, c := range []struct {
		method, addr string
		marked       bool
		says         string
	}{
		{http.MethodPost, gone, true, "did not answer this server"},
		{http.MethodPut, gone, true, "did not answer this server"},
		{http.MethodGet, cut.Listener.Addr().String(), true, "did not answer this server"},
		{http.MethodPost, cut.Listener.Addr().String(), false, "may or may not have been applied"},
		{http.MethodDelete, cut.Listener.Addr().String(), false, "may or may not have been applied"},
	} {
		var body io.Reader
		if c.method == http.MethodPost || c.method == http.MethodPut {
			body = strings.NewReader(`{"name":"web"}`)
		}
		rec := httptest.NewRecorder()
		n.forwardTo(KindAPI, "node-l", c.addr, rec, httptest.NewRequest(c.method, "/v1/apps", body))
		marked := rec.Header().Get(HeaderNoLeader) != ""
		if rec.Code != http.StatusServiceUnavailable || marked != c.marked || !strings.Contains(rec.Body.String(), c.says) {
			t.Errorf("%s to a leader at %s: %d, marked %v, %s; want 503, marked %v, %q", c.method, c.addr, rec.Code, marked,
				strings.TrimSpace(rec.Body.String()), c.marked, c.says)
		}
	}
}
