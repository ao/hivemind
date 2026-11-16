package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/server"
)

// waitAPI serves PUT /v1/apps/{name} and POST .../scale with reply, and GET
// .../status with statuses in turn, the last one for ever after. It sets
// the client's server and token, and shortens --wait's poll and settle.
func waitAPI(t *testing.T, reply string, statuses ...string) {
	t.Helper()
	isolate(t)
	poll, settle := waitPoll, waitSettle
	waitPoll, waitSettle = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { waitPoll, waitSettle = poll, settle })
	var mu sync.Mutex
	next := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.Write([]byte(reply))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		s := statuses[min(next, len(statuses)-1)]
		next++
		mu.Unlock()
		w.Write([]byte(s))
	}))
	t.Cleanup(ts.Close)
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, "test")
}

// status is GET /v1/apps/web/status with the replicas given, as JSON
// objects, for an app at generation gen with desired replicas.
func status(gen, desired int, rollout string, replicas ...string) string {
	ro := ""
	if rollout != "" {
		ro = `,"rollout":` + rollout
	}
	return `{"name":"web","image":"nginx:alpine","generation":` + itoa(gen) + `,"desired":` + itoa(desired) +
		`,"running":0,"ready":0,"replicas":[` + strings.Join(replicas, ",") + `]` + ro + `}`
}

func itoa(n int) string { return strconv.Itoa(n) }

const (
	putWeb1 = `{"app":{"name":"web","generation":1,"replicas":1},"created":true}`
	noNode  = `{"index":0,"node_id":"","generation":1,"desired":"running","pending_reason":"no node has host port 8080 free","state":"pending","ready":false}`
	pulling = `{"index":0,"node_id":"n1","generation":1,"desired":"running","state":"pending","error":"pulling nginx:alpine, started 10:00:00 UTC; the registry has not answered yet","ready":false}`
	ready0  = `{"index":0,"node_id":"n1","generation":1,"desired":"running","state":"running","container_id":"c0","ready":true}`
	crash0  = `{"index":0,"node_id":"n1","generation":1,"desired":"running","state":"exited","error":"exited with code 1","ready":false,"crash_looping":true}`
)

// deploy --wait prints each reason a replica waits once, as it changes,
// and returns when the replica is running and ready.
func TestDeployWait(t *testing.T) {
	waitAPI(t, putWeb1, status(1, 1, "", noNode), status(1, 1, "", noNode), status(1, 1, "", pulling), status(1, 1, "", pulling),
		status(1, 1, "", ready0))
	out, _, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--wait")
	want := "web: created, generation 1, 1 replica\n" +
		"web: replica 0: pending, with no node: no node has host port 8080 free\n" +
		"web: replica 0: pulling nginx:alpine, started 10:00:00 UTC; the registry has not answered yet\n" +
		"web: 1 replica running and ready\n"
	if err != nil || out != want {
		t.Fatalf("deploy --wait: %v\n%s\nwant:\n%s", err, out, want)
	}
}

// A replica that is ready for a moment and then crash-loops is not taken as
// ready: it must stay so for the settle time.
func TestDeployWaitOutlastsAQuickExit(t *testing.T) {
	waitAPI(t, putWeb1, status(1, 1, "", ready0), status(1, 1, "", crash0), status(1, 1, "", crash0), status(1, 1, "", ready0))
	out, _, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--wait")
	if err != nil || !strings.Contains(out, "web: replica 0: crash-looping: exited with code 1\n") || !strings.HasSuffix(out, "web: 1 replica running and ready\n") {
		t.Fatalf("deploy --wait over a quick exit: %v\n%s", err, out)
	}
}

// During a rollout a replica at the old generation does not count, and the
// rollout's reason is printed when it pauses.
func TestDeployWaitForTheNewGeneration(t *testing.T) {
	old := `{"index":0,"node_id":"n1","generation":1,"desired":"running","state":"running","ready":true}`
	sick := `{"index":0,"node_id":"n1","generation":2,"desired":"running","state":"running","health":"starting","ready":false}`
	fine := `{"index":0,"node_id":"n1","generation":2,"desired":"running","state":"running","health":"healthy","ready":true}`
	paused := `{"target":2,"state":"paused","reason":"the rollout to generation 2 is paused: replica 0 did not become ready","moving":0}`
	waitAPI(t, `{"app":{"name":"web","generation":2,"replicas":1},"created":false}`,
		status(2, 1, `{"target":2,"state":"in_progress","moving":0}`, old), status(2, 1, paused, sick), status(2, 1, "", fine))
	out, _, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:1.27", "--wait")
	want := "web: updated, generation 2, 1 replica\n" +
		"web: replica 0: running; its health check has not passed yet\n" +
		"web: the rollout to generation 2 is paused: replica 0 did not become ready\n" +
		"web: 1 replica running and ready\n"
	if err != nil || out != want {
		t.Fatalf("deploy --wait during a rollout: %v\n%s\nwant:\n%s", err, out, want)
	}
}

