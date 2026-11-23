package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/ha"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// fakeLeadership leads, or passes every request it gets to forwarded.
type fakeLeadership struct {
	leads     bool
	forwarded []string
}

func (f *fakeLeadership) IsLeader() bool { return f.leads }
func (f *fakeLeadership) Forward(kind string, w http.ResponseWriter, r *http.Request) {
	f.forwarded = append(f.forwarded, kind+" "+r.URL.Path)
	w.WriteHeader(http.StatusTeapot)
}

// TestForgedForwardingHeadersGetNothing: a worker that sends the
// forwarding headers to the leader's node listener, naming another node's
// certificate and another address, is served as itself: it still cannot
// read another node's assignments, and its own requests work as before.
func TestForgedForwardingHeadersGetNothing(t *testing.T) {
	l := &fakeLeadership{leads: true}
	holder := &HAHolder{}
	holder.Set(l)
	fx := newFixtureWith(t, func(c *Config) { c.HA = holder })
	idA, certA := fx.enroll(fx.issue(time.Hour))
	idB, certB := fx.enroll(fx.issue(time.Hour))
	a := fx.nodeClient(certA)
	fx.registerNode(a, idA)
	fx.registerNode(fx.nodeClient(certB), idB)
	forge := func(path string) int {
		req, err := http.NewRequest(http.MethodGet, fx.nodeURL(path), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(ha.HeaderKind, ha.KindNode)
		req.Header.Set(ha.HeaderChain, base64.StdEncoding.EncodeToString(certB.Leaf.Raw)+","+base64.StdEncoding.EncodeToString(fx.ca.Cert.Raw))
		req.Header.Set(ha.HeaderRemote, "10.1.2.3:4")
		req.Header.Set(ha.HeaderTLS, "1")
		resp, err := a.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := forge(nodeapi.AssignmentsPath(idB)); code != http.StatusForbidden {
		t.Fatalf("A, claiming B's certificate in the forwarding headers, reads B's assignments: %d, want 403", code)
	}
	if code := forge(nodeapi.AssignmentsPath(idA)); code != http.StatusOK {
		t.Fatalf("A reads its own assignments with forged headers: %d, want 200", code)
	}
	if len(l.forwarded) != 0 {
		t.Fatalf("the leader passed requests on: %v", l.forwarded)
	}
}

// TestFollowerListenersForward: a follower's node and join listeners pass
// every request to the leader.
func TestFollowerListenersForward(t *testing.T) {
	l := &fakeLeadership{}
	holder := &HAHolder{}
	fx := newFixtureWith(t, func(c *Config) { c.HA = holder })
	idA, certA := fx.enroll(fx.issue(time.Hour)) // a cluster of one yet: served here
	holder.Set(l)
	a := fx.nodeClient(certA)
	if code, _ := fx.send(a, http.MethodGet, fx.nodeURL(nodeapi.AssignmentsPath(idA)), nil); code != http.StatusTeapot {
		t.Fatalf("a follower's node listener answered %d itself", code)
	}
	if _, _, code, _ := fx.tryJoin(fx.issue(time.Hour)); code != http.StatusTeapot {
		t.Fatalf("a follower's join listener answered %d itself", code)
	}
	want := []string{ha.KindNode + " " + nodeapi.AssignmentsPath(idA), ha.KindJoin + " " + nodeapi.JoinPath}
	if strings.Join(l.forwarded, ";") != strings.Join(want, ";") {
		t.Fatalf("forwarded %v, want %v", l.forwarded, want)
	}
}

// joinServerFixture is a cluster of one with a ServerJoiner that records
// what it admits.
func joinServerFixture(t *testing.T, version string) (*fixture, *pki.Tokens, *[]string) {
	t.Helper()
	var admitted []string
	var tokens *pki.Tokens
	fx := newFixtureWith(t, func(c *Config) {
		tokens = pki.NewTokens(c.Store.ServerJoinTokens())
		c.ServerTokens = tokens
		c.Version = version
		c.JoinServer = func(_ context.Context, id string, req nodeapi.JoinServerRequest) (nodeapi.JoinServerReply, error) {
			admitted = append(admitted, id)
			return nodeapi.JoinServerReply{NodeID: id, Servers: []nodeapi.ServerInfo{{ID: id, Advertise: req.Advertise, NodePort: req.NodePort,
				RaftAddr: req.Advertise + ":4486"}}}, nil
		}
	})
	return fx, tokens, &admitted
}

func (fx *fixture) joinServer(req nodeapi.JoinServerRequest) (int, string) {
	fx.t.Helper()
	code, body := fx.post(fx.joinClient(fingerprintOf(fx.t, req.Token)), "https://"+fx.joinAddr+nodeapi.JoinServerPath, req)
	var e nodeapi.ErrorBody
	_ = json.Unmarshal(body, &e)
	return code, e.Error
}

// TestServerJoinChecks: a server's join takes a server's token only, and
// refuses a worker's; a worker's join refuses a server's token; each
// refusal of a token of the other role says what the token is and which
// to use, and leaves it unused; a joining server whose build does not run
// the store's schema version (here a request of 2.3.0, which runs 18
// alone), or with an address no other machine can reach, is refused
// before the token is used.
func TestServerJoinChecks(t *testing.T) {
	fx, tokens, admitted := joinServerFixture(t, "v2.3.0")
	serverTok, _, err := tokens.Issue(fx.ca.Fingerprint(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	workerTok := fx.issue(time.Hour)
	if code, msg := fx.joinServer(nodeapi.JoinServerRequest{Token: workerTok, Advertise: "10.0.0.6", NodePort: 4484, Version: "v2.4.0", SchemaOldest: store.SchemaOldest, SchemaNewest: store.SchemaVersion}); code != http.StatusUnauthorized ||
		msg != "the join token is a worker's: a server joins with a token from hivemind join-token --role server" {
		t.Fatalf("a server join with a worker's token: %d %s", code, msg)
	}
	if _, _, code, msg := fx.tryJoin(serverTok); code != http.StatusUnauthorized ||
		msg != "the join token is a server's: join this machine as a server with hivemind join --role server, or as a worker with a token from hivemind join-token" {
		t.Fatalf("a worker join with a server's token: %d %s", code, msg)
	}
	// The server's join that was refused did not use the worker's token.
	if _, _, code, msg := fx.tryJoin(workerTok); code != http.StatusOK {
		t.Fatalf("a worker join with the worker's token a server's join was refused: %d %s", code, msg)
	}
	if _, _, code, msg := fx.tryJoin(workerTok); code != http.StatusUnauthorized || msg != errJoinRefused {
		t.Fatalf("a worker's token used twice: %d %s, want the message for a used token", code, msg)
	}
	if code, msg := fx.joinServer(nodeapi.JoinServerRequest{Token: serverTok, Advertise: "10.0.0.6", NodePort: 4484, Version: "v2.3.0"}); code != http.StatusConflict ||
		!strings.Contains(msg, fmt.Sprintf("store is at schema version %d", store.SchemaVersion)) {
		t.Fatalf("a server join at another version: %d %s", code, msg)
	}
	if code, msg := fx.joinServer(nodeapi.JoinServerRequest{Token: serverTok, Advertise: "127.0.0.1", NodePort: 4484, Version: "v2.4.0", SchemaOldest: store.SchemaOldest, SchemaNewest: store.SchemaVersion}); code != http.StatusBadRequest {
		t.Fatalf("a server join advertising loopback: %d %s", code, msg)
	}
	if len(*admitted) != 0 {
		t.Fatalf("admitted %v before a valid join", *admitted)
	}
	// The worker's join that was refused did not use the server token, and
	// none of the refusals did.
	if code, msg := fx.joinServer(nodeapi.JoinServerRequest{Token: serverTok, Advertise: "10.0.0.6", NodePort: 4484, Version: "v2.4.0", SchemaOldest: store.SchemaOldest, SchemaNewest: store.SchemaVersion}); code != http.StatusOK {
		t.Fatalf("a valid server join: %d %s", code, msg)
	}
	if len(*admitted) != 1 {
		t.Fatalf("admitted %v", *admitted)
	}
	if code, _ := fx.joinServer(nodeapi.JoinServerRequest{Token: serverTok, Advertise: "10.0.0.7", NodePort: 4484, Version: "v2.4.0", SchemaOldest: store.SchemaOldest, SchemaNewest: store.SchemaVersion}); code != http.StatusUnauthorized {
		t.Fatalf("a server token used twice: %d", code)
	}
	if removed, _ := fx.st.IsNodeRemoved(context.Background(), (*admitted)[0]); removed {
		t.Fatal("the new server's ID is on the denylist")
	}
}

// TestServersHeader: a cluster of several names its servers on node API
// replies, and a cluster of one names none.
func TestServersHeader(t *testing.T) {
	fx := newFixture(t, nil)
	id, cert := fx.enroll(fx.issue(time.Hour))
	c := fx.nodeClient(cert)
	fx.registerNode(c, id)
	get := func() string {
		resp, err := c.Get(fx.nodeURL(nodeapi.AssignmentsPath(id)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get(nodeapi.ServersHeader)
	}
	if v := get(); v != "" {
		t.Fatalf("a cluster of one names servers: %q", v)
	}
	ctx := context.Background()
	for _, s := range []store.Server{{ID: "node-s1", Advertise: "10.0.0.5", NodePort: 4484, RaftAddr: "10.0.0.5:4486"},
		{ID: "node-s2", Advertise: "hm-2.example.internal", NodePort: 4490, RaftAddr: "hm-2.example.internal:4492"}} {
		if err := fx.st.PutServer(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(serversEvery + 100*time.Millisecond)
	if v := get(); v != "10.0.0.5:4484,hm-2.example.internal:4490" {
		t.Fatalf("servers header %q", v)
	}
}

// holdingLeadership is a follower whose leader holds each request passed
// to it until the request's context ends, as it holds a log poll.
type holdingLeadership struct{ started chan string }

func (h *holdingLeadership) IsLeader() bool { return false }
func (h *holdingLeadership) Forward(kind string, w http.ResponseWriter, r *http.Request) {
	h.started <- r.URL.Path
	<-r.Context().Done()
	w.WriteHeader(http.StatusServiceUnavailable)
}

// TestShutdownEndsForwardedLogPolls: a follower's listener that is
// passing a node's log poll to the leader stops in well under its 10 s
// wait for requests in flight, which every join --role server spent on its
// own node's poll in the eighth soak, logging "cluster shutdown: context
// deadline exceeded". Other requests passed on are left to finish.
func TestShutdownEndsForwardedLogPolls(t *testing.T) {
	holder := &HAHolder{}
	fx := newFixtureWith(t, func(c *Config) { c.HA = holder })
	id, cert := fx.enroll(fx.issue(time.Hour)) // a cluster of one yet
	h := &holdingLeadership{started: make(chan string, 1)}
	holder.Set(h)
	a := fx.nodeClient(cert)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := a.Get(fx.nodeURL(nodeapi.LogRequestsPath(id))); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case p := <-h.started:
		if p != nodeapi.LogRequestsPath(id) {
			t.Fatalf("passed on %s", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the log poll was not passed to the leader")
	}
	start := time.Now()
	if err := fx.c.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the listener took %s to stop with a log poll passed to the leader", d)
	}
	<-done

	for _, p := range []string{nodeapi.LogRequestsPath(id), nodeapi.Prefix + "/nodes/" + id + "/logs/streams/r1"} {
		if !isLogPath(p) {
			t.Fatalf("%s is a log path", p)
		}
	}
	for _, p := range []string{nodeapi.AssignmentsPath(id), nodeapi.Prefix + "/nodes/" + id + "/status", "/logs/requests"} {
		if isLogPath(p) {
			t.Fatalf("%s is not a log path", p)
		}
	}
}
