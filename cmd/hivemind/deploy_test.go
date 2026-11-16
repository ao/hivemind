package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/spec"
)

// deploy from flags takes the container's command after --, and named
// volumes with --volume.
func TestDeployCommandAndVolumesFromFlags(t *testing.T) {
	st, _ := apiServer(t, "")
	ctx := context.Background()
	if out, _, err := run(t, "", "deploy", "--name", "sleeper", "--image", "busybox:stable", "--", "sh", "-c", "sleep 86400"); err != nil ||
		!strings.HasPrefix(out, "sleeper: created") {
		t.Fatalf("deploy with a command: %q, %v", out, err)
	}
	a, err := st.GetApp(ctx, "sleeper")
	if err != nil || !slices.Equal(a.Spec.Command, []string{"sh", "-c", "sleep 86400"}) {
		t.Fatalf("stored command %q, %v", a.Spec.Command, err)
	}
	if _, _, err := run(t, "", "deploy", "--name", "db", "--image", "postgres:17", "--volume", "data:/var/lib/postgresql/data"); err != nil {
		t.Fatal(err)
	}
	a, err = st.GetApp(ctx, "db")
	if err != nil || !slices.Equal(a.Spec.Volumes, []spec.Volume{{Name: "data", Path: "/var/lib/postgresql/data"}}) {
		t.Fatalf("stored volumes %+v, %v", a.Spec.Volumes, err)
	}

	file := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(file, []byte("name: x\nimage: busybox\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"deploy", "--name", "x", "--image", "busybox", "sleep", "60"}, `unexpected argument "sleep": put the container's command after --`},
		{[]string{"deploy", "--name", "x", "--image", "busybox", "--volume", "data"}, `--volume "data": want name:/path`},
		{[]string{"deploy", "--name", "x", "--image", "busybox", "--volume", ":/data"}, "want name:/path"},
		{[]string{"deploy", "--name", "x", "--image", "busybox", "--volume", "/srv/data:/data"}, `--volume "/srv/data:/data": /srv/data is a path on the host, ` +
			"and bind mounts are not supported: hivemind mounts named volumes only; give a name, as in --volume data:/data"},
		{[]string{"deploy", "--name", "x", "--image", "busybox", "--volume", "./data:/data"}, "bind mounts are not supported"},
		{[]string{"deploy", "--name", "x", "--image", "busybox", "--volume", "~/data:/data"}, "bind mounts are not supported"},
		{[]string{"deploy", "-f", file, "--", "sleep", "60"}, "use -f or a command after --, not both"},
		{[]string{"deploy", "-f", file, "--volume", "data:/data"}, "use -f or the --volume flag, not both"},
	} {
		if _, _, err := run(t, "", c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
}

// A key the spec does not have is named with its line, where it is, and
// the key it is closest to, not with a Go type.
func TestDeployFileUnknownKeys(t *testing.T) {
	apiServer(t, "")
	dir := t.TempDir()
	for _, c := range []struct {
		file string
		want string
	}{
		{"name: web\nimage: nginx\nreplica: 2\n", "line 3: unknown key replica in the app (did you mean replicas?)"},
		{"name: web\nimage: nginx\nports:\n  - host: 8080\n    contaner: 80\n", "line 5: unknown key contaner in a port (did you mean container?)"},
		{"name: web\nimage: nginx\nhealth:\n  http: {port: 80}\n  start-period: 5s\n", "line 5: unknown key start-period in health (did you mean start_period?)"},
		{"name: web\nimage: nginx\nflavour: x\n", "line 3: unknown key flavour in the app; its keys are name, image, replicas, command, env, ports, volumes, resources, health, routes"},
	} {
		path := filepath.Join(dir, "app.yaml")
		if err := os.WriteFile(path, []byte(c.file), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := run(t, "", "deploy", "-f", path)
		if err == nil || err.Error() != path+": "+c.want || strings.Contains(err.Error(), "spec.") {
			t.Errorf("%q: %v, want %q", c.file, err, c.want)
		}
	}
}

func TestClosestKey(t *testing.T) {
	keys := []string{"name", "image", "replicas", "command", "env"}
	for in, want := range map[string]string{"replica": "replicas", "Image": "image", "imagee": "image", "comand": "command",
		"en": "", "xyz": "", "names": "name", "volumes": ""} {
		if got := closestKey(in, keys); got != want {
			t.Errorf("closestKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// The app files in docs/concepts.md decode, and pass the server's checks.
func TestConceptsAppFilesAreValid(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "concepts.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(string(b), -1)
	if len(blocks) < 2 {
		t.Fatalf("%d YAML blocks in docs/concepts.md, want the two app files", len(blocks))
	}
	for _, m := range blocks {
		a, err := readSpecFile(strings.NewReader(m[1]), "-")
		if err != nil {
			t.Errorf("%v:\n%s", err, m[1])
			continue
		}
		a.Normalize()
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", a.Name, err)
		}
	}
}

// deploy of a spec the app already has says unchanged, and one that
// changes it says updated, with or without a new generation.
func TestDeployUnchanged(t *testing.T) {
	apiServer(t, "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--name", "web", "--image", "nginx:alpine", "--env", "A=1"}, "web: created, generation 1, 1 replica\n"},
		{[]string{"--name", "web", "--image", "nginx:alpine", "--env", "A=1"}, "web: unchanged, generation 1, 1 replica\n"},
		{[]string{"--name", "web", "--image", "nginx:alpine", "--env", "A=1", "--replicas", "2"}, "web: updated, generation 1, 2 replicas\n"},
		{[]string{"--name", "web", "--image", "nginx:alpine", "--env", "A=2", "--replicas", "2"}, "web: updated, generation 2, 2 replicas\n"},
		{[]string{"--name", "web", "--image", "nginx:alpine", "--env", "A=2", "--replicas", "2"}, "web: unchanged, generation 2, 2 replicas\n"},
	} {
		out, _, err := run(t, "", append([]string{"deploy"}, c.args...)...)
		if err != nil || out != c.want {
			t.Fatalf("deploy %v: %q, %v; want %q", c.args, out, err, c.want)
		}
	}
	out, _, err := run(t, "", "deploy", "--name", "web", "--image", "nginx:alpine", "--env", "A=2", "--replicas", "2", "--json")
	if err != nil || !strings.Contains(out, `"unchanged": true`) {
		t.Fatalf("deploy --json of the same spec: %v\n%s", err, out)
	}
}