// After --timeout, --wait prints the status and fails with exit status 1.
// The spec stays deployed.
func TestDeployWaitTimesOut(t *testing.T) {
	waitAPI(t, putWeb1, status(1, 1, "", pulling))
	out, _, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--wait", "--timeout", "60ms")
	want := "web: 0 of 1 replica running and ready after 60ms; hivemind status web shows why"
	if err == nil || err.Error() != want || exitCode(err) != 1 {
		t.Fatalf("deploy --wait --timeout: %v (exit %d), want %q", err, exitCode(err), want)
	}
	for _, line := range []string{"web: replica 0: pulling nginx:alpine", "APP  IMAGE", "APP  REPLICA", "pending"} {
		if !strings.Contains(out, line) {
			t.Errorf("output after the timeout has no %q:\n%s", line, out)
		}
	}
}

// A server older than 2.2 sends no ready field: a running replica with no
// health check, or a passing one, is taken as ready.
func TestDeployWaitAgainstAnOldServer(t *testing.T) {
	starting := `{"index":0,"node_id":"n1","generation":1,"desired":"running","state":"running","health":"starting"}`
	healthy := `{"index":0,"node_id":"n1","generation":1,"desired":"running","state":"running","health":"healthy"}`
	waitAPI(t, putWeb1, status(1, 1, "", starting), status(1, 1, "", healthy))
	out, _, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--wait")
	if err != nil || !strings.Contains(out, "health check has not passed yet") || !strings.HasSuffix(out, "web: 1 replica running and ready\n") {
		t.Fatalf("deploy --wait against an old server: %v\n%s", err, out)
	}
}

// scale --wait waits for the new replicas too.
func TestScaleWait(t *testing.T) {
	one := `{"index":1,"node_id":"n1","generation":1,"desired":"running","state":"pending","ready":false}`
	oneReady := `{"index":1,"node_id":"n1","generation":1,"desired":"running","state":"running","ready":true}`
	waitAPI(t, `{"name":"web","generation":1,"replicas":2}`, status(1, 2, "", ready0, one), status(1, 2, "", ready0, oneReady))
	out, _, err := run(t, "", "scale", "web", "2", "--wait")
	if want := "web: 2 replicas\nweb: 2 replicas running and ready\n"; err != nil || out != want {
		t.Fatalf("scale --wait: %v\n%s\nwant:\n%s", err, out, want)
	}
}

// With --json, standard output has only the reply, and the progress goes to
// standard error.
func TestDeployWaitJSON(t *testing.T) {
	waitAPI(t, putWeb1, status(1, 1, "", pulling), status(1, 1, "", ready0))
	out, errOut, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--wait", "--json")
	var res server.PutResult
	if err != nil || json.Unmarshal([]byte(out), &res) != nil || res.App.Name != "web" {
		t.Fatalf("deploy --wait --json: %v\n%s", err, out)
	}
	if !strings.Contains(errOut, "web: replica 0: pulling") || !strings.Contains(errOut, "web: 1 replica running and ready") {
		t.Fatalf("deploy --wait --json, standard error: %q", errOut)
	}
}

func TestWaitFlagErrors(t *testing.T) {
	waitAPI(t, putWeb1, status(1, 1, "", ready0))
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"deploy", "--name", "web", "--image", "nginx", "--timeout", "1m"}, "--timeout is how long --wait waits: give --wait too"},
		{[]string{"scale", "web", "2", "--timeout", "1m"}, "--timeout is how long --wait waits: give --wait too"},
		{[]string{"deploy", "--name", "web", "--image", "nginx", "--wait", "--timeout", "0s"}, "--timeout must be more than 0"},
	} {
		if _, _, err := run(t, "", c.args...); err == nil || err.Error() != c.want {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
}
