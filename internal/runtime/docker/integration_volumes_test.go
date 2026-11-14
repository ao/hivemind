//go:build integration

package docker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

// itVolumeLabels returns a volume's labels, read from the engine directly.
func itVolumeLabels(t *testing.T, c *Client, name string) map[string]string {
	t.Helper()
	code, body := itRawAPI(t, c, http.MethodGet, "/volumes/"+name)
	if code != http.StatusOK {
		t.Fatalf("volume %s: HTTP %d %s", name, code, body)
	}
	var v struct {
		Name   string
		Labels map[string]string
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v.Labels
}

// itRunOnce creates and starts a container that runs cmd with the app's
// volume at /data, waits for it to exit, and returns its stdout.
func itRunOnce(t *testing.T, c *Client, app, volume, cmd string) string {
	t.Helper()
	id := itCreate(t, c, runtime.ContainerConfig{
		Name: itName(app), Image: alpineImage, App: app,
		Command: []string{"sh", "-c", cmd},
		Volumes: []runtime.VolumeMount{{Name: volume, Path: "/data"}},
	})
	if err := c.Start(itCtx(t), id); err != nil {
		t.Fatal(err)
	}
	if got := itWaitState(t, c, id, "exited", 30*time.Second); got.ExitCode != 0 {
		t.Fatalf("%s: exit code %d", cmd, got.ExitCode)
	}
	var out bytes.Buffer
	if err := c.Logs(itCtx(t), id, runtime.LogOptions{}, &out, &out); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out.String())
}

// Two apps that both name a volume "data" get separate volumes, each
// labelled with its app.
func TestIntegrationVolumesScopedToApp(t *testing.T) {
	c := itClient(t)
	itPull(t, c, alpineImage)
	appA, appB := itName("va"), itName("vb")
	const vol = "data"
	for _, app := range []string{appA, appB} {
		name := runtime.VolumeName(app, vol)
		t.Cleanup(func() { itRawAPI(t, c, http.MethodDelete, "/volumes/"+name) })
	}

	// Print what is in /data/owner, or "empty", then write the app's name.
	write := func(app string) string {
		return "cat /data/owner 2>/dev/null || echo empty; echo " + app + " > /data/owner"
	}
	if got := itRunOnce(t, c, appA, vol, write(appA)); got != "empty" {
		t.Fatalf("first container of %s found %q in a new volume", appA, got)
	}
	if got := itRunOnce(t, c, appB, vol, write(appB)); got != "empty" {
		t.Fatalf("%s found %q: it shares %s's volume", appB, got, appA)
	}
	if got := itRunOnce(t, c, appA, vol, "cat /data/owner"); got != appA {
		t.Fatalf("%s's volume holds %q, want %q", appA, got, appA)
	}

	for _, app := range []string{appA, appB} {
		labels := itVolumeLabels(t, c, runtime.VolumeName(app, vol))
		if labels[runtime.LabelApp] != app || labels[runtime.LabelVolume] != vol {
			t.Errorf("volume %s labels = %v", runtime.VolumeName(app, vol), labels)
		}
	}
	// The spec name alone is not used as an engine volume.
	if code, _ := itRawAPI(t, c, http.MethodGet, "/volumes/"+vol); code != http.StatusNotFound {
		t.Errorf("a volume named %q exists: HTTP %d", vol, code)
	}
}

// A volume that already exists, without labels, is mounted with its data,
// and its labels are left alone.
func TestIntegrationExistingVolumeMountedAsIs(t *testing.T) {
	c := itClient(t)
	itPull(t, c, alpineImage)
	app := itName("pre")
	name := runtime.VolumeName(app, "data")
	t.Cleanup(func() { itRawAPI(t, c, http.MethodDelete, "/volumes/"+name) })

	// docker volume create, out of band, with no labels.
	req, err := http.NewRequestWithContext(itCtx(t), http.MethodPost, c.base+"/v"+APIVersion+"/volumes/create",
		strings.NewReader(`{"Name":"`+name+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("volume create: HTTP %d", resp.StatusCode)
	}
	if got := itRunOnce(t, c, app, "data", "echo kept > /data/f; cat /data/f"); got != "kept" {
		t.Fatalf("output %q", got)
	}
	if got := itRunOnce(t, c, app, "data", "cat /data/f"); got != "kept" {
		t.Fatalf("second container read %q", got)
	}
	if labels := itVolumeLabels(t, c, name); len(labels) != 0 {
		t.Errorf("labels of the existing volume changed to %v", labels)
	}
}
