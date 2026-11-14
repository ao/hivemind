package compose

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ao/hivemind/internal/spec"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

// TestGolden converts testdata/golden/compose.yaml, with the .env beside it
// and no other variables, and compares the specs and the notes with
// apps.yaml and notes.txt there.
func TestGolden(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	file, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dotenv, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Convert(file, Options{DotEnv: dotenv})
	if err != nil {
		t.Fatal(err)
	}
	apps, err := Marshal(res.Apps)
	if err != nil {
		t.Fatal(err)
	}
	var notes bytes.Buffer
	for _, n := range res.Notes {
		notes.WriteString(n.String() + "\n")
	}
	for name, got := range map[string][]byte{"apps.yaml": apps, "notes.txt": notes.Bytes()} {
		path := filepath.Join(dir, name)
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs (run go test -run TestGolden -update to rewrite it)\n--- got\n%s\n--- want\n%s", path, got, want)
		}
	}
	// Every document in apps.yaml is a spec that deploy -f accepts.
	if got := roundTrip(t, apps); len(got) != len(res.Apps) {
		t.Fatalf("%d documents, %d apps", len(got), len(res.Apps))
	}
}

// roundTrip decodes the YAML documents that Marshal wrote, the way
// `hivemind deploy -f` reads one, and checks each against its spec.
func roundTrip(t *testing.T, out []byte) []spec.App {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(out))
	dec.KnownFields(true)
	var apps []spec.App
	for {
		var a spec.App
		err := dec.Decode(&a)
		if errors.Is(err, io.EOF) {
			return apps
		}
		if err != nil {
			t.Fatalf("decode the output: %v\n%s", err, out)
		}
		a.Normalize()
		if err := a.Validate(); err != nil {
			t.Fatalf("the output does not validate: %v\n%s", err, out)
		}
		apps = append(apps, a)
	}
}

// FuzzConvert feeds arbitrary files and .env files through Convert. It must
// never panic. An error must be a *Error with problems. Every app it returns
// must validate, come in service order, match AppNames, and survive the
// YAML that convert prints.
func FuzzConvert(f *testing.F) {
	golden, err := os.ReadFile(filepath.Join("testdata", "golden", "compose.yaml"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(golden, []byte("DB_PASSWORD=x\nAPI_TAG='1'\n"))
	f.Add([]byte(web("ports: ['8080:80', {target: 1, published: 2}]\nenvironment: [A=$A]\ncommand: sh -c 'x'")), []byte(nil))
	f.Add([]byte("volumes: {d: }\nservices:\n  a: {image: x, volumes: [d:/d], deploy: {resources: {limits: {cpus: 1, memory: 1g}}}}\n"), []byte("A=\"b\\n\"\n"))
	f.Add([]byte("x: &x {image: nginx}\nservices:\n  a: {<<: *x, healthcheck: {test: [CMD-SHELL, x], interval: 5s}}\n"), []byte(nil))
	f.Add([]byte("a: &a [*a]"), []byte("export X=1 # c"))
	f.Fuzz(func(t *testing.T, data, dotenv []byte) {
		opts := Options{Lookup: env(map[string]string{"A": "a", "PORT": "8080", "EMPTY": ""})}
		if len(dotenv) > 0 {
			opts.DotEnv = dotenv
		}
		res, err := Convert(data, opts)
		if err != nil {
			var ce *Error
			if !errors.As(err, &ce) || len(ce.Problems) == 0 || strings.TrimSpace(err.Error()) == "" {
				t.Fatalf("error %T with no problems: %v", err, err)
			}
			return
		}
		if len(res.Apps) == 0 {
			t.Fatal("no apps and no error")
		}
		names, err := AppNames(data, "")
		if err != nil || len(names) != len(res.Apps) {
			t.Fatalf("Convert gave %d apps, AppNames %v, %v", len(res.Apps), names, err)
		}
		for i, a := range res.Apps {
			s := a.Spec
			s.Normalize()
			if err := s.Validate(); err != nil {
				t.Fatalf("app %s does not validate: %v", a.Service, err)
			}
			if s.Template() != a.Spec.Template() || s.Replicas != a.Spec.Replicas {
				t.Fatalf("app %s was not normalised", a.Service)
			}
			if names[i] != a.Spec.Name {
				t.Fatalf("app %d: AppNames %q, Convert %q", i, names[i], a.Spec.Name)
			}
			if i > 0 && res.Apps[i-1].Service >= a.Service {
				t.Fatalf("apps out of order: %q then %q", res.Apps[i-1].Service, a.Service)
			}
		}
		out, err := Marshal(res.Apps)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		back := roundTrip(t, out)
		if len(back) != len(res.Apps) {
			t.Fatalf("%d documents for %d apps:\n%s", len(back), len(res.Apps), out)
		}
		for i, b := range back {
			if b.Name != res.Apps[i].Spec.Name || b.Replicas != res.Apps[i].Spec.Replicas || b.Template() != res.Apps[i].Spec.Template() {
				t.Fatalf("app %s changed in the printed YAML:\n%s\n%s", b.Name, res.Apps[i].Spec.Template(), b.Template())
			}
		}
	})
}
