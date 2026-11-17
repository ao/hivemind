package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/server"
)

// putPending is a put reply for web with the given pending replicas, all
// with reason, and count.
func putPending(replicas int, reason string, stuck bool, count int, idx ...int) string {
	res := server.PutResult{App: server.AppView{Name: "web", Generation: 1, Replicas: replicas}, Created: true, PendingCount: count}
	for _, i := range idx {
		res.Pending = append(res.Pending, server.PendingReplica{Replica: i, Reason: reason, Stuck: stuck})
	}
	b, _ := json.Marshal(res)
	return string(b)
}

const heldBy = "host port 127.0.0.1:8080/tcp is in use on every eligible node; held by app api"

// deploy warns on standard error of each replica the put left with no node,
// one line for each reason, and exits 0: the spec is deployed.
func TestDeployWarnsOfPendingReplicas(t *testing.T) {
	waitAPI(t, putPending(3, heldBy, true, 2, 1, 2), status(1, 3, "", ready0))
	out, errOut, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--replicas", "3", "--port", "8080:80")
	if err != nil || out != "web: created, generation 1, 3 replicas\n" {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	if want := "web: warning: replicas 1, 2 have no node: " + heldBy + "\n"; errOut != want {
		t.Fatalf("standard error:\n%q\nwant:\n%q", errOut, want)
	}
}

// Past the reply's list, one line counts the rest; text from the server is
// printed escaped.
func TestDeployWarningIsBoundedAndEscaped(t *testing.T) {
	waitAPI(t, putPending(14, "no node has 1Gi of memory free\x1b[2J", false, 13, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10), status(1, 14, "", ready0))
	_, errOut, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--replicas", "14")
	want := `web: warning: replicas 1, 2, 3, 4, 5, 6, 7, 8, 9, 10 have no node: no node has 1Gi of memory free\x1b[2J` + "\n" +
		"web: warning: 3 more replicas have no node; hivemind status web shows them\n"
	if err != nil || errOut != want {
		t.Fatalf("deploy: %v\nstandard error:\n%q\nwant:\n%q", err, errOut, want)
	}
}

// With --wait, a replica whose reason does not change by itself fails the
// command at once, with the reason, rather than at the timeout.
func TestDeployWaitStopsAtAStuckReplica(t *testing.T) {
	waitAPI(t, putPending(3, heldBy, true, 2, 1, 2), status(1, 3, "", noNode))
	out, errOut, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--replicas", "3", "--wait", "--timeout", "1h")
	want := "web: replica 1 has no node, and will not get one by itself: " + heldBy +
		"; the spec stays deployed; 2 replicas have no node, and hivemind status web shows them"
	if err == nil || err.Error() != want || exitCode(err) != 1 {
		t.Fatalf("deploy --wait: %v (exit %d)\nwant %q", err, exitCode(err), want)
	}
	if out != "web: created, generation 1, 3 replicas\n" || errOut != "" {
		t.Fatalf("output:\n%s\nstandard error:\n%s", out, errOut)
	}
}

// A pending replica that may get a node later is a warning, and --wait
// waits for it as before.
func TestDeployWaitWaitsForAReplicaThatIsNotStuck(t *testing.T) {
	waitAPI(t, putPending(1, "no node has been ready for 30s", false, 1, 0), status(1, 1, "", noNode), status(1, 1, "", ready0))
	out, errOut, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--wait")
	if err != nil || !strings.HasSuffix(out, "web: 1 replica running and ready\n") {
		t.Fatalf("deploy --wait: %v\n%s", err, out)
	}
	if want := "web: warning: replica 0 has no node: no node has been ready for 30s\n"; errOut != want {
		t.Fatalf("standard error: %q, want %q", errOut, want)
	}
}

// scale warns as deploy does, and its --wait stops at a stuck replica too.
func TestScaleWarnsOfPendingReplicas(t *testing.T) {
	reply := `{"name":"web","generation":1,"replicas":2,"pending":[{"replica":1,"reason":"` + heldBy + `","stuck":true}],"pending_count":1}`
	waitAPI(t, reply, status(1, 2, "", ready0))
	out, errOut, err := run(t, "", "scale", "web", "2")
	if err != nil || out != "web: 2 replicas\n" || errOut != "web: warning: replica 1 has no node: "+heldBy+"\n" {
		t.Fatalf("scale: %v\n%s\nstandard error: %q", err, out, errOut)
	}
	_, _, err = run(t, "", "scale", "web", "2", "--wait")
	if want := "web: replica 1 has no node, and will not get one by itself: " + heldBy + "; the spec stays deployed"; err == nil || err.Error() != want {
		t.Fatalf("scale --wait: %v, want %q", err, want)
	}
	out, _, err = run(t, "", "scale", "web", "2", "--json")
	var sr server.ScaleResult
	if err != nil || json.Unmarshal([]byte(out), &sr) != nil || sr.Name != "web" || sr.PendingCount != 1 || !sr.Pending[0].Stuck {
		t.Fatalf("scale --json: %v\n%s", err, out)
	}
}

// Against the real API: the reply names the app that holds the port, deploy
// warns and exits 0, and deploy --wait fails at once.
func TestDeployWarningFromTheServer(t *testing.T) {
	composeAPI(t)
	if _, errOut, err := run(t, "", "deploy", "--name", "api", "--image", "nginx:alpine", "--port", "8080:80"); err != nil || errOut != "" {
		t.Fatalf("deploy api: %v, %q", err, errOut)
	}
	out, errOut, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--port", "8080:80")
	if err != nil || out != "web: created, generation 1, 1 replica\n" || errOut != "web: warning: replica 0 has no node: "+heldBy+"\n" {
		t.Fatalf("deploy web: %v\n%s\nstandard error: %q", err, out, errOut)
	}
	_, _, err = run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--port", "8080:80", "--wait")
	if want := "web: replica 0 has no node, and will not get one by itself: " + heldBy + "; the spec stays deployed"; err == nil || err.Error() != want || exitCode(err) != 1 {
		t.Fatalf("deploy web --wait: %v, want %q", err, want)
	}
}
