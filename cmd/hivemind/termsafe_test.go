package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/server"
)

func TestTermSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain, ü":                 "plain, ü",
		"a\x1b[2Jb":                `a\x1b[2Jb`,
		"line\nnext\ttab\r":        `line\nnext\ttab\r`,
		"\u009b31m\x07\x00":        `\u009b31m\a\x00`,
		"\x1b]0;title\x07":         `\x1b]0;title\a`,
		"no escapes at all: ${X}.": "no escapes at all: ${X}.",
	} {
		if got := termSafe(in); got != want {
			t.Errorf("termSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

// A server, or a worker through it, that sends text with terminal escapes
// in a replica's state and error, a rollout reason, a node's name and
// version, or an error message gets them printed as \x1b and the like,
// never as the bytes, by status, nodes and the error.
func TestServerTextIsEscaped(t *testing.T) {
	isolate(t)
	const esc = "\x1b[2J\x1b]0;owned\x07\u009b31m"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/status":
			json.NewEncoder(w).Encode([]server.AppStatus{{Name: "web", Image: "nginx", Generation: 1, Desired: 1,
				Rollout:  &server.RolloutStatus{Target: 2, State: "paused", Reason: "paused" + esc},
				Replicas: []server.ReplicaStatus{{Index: 0, NodeID: "n1", Generation: 1, Desired: "running", State: "exited" + esc, ContainerID: "abc", Error: "failed" + esc}}}})
		case "/v1/nodes":
			json.NewEncoder(w).Encode([]server.NodeView{{ID: "n1", Name: "box" + esc, Version: "2.2.0" + esc, State: "ready"}})
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(server.ErrorBody{Error: "refused" + esc})
		}
	}))
	defer ts.Close()
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, "test")

	for _, args := range [][]string{{"status"}, {"nodes"}} {
		out, errOut, err := run(t, "", args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.ContainsAny(out+errOut, "\x1b\x07\u009b") {
			t.Errorf("%v printed a control character:\n%q", args, out+errOut)
		}
		if !strings.Contains(out, `\x1b[2J\x1b]0;owned\a\u009b31m`) {
			t.Errorf("%v: the escaped text is missing:\n%s", args, out)
		}
	}
	_, _, err := run(t, "", "scale", "web", "2")
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\x07\u009b") || !strings.Contains(err.Error(), `refused\x1b[2J`) {
		t.Fatalf("error %q, want the server's message escaped", err)
	}
}
