package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

// rotationDaemon is a fake daemon whose default log driver and /info
// failures a test sets.
type rotationDaemon struct {
	mu       sync.Mutex
	driver   string
	infoDown bool
}

func (d *rotationDaemon) set(driver string, down bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.driver, d.infoDown = driver, down
}

func (d *rotationDaemon) handle(w http.ResponseWriter, ex exchange) {
	d.mu.Lock()
	driver, down := d.driver, d.infoDown
	d.mu.Unlock()
	if ex.Path == "/info" {
		if down {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "info failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"LoggingDriver": driver})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"Id": "c0ffee"})
}

// logConfigOf returns the LogConfig of the last create request, or nil
// when it has none.
func logConfigOf(t *testing.T, fd *fakeDaemon) *logConfig {
	t.Helper()
	reqs := fd.requests()
	last := reqs[len(reqs)-1]
	if last.Path != "/containers/create" {
		t.Fatalf("last request is %s %s, want a create", last.Method, last.Path)
	}
	var body struct {
		HostConfig struct {
			LogConfig *logConfig
		}
	}
	if err := json.Unmarshal(last.Body, &body); err != nil {
		t.Fatal(err)
	}
	return body.HostConfig.LogConfig
}

func countInfo(fd *fakeDaemon) int {
	n := 0
	for _, ex := range fd.requests() {
		if ex.Path == "/info" {
			n++
		}
	}
	return n
}

// With the daemon's default driver json-file, a container is created with
// json-file, max-size 10m and max-file 5. With any other driver, Create
// leaves the log config to the daemon. The driver is read at most once a
// minute, and a failed read uses the last one.
func TestCreateLogRotation(t *testing.T) {
	d := &rotationDaemon{driver: "json-file"}
	c, fd := newFake(t, d.handle)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	create := func() error {
		_, err := c.Create(context.Background(), runtime.ContainerConfig{Name: "web-0-1", Image: "nginx", App: "web"})
		return err
	}

	// No driver read yet and /info fails: the create is not sent.
	d.set("json-file", true)
	if err := create(); err == nil {
		t.Fatal("Create succeeded without the daemon's log driver")
	}
	if n := len(fd.requests()); n != 1 {
		t.Fatalf("%d requests, want only the failed /info", n)
	}

	d.set("json-file", false)
	requireNoErr(t, create())
	got := logConfigOf(t, fd)
	if got == nil || got.Type != "json-file" || len(got.Config) != 2 || got.Config["max-size"] != "10m" || got.Config["max-file"] != "5" {
		t.Fatalf("log config %+v, want json-file with max-size 10m and max-file 5", got)
	}
	requireNoErr(t, create())
	if n := countInfo(fd); n != 2 {
		t.Fatalf("%d /info requests for two creates in a minute after the failure, want 2", n)
	}

	// A minute later the daemon has another driver: Create reads it, and
	// sends no log config.
	now = now.Add(logDriverTTL)
	d.set("journald", false)
	requireNoErr(t, create())
	if got := logConfigOf(t, fd); got != nil {
		t.Fatalf("log config %+v with the journald driver, want none", got)
	}
	if n := countInfo(fd); n != 3 {
		t.Fatalf("%d /info requests, want 3", n)
	}

	// /info fails once a driver is known: the last one is used.
	now = now.Add(logDriverTTL)
	d.set("json-file", true)
	requireNoErr(t, create())
	if got := logConfigOf(t, fd); got != nil {
		t.Fatalf("log config %+v after a failed /info, want the last driver's (none)", got)
	}

	for _, driver := range []string{"local", "syslog", "none", ""} {
		now = now.Add(logDriverTTL)
		d.set(driver, false)
		requireNoErr(t, create())
		if got := logConfigOf(t, fd); got != nil {
			t.Errorf("driver %q: log config %+v, want none", driver, got)
		}
	}
}
