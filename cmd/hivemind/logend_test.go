package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/server"
)

// logServer answers GET /v1/apps/web/logs with two lines, then ends the
// body as end says: "done" or "shutdown" in the trailer, nothing, or a
// connection dropped in the middle of the chunked body.
func logServer(t *testing.T, end string) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Trailer", server.LogEndTrailer)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "one\ntwo\n")
		w.(http.Flusher).Flush()
		switch end {
		case "drop":
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				conn.Close()
			}
		case "":
		default:
			w.Header().Set(server.LogEndTrailer, end)
		}
	}))
	t.Cleanup(ts.Close)
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, "token")
}

// logs -f says why the stream ended. The end of the log is not an error;
// a server that shuts down, a broken stream and a dropped connection are,
// with a message that says so.
func TestLogsFollowSaysWhyItEnded(t *testing.T) {
	cases := []struct {
		end, want string
	}{
		{server.LogEndDone, ""},
		{"", ""}, // a server older than 2.2 sends no trailer
		{server.LogEndShutdown, "the server is shutting down"},
		{server.LogEndError, "could not read the rest of the log"},
		{"drop", "the connection to the server broke"},
	}
	for _, c := range cases {
		t.Run(c.end, func(t *testing.T) {
			isolate(t)
			logServer(t, c.end)
			out, _, err := run(t, "", "logs", "-f", "web")
			if out != "one\ntwo\n" {
				t.Fatalf("output %q", out)
			}
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("logs -f: %v, want no error", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("logs -f: %v, want an error with %q", err, c.want)
			case c.want != "" && !strings.Contains(err.Error(), "run the command again"):
				t.Fatalf("logs -f: %v does not say what to do", err)
			}
		})
	}
}

func TestLogEndWithoutFollow(t *testing.T) {
	if err := logEnd(nil, server.LogEndDone, false); err != nil {
		t.Fatal(err)
	}
	err := logEnd(io.ErrUnexpectedEOF, "", false)
	if err == nil || strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("logEnd without -f: %v", err)
	}
}
