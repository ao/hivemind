package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/ha"
	"github.com/ao/hivemind/internal/server"
)

// TestSchemaWaits: the step to the next schema version waits for every
// server that does not run it, each named with why: no answer, a build of
// 2.3.0 by the version its node sent, or a build whose versions stop
// before it. It waits for none once each runs it.
func TestSchemaWaits(t *testing.T) {
	builds := []ha.ServerBuild{
		{ID: "node-a", Answered: true, Build: ha.BuildInfo{Version: "v2.4.0", SchemaOldest: 18, SchemaNewest: 19, Schema: 18}},
		{ID: "node-b", Answered: true, Legacy: true, Build: ha.BuildInfo{SchemaOldest: 18, SchemaNewest: 18, Schema: 18}},
		{ID: "node-c", Err: "dial tcp 10.0.0.7:4486: connect: connection refused"},
		{ID: "node-d", Answered: true, Build: ha.BuildInfo{Version: "v2.4.0-rc1", SchemaOldest: 18, SchemaNewest: 18, Schema: 18}},
	}
	got := schemaWaits(builds, 19, map[string]string{"node-b": "v2.3.0"})
	for _, want := range []string{"node-b (v2.3.0, which runs schema version 18 alone)", "node-c (no answer: dial tcp",
		"node-d (v2.4.0-rc1, which runs schema versions 18 to 18)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("schemaWaits: %q, without %q", got, want)
		}
	}
	if strings.Contains(got, "node-a") {
		t.Fatalf("schemaWaits names a server that runs the next version: %q", got)
	}
	if got := schemaWaits(builds[:1], 19, nil); got != "" {
		t.Fatalf("with every server on the next version: %q", got)
	}
	if got := schemaWaits(nil, 19, nil); got == "" {
		t.Fatal("no builds at all: the step went ahead")
	}
}

// TestPrintServersVersions: hivemind servers shows each server's release,
// marked when it is the version its node sent, and the store's schema with
// what its step waits for.
func TestPrintServersVersions(t *testing.T) {
	now := time.Now()
	rep := server.ServersReply{Leader: "node-a", AnsweredBy: "node-a", Schema: 18, SchemaNext: 19, SchemaWaits: "node-b (v2.3.0, which runs schema version 18 alone)",
		Servers: []server.ServerEntry{
			{ID: "node-a", Name: "hm-1", Advertise: "10.0.0.5", NodePort: 4484, Role: "leader", Version: "v2.4.0", VersionFrom: "build", SchemaOldest: 18, SchemaNewest: 19},
			{ID: "node-b", Name: "hm-2", Advertise: "10.0.0.6", NodePort: 4484, Role: "follower", Version: "v2.3.0", VersionFrom: "node"},
			{ID: "node-c", Name: "hm-3", Advertise: "10.0.0.7", NodePort: 4484, Role: "follower"},
		}}
	var b bytes.Buffer
	printServers(&b, rep, now)
	out := b.String()
	for _, want := range []string{"VERSION", "v2.4.0", "v2.3.0 (node)", "Store schema 18; it moves to 19 once every server runs it: node-b (v2.3.0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("hivemind servers without %q:\n%s", want, out)
		}
	}
	lines := strings.Split(out, "\n")
	if f := strings.Fields(lines[3]); len(f) < 5 || f[0] != "node-c" || f[4] != "-" {
		t.Fatalf("a server with no version: %q", lines[3])
	}
	b.Reset()
	rep.SchemaWaits = ""
	printServers(&b, rep, now)
	if !strings.Contains(b.String(), "every server runs 19") {
		t.Fatalf("with nothing to wait for:\n%s", b.String())
	}
	b.Reset()
	rep.Schema, rep.SchemaNext = 0, 0
	printServers(&b, rep, now)
	if strings.Contains(b.String(), "schema") {
		t.Fatalf("a reply from a server that says no schema:\n%s", b.String())
	}
}
