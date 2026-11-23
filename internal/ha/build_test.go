package ha

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// TestBuildAnswers: a server of 2.4.0 answers with its build; a refusal,
// as 2.3.0 gives a request it does not know, is a build that runs schema
// version 18 alone; anything else is no answer.
func TestBuildAnswers(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	b := buildAnswer("node-a", http.StatusOK, []byte(`{"version":"v2.4.0","schema_oldest":18,"schema_newest":19,"schema":18}`), at)
	if !b.Answered || b.Legacy || b.Build.Version != "v2.4.0" || !b.RunsSchema(18) || !b.RunsSchema(19) || b.RunsSchema(20) {
		t.Fatalf("a 2.4.0 answer: %+v", b)
	}
	old := buildAnswer("node-b", http.StatusServiceUnavailable,
		[]byte(`{"error":"this server is not the leader; node-a at 10.0.0.5:4486 is"}`), at)
	if !old.Answered || !old.Legacy || !old.RunsSchema(store.HASchemaVersion) || old.RunsSchema(store.HASchemaVersion+1) {
		t.Fatalf("a 2.3.0 follower's refusal: %+v", old)
	}
	if b := buildAnswer("node-c", http.StatusBadRequest, []byte(`{"error":"unknown forwarded request"}`), at); !b.Legacy {
		t.Fatalf("a 2.3.0 leader's refusal: %+v", b)
	}
	for _, body := range []string{`not json`, `{"schema_oldest":0,"schema_newest":19}`, `{"schema_oldest":19,"schema_newest":18}`} {
		if b := buildAnswer("node-d", http.StatusOK, []byte(body), at); b.Answered || b.Err == "" || b.RunsSchema(18) {
			t.Fatalf("an answer that is not a build, %s: %+v", body, b)
		}
	}
	if (ServerBuild{ID: "node-e", Err: "dial tcp: connection refused"}).RunsSchema(18) {
		t.Fatal("a server that did not answer runs a schema")
	}
}

// TestSchemaStepOnThreeServers: three servers whose stores are at 2.3.0's
// schema version, as after a rolling upgrade's last restart. The leader
// asks each for its build over the server port, followers included; while
// one says its build runs 18 alone, the next version is not run by every
// server; once it runs both, the step through the log moves every store to
// 19, and the writes after it reach every server.
func TestSchemaStepOnThreeServers(t *testing.T) {
	c := newTestCluster(t, 3, store.WithHeldSchema())
	var only18 atomic.Bool
	only18.Store(true)
	lagging := c.servers[2]
	lagging.build = func() BuildInfo {
		if only18.Load() {
			return BuildInfo{Version: "v2.3.9", SchemaOldest: store.HASchemaVersion, SchemaNewest: store.HASchemaVersion, Schema: lagging.st.Schema()}
		}
		oldest, newest := lagging.st.SchemaRange()
		return BuildInfo{Version: "v2.4.0", SchemaOldest: oldest, SchemaNewest: newest, Schema: lagging.st.Schema()}
	}
	c.start()
	for _, s := range c.servers {
		if s.st.Schema() != store.HASchemaVersion {
			t.Fatalf("%s's store is at %d, want %d", s.id, s.st.Schema(), store.HASchemaVersion)
		}
	}
	l := c.leader()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	next := store.HASchemaVersion + 1
	builds := l.node.Builds(ctx)
	if len(builds) != 3 {
		t.Fatalf("builds of %d servers: %+v", len(builds), builds)
	}
	for _, b := range builds {
		switch {
		case !b.Answered:
			t.Fatalf("%s did not answer: %s", b.ID, b.Err)
		case b.ID == lagging.id && b.RunsSchema(next):
			t.Fatalf("%s runs %d: %+v", b.ID, next, b)
		case b.ID != lagging.id && !b.RunsSchema(next):
			t.Fatalf("%s does not run %d: %+v", b.ID, next, b)
		case b.Build.Schema != store.HASchemaVersion:
			t.Fatalf("%s says its store is at %d", b.ID, b.Build.Schema)
		}
	}

	only18.Store(false)
	for _, b := range l.node.Builds(ctx) {
		if !b.RunsSchema(next) {
			t.Fatalf("%s does not run %d once upgraded: %+v", b.ID, next, b)
		}
	}
	if v, err := l.st.StepSchema(ctx); err != nil || v != next {
		t.Fatalf("the step: %d %v", v, err)
	}
	c.waitFor(10*time.Second, "every store at the next version", func() bool {
		for _, s := range c.servers {
			if s.st.Schema() != next {
				return false
			}
		}
		return true
	})
	if _, err := l.st.PutApp(ctx, spec.App{Name: "after", Image: "busybox", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	c.converged("after:2,before:1")
	for _, b := range l.node.Builds(ctx) {
		if b.Build.Schema != next {
			t.Fatalf("%s says its store is at %d after the step", b.ID, b.Build.Schema)
		}
	}
}
