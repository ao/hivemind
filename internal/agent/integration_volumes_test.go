//go:build integration

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// itVolumeAPI calls /volumes/<name> on the engine directly and returns the
// HTTP status. The runtime has no volume calls.
func itVolumeAPI(t *testing.T, method, name string) int {
	t.Helper()
	sock := strings.TrimPrefix(os.Getenv("DOCKER_HOST"), "unix://")
	hc := &http.Client{Transport: &http.Transport{Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	req, err := http.NewRequest(method, "http://docker/v"+docker.APIVersion+"/volumes/"+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Two apps on one node that both use a volume called "data" each get their
// own data. Deleting an app keeps its volume, and deploying the app again
// finds the data.
func TestIntegrationVolumesSeparateAndKept(t *testing.T) {
	rt := itRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	var b [4]byte
	_, _ = rand.Read(b[:])
	sfx := hex.EncodeToString(b[:])
	apps := []string{"vola-" + sfx, "volb-" + sfx}
	t.Cleanup(func() {
		for _, app := range apps {
			cs, _ := rt.List(context.Background(), map[string]string{runtime.LabelApp: app})
			for _, c := range cs {
				_ = rt.Remove(context.Background(), c.ID, true)
			}
			itVolumeAPI(t, http.MethodDelete, runtime.VolumeName(app, "data"))
		}
	})

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertNode(ctx, store.Node{ID: "node-1", Name: "local", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	a, stop := itAgent(t, rt, st)
	defer stop()

	// Each replica writes its app's name to /data/owner unless the file is
	// there already, so the second app would keep the first one's name if
	// the volume were shared.
	deploy := func(app string) {
		t.Helper()
		_, err := st.PutApp(ctx, spec.App{Name: app, Image: "alpine:3",
			Env:     map[string]string{"OWNER": app},
			Command: []string{"sh", "-c", `[ -f /data/owner ] || echo "$OWNER" > /data/owner; sleep 300`},
			Volumes: []spec.Volume{{Name: "data", Path: "/data"}}})
		if err != nil {
			t.Fatal(err)
		}
		a.Trigger()
	}
	// run execs a shell command in the app's running replica. running is
	// false when there is none yet.
	run := func(app, cmd string) (running bool, code int) {
		t.Helper()
		_, r := itRunning(t, rt, app)
		c, ok := r["0"]
		if !ok {
			return false, -1
		}
		code, err := rt.Exec(ctx, c.ID, []string{"sh", "-c", cmd})
		if err != nil {
			return false, -1
		}
		return true, code
	}
	owner := func(app string) (running bool, code int) {
		return run(app, `test "$(cat /data/owner)" = "`+app+`"`)
	}

	for _, app := range apps {
		deploy(app)
		itWait(t, app+" running", time.Minute, func() bool { ok, _ := owner(app); return ok })
	}
	for _, app := range apps {
		if _, code := owner(app); code != 0 {
			t.Fatalf("%s: /data/owner is not %q (exit code %d): the volume is shared", app, app, code)
		}
		if got := itVolumeAPI(t, http.MethodGet, runtime.VolumeName(app, "data")); got != http.StatusOK {
			t.Fatalf("volume %s: HTTP %d", runtime.VolumeName(app, "data"), got)
		}
	}

	// Mark the first app's volume, then delete the app: its container goes
	// and its volume stays.
	if _, code := run(apps[0], "echo before-delete > /data/kept"); code != 0 {
		t.Fatalf("writing the marker: exit code %d", code)
	}
	if err := st.DeleteApp(ctx, apps[0]); err != nil {
		t.Fatal(err)
	}
	a.Trigger()
	itWait(t, "containers removed", 30*time.Second, func() bool {
		all, _ := itRunning(t, rt, apps[0])
		return all == 0
	})
	if got := itVolumeAPI(t, http.MethodGet, runtime.VolumeName(apps[0], "data")); got != http.StatusOK {
		t.Fatalf("volume of the deleted app: HTTP %d, want 200", got)
	}

	// Deploy it again: the new container finds the marker.
	deploy(apps[0])
	itWait(t, "redeployed app running", time.Minute, func() bool { ok, _ := owner(apps[0]); return ok })
	if _, code := run(apps[0], `test "$(cat /data/kept)" = before-delete`); code != 0 {
		t.Fatalf("redeployed %s: the data written before the delete is gone (exit code %d)", apps[0], code)
	}
	// The other app's data was never touched.
	if _, code := run(apps[1], "test ! -e /data/kept"); code != 0 {
		t.Fatalf("%s sees the marker written by %s", apps[1], apps[0])
	}
}

// itEngine calls the engine directly, with a JSON body unless body is
// empty, and returns the HTTP status and the reply.
func itEngine(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	sock := strings.TrimPrefix(os.Getenv("DOCKER_HOST"), "unix://")
	hc := &http.Client{Transport: &http.Transport{Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	req, err := http.NewRequest(method, "http://docker/v"+docker.APIVersion+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// An image that declares a VOLUME gives each container an anonymous volume.
// Three rollouts and a delete remove each one with its container, and the
// app's named volume keeps what every generation wrote to it.
func TestIntegrationRolloutsLeaveNoAnonymousVolume(t *testing.T) {
	rt := itRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	var b [4]byte
	_, _ = rand.Read(b[:])
	sfx := hex.EncodeToString(b[:])
	app := "anon-" + sfx
	named := runtime.VolumeName(app, "data")

	// The image: alpine with VOLUME /anon, made by committing a container
	// with that change.
	if err := rt.Pull(ctx, "alpine:3", nil); err != nil {
		t.Fatal(err)
	}
	code, body := itEngine(t, http.MethodPost, "/containers/create", `{"Image":"alpine:3"}`)
	if code != http.StatusCreated {
		t.Fatalf("create the base container: HTTP %d %s", code, body)
	}
	var base struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &base); err != nil {
		t.Fatal(err)
	}
	img := "hivemind-it-anon:" + sfx
	q := url.Values{"container": {base.ID}, "repo": {"hivemind-it-anon"}, "tag": {sfx}, "changes": {"VOLUME /anon"}}
	if code, body := itEngine(t, http.MethodPost, "/commit?"+q.Encode(), "{}"); code != http.StatusCreated {
		t.Fatalf("commit: HTTP %d %s", code, body)
	}
	itEngine(t, http.MethodDelete, "/containers/"+base.ID+"?force=1", "")
	t.Cleanup(func() {
		cs, _ := rt.List(context.Background(), map[string]string{runtime.LabelApp: app})
		for _, c := range cs {
			_ = rt.Remove(context.Background(), c.ID, true)
		}
		itEngine(t, http.MethodDelete, "/volumes/"+named, "")
		itEngine(t, http.MethodDelete, "/images/"+img+"?force=1", "")
	})

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertNode(ctx, store.Node{ID: "node-1", Name: "local", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	a, stop := itAgent(t, rt, st)
	defer stop()

	// anon holds the anonymous volume of every container the app has had.
	anon := map[string]bool{}
	record := func() {
		cs, err := rt.List(ctx, map[string]string{runtime.LabelApp: app})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			code, body := itEngine(t, http.MethodGet, "/containers/"+c.ID+"/json", "")
			if code != http.StatusOK {
				continue // removed since the list
			}
			var in struct {
				Mounts []struct{ Type, Name, Destination string }
			}
			if err := json.Unmarshal(body, &in); err != nil {
				t.Fatal(err)
			}
			for _, m := range in.Mounts {
				if m.Type == "volume" && m.Destination == "/anon" {
					anon[m.Name] = true
				}
			}
		}
	}
	deploy := func(version string) {
		t.Helper()
		_, err := st.PutApp(ctx, spec.App{Name: app, Image: img,
			Env:     map[string]string{"VERSION": version},
			Command: []string{"sh", "-c", `echo "$VERSION" >> /data/versions; echo x > /anon/f; sleep 300`},
			Volumes: []spec.Volume{{Name: "data", Path: "/data"}}})
		if err != nil {
			t.Fatal(err)
		}
		a.Trigger()
	}
	for v := 1; v <= 4; v++ {
		gen := strconv.Itoa(v)
		deploy(gen)
		itWait(t, "generation "+gen+" running", 2*time.Minute, func() bool {
			if err := st.AdvanceRollouts(ctx); err != nil {
				t.Errorf("AdvanceRollouts: %v", err)
			}
			a.Trigger()
			record()
			all, r := itRunning(t, rt, app)
			return all == 1 && len(r) == 1 && r["0"].Labels[runtime.LabelGeneration] == gen
		})
	}
	_, r := itRunning(t, rt, app)
	if code, err := rt.Exec(ctx, r["0"].ID, []string{"sh", "-c", `test "$(cat /data/versions | tr '\n' ' ')" = "1 2 3 4 "`}); err != nil || code != 0 {
		t.Fatalf("the named volume does not hold the line of each generation: exit code %d, %v", code, err)
	}

	if err := st.DeleteApp(ctx, app); err != nil {
		t.Fatal(err)
	}
	a.Trigger()
	itWait(t, "containers removed", 30*time.Second, func() bool {
		all, _ := itRunning(t, rt, app)
		return all == 0
	})
	if len(anon) != 4 {
		t.Fatalf("saw %d anonymous volumes, want one for each of the 4 containers: %v", len(anon), anon)
	}
	for name := range anon {
		if got := itVolumeAPI(t, http.MethodGet, name); got != http.StatusNotFound {
			t.Errorf("anonymous volume %s is left after its container was removed: HTTP %d, want 404", name, got)
		}
	}
	if got := itVolumeAPI(t, http.MethodGet, named); got != http.StatusOK {
		t.Errorf("named volume %s after the delete: HTTP %d, want 200", named, got)
	}
}
