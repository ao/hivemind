package ha

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// TestNoLeaderForHeader: a 503 for no leader says how long the server has
// known none when it knows none, and nothing when it names a leader or
// does not know, so that a client given that one server can tell a lost
// majority from an election (the fifth acceptance run's eleventh finding).
func TestNoLeaderForHeader(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&NotLeaderError{Reachable: 1, Servers: 3, For: 75*time.Second + 900*time.Millisecond}, "75"},
		{fmt.Errorf("putting the app: %w", &NotLeaderError{Reachable: 2, Servers: 3}), "0"},
		{&NotLeaderError{Leader: "node-a", Addr: "10.0.0.5:4486", For: time.Minute}, ""},
		{errors.New("the leader is starting"), ""},
	} {
		rec := httptest.NewRecorder()
		WriteNotLeader(rec, c.err)
		if rec.Header().Get(HeaderNoLeader) == "" || rec.Header().Get(HeaderNoLeaderFor) != c.want {
			t.Errorf("%v: %s %q, %s %q; want %q", c.err, HeaderNoLeader, rec.Header().Get(HeaderNoLeader), HeaderNoLeaderFor,
				rec.Header().Get(HeaderNoLeaderFor), c.want)
		}
	}
}
