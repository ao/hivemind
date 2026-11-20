package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// lostLeadership is a store.Replicator whose writes fail as a server's do
// when the leadership changes: before they start (ErrNotLeader), or once
// they are in the log (ErrOutcomeUnknown).
type lostLeadership struct {
	notLeader atomic.Bool
	unknown   atomic.Bool
}

func (l *lostLeadership) CanWrite() (uint64, error) {
	if l.notLeader.Load() {
		return 0, fmt.Errorf("no leader: this server reaches 1 of the 3 servers, and a write needs 2: %w", store.ErrNotLeader)
	}
	return 1, nil
}

func (l *lostLeadership) Replicate(context.Context, uint64, []byte) error {
	if l.unknown.Load() {
		return fmt.Errorf("%w: leadership lost while committing log", store.ErrOutcomeUnknown)
	}
	return nil
}

// TestNodeWriteLostToALeaderChangeIs503: a node's report or lease renewal
// that the store refuses because this server no longer leads gets the
// API's 503, marked no leader so that the worker tries another server, and
// one whose outcome is unknown a 503 that says so; both are warnings. In
// the ninth soak both were 500 "internal error" and an error line on the
// old leader at every change of leader.
func TestNodeWriteLostToALeaderChangeIs503(t *testing.T) {
	var logs lockedBuffer
	fx := newFixtureWith(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	id, cert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	c := fx.nodeClient(cert)
	fx.registerNode(c, id)
	repl := &lostLeadership{}
	fx.st.SetReplicator(repl)
	status, _ := json.Marshal(nodeapi.StatusFromStore(nil))

	send := func(path string, body []byte) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, fx.nodeURL(path), strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		if body == nil {
			req.Body = http.NoBody
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var e nodeapi.ErrorBody
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, resp.Header, e.Error
	}

	repl.notLeader.Store(true)
	for _, p := range []struct {
		path string
		body []byte
	}{{nodeapi.StatusPath(id), status}, {nodeapi.LeasePath(id), nil}} {
		code, h, msg := send(p.path, p.body)
		if code != http.StatusServiceUnavailable || h.Get(nodeapi.NoLeaderHeader) == "" || h.Get("Retry-After") != "1" ||
			!strings.Contains(msg, "no leader: this server reaches 1 of the 3 servers") {
			t.Fatalf("%s without a leader: %d, no-leader mark %q, Retry-After %q, %q", p.path, code, h.Get(nodeapi.NoLeaderHeader),
				h.Get("Retry-After"), msg)
		}
	}

	repl.notLeader.Store(false)
	repl.unknown.Store(true)
	code, h, msg := send(nodeapi.StatusPath(id), status)
	if code != http.StatusServiceUnavailable || h.Get(nodeapi.NoLeaderHeader) != "" ||
		!strings.Contains(msg, "may or may not have been applied: leadership lost while committing log: check, and try again if it is not") {
		t.Fatalf("a report whose outcome is unknown: %d, no-leader mark %q, %q", code, h.Get(nodeapi.NoLeaderHeader), msg)
	}

	out := logs.String()
	if strings.Contains(out, "level=ERROR") || strings.Count(out, `level=WARN msg="a node's request was refused: this server does not lead now"`) != 2 ||
		strings.Count(out, `level=WARN msg="a node's write may or may not have been applied`) != 1 {
		t.Fatalf("the server's log:\n%s", out)
	}

	// The worker's client moves on from the marked 503, and takes the
	// other for what it says.
	repl.unknown.Store(false)
	repl.notLeader.Store(true)
	nc, err := nodeapi.NewClient("https://"+fx.nodeAddr, c.Transport.(*http.Transport).TLSClientConfig)
	if err != nil {
		t.Fatal(err)
	}
	var ue *nodeapi.UnreachableError
	if err := nc.RenewLease(context.Background(), id); err == nil || !errors.As(err, &ue) {
		t.Fatalf("the node client's renewal without a leader: %v, want it taken as no server reachable", err)
	}
}
