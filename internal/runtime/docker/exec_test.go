package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

func TestInspectReadsAddressAndTimes(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusOK, map[string]any{
			"Id":   "abc",
			"Name": "/web",
			"State": map[string]any{
				"Status":     "exited",
				"ExitCode":   1,
				"StartedAt":  "2026-11-13T10:00:00.5Z",
				"FinishedAt": "2026-11-13T10:00:02Z",
			},
			"NetworkSettings": map[string]any{
				"IPAddress": "",
				"Networks": map[string]any{
					"zz": map[string]any{"IPAddress": "172.18.0.9"},
					"aa": map[string]any{"IPAddress": "172.17.0.4"},
				},
			},
		})
	})
	got, err := c.Inspect(context.Background(), "abc")
	requireNoErr(t, err)
	if got.IPAddress != "172.17.0.4" {
		t.Errorf("IPAddress = %q, want the first network by name", got.IPAddress)
	}
	if !got.StartedAt.Equal(time.Date(2026, 11, 13, 10, 0, 0, 500000000, time.UTC)) {
		t.Errorf("StartedAt = %v", got.StartedAt)
	}
	if !got.FinishedAt.Equal(time.Date(2026, 11, 13, 10, 0, 2, 0, time.UTC)) {
		t.Errorf("FinishedAt = %v", got.FinishedAt)
	}
}

func TestInspectLegacyAddressWinsAndUnsetTimesAreZero(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusOK, map[string]any{
			"Id":   "abc",
			"Name": "/web",
			"State": map[string]any{
				"Status":     "created",
				"StartedAt":  "0001-01-01T00:00:00Z",
				"FinishedAt": "0001-01-01T00:00:00Z",
			},
			"NetworkSettings": map[string]any{
				"IPAddress": "10.0.0.2",
				"Networks":  map[string]any{"bridge": map[string]any{"IPAddress": "172.17.0.2"}},
			},
		})
	})
	got, err := c.Inspect(context.Background(), "abc")
	requireNoErr(t, err)
	if got.IPAddress != "10.0.0.2" {
		t.Errorf("IPAddress = %q, want the top-level address", got.IPAddress)
	}
	if !got.StartedAt.IsZero() || !got.FinishedAt.IsZero() {
		t.Errorf("unset times are not zero: %v %v", got.StartedAt, got.FinishedAt)
	}
}

func TestExecReturnsExitCode(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
		switch {
		case ex.Method == http.MethodPost && ex.Path == "/containers/abc/exec":
			writeJSON(w, http.StatusCreated, map[string]string{"Id": "e1"})
		case ex.Method == http.MethodPost && ex.Path == "/exec/e1/start":
			_, _ = w.Write([]byte("ok\n"))
		case ex.Method == http.MethodGet && ex.Path == "/exec/e1/json":
			writeJSON(w, http.StatusOK, map[string]any{"Running": false, "ExitCode": 3})
		default:
			t.Errorf("unexpected request %s %s", ex.Method, ex.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	code, err := c.Exec(context.Background(), "abc", []string{"test", "-f", "/ready"})
	requireNoErr(t, err)
	if code != 3 {
		t.Fatalf("exit code %d, want 3", code)
	}
	reqs := fd.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want create, start and inspect", len(reqs))
	}
	var create struct {
		Cmd          []string `json:"Cmd"`
		AttachStdout bool     `json:"AttachStdout"`
		AttachStderr bool     `json:"AttachStderr"`
	}
	if err := json.Unmarshal(reqs[0].Body, &create); err != nil {
		t.Fatal(err)
	}
	if strings.Join(create.Cmd, " ") != "test -f /ready" || !create.AttachStdout || !create.AttachStderr {
		t.Errorf("create body = %s", reqs[0].Body)
	}
	var start struct {
		Detach bool `json:"Detach"`
	}
	if err := json.Unmarshal(reqs[1].Body, &start); err != nil || start.Detach {
		t.Errorf("start body = %s, want Detach false so the stream ends with the process", reqs[1].Body)
	}
}

func TestExecNonRunningContainerIsConflict(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "Container abc is not running"})
	})
	_, err := c.Exec(context.Background(), "abc", []string{"true"})
	requireIs(t, err, runtime.ErrConflict)
}

func TestExecRejectsEmptyCommandWithoutRequest(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
		t.Errorf("request sent for an empty command: %s %s", ex.Method, ex.Path)
	})
	_, err := c.Exec(context.Background(), "abc", nil)
	requireIs(t, err, runtime.ErrInvalid)
	if n := len(fd.requests()); n != 0 {
		t.Fatalf("%d requests", n)
	}
}

func TestExecCancelledContextIsNotUnavailable(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		switch ex.Path {
		case "/containers/abc/exec":
			writeJSON(w, http.StatusCreated, map[string]string{"Id": "e2"})
		case "/exec/e2/start":
			// The process runs for a long time: hold the stream open.
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-release
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.Exec(ctx, "abc", []string{"sleep", "100"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context deadline", err)
	}
	requireNotIs(t, err, runtime.ErrUnavailable)
}

func TestExecStillRunningAfterStreamIsError(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		switch ex.Path {
		case "/containers/abc/exec":
			writeJSON(w, http.StatusCreated, map[string]string{"Id": "e3"})
		case "/exec/e3/start":
			_, _ = w.Write(nil)
		case "/exec/e3/json":
			writeJSON(w, http.StatusOK, map[string]any{"Running": true, "ExitCode": 0})
		}
	})
	_, err := c.Exec(context.Background(), "abc", []string{"true"})
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("err = %v, want a still-running error", err)
	}
}
