package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

const testToken = "tok-s3cr3t-value-do-not-print"

// testOpts returns Options with a fake environment, a fake file system that
// returns the same content for every path, and uid 1000.
func testOpts(env map[string]string, file string) Options {
	return Options{
		Lookup: func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		},
		ReadFile: func(string) ([]byte, error) {
			return []byte(file), nil
		},
		Getuid: func() int { return 1000 },
	}
}

func newFlags(t *testing.T, opts Options) *pflag.FlagSet {
	t.Helper()
	fl := pflag.NewFlagSet("test", pflag.ContinueOnError)
	Bind(fl, opts)
	return fl
}

// fieldValues renders every Config field by name. Tests compare these maps.
func fieldValues(c *Config) map[string]string {
	return map[string]string{
		"config":             c.ConfigFile,
		"data-dir":           c.DataDir,
		"api-addr":           c.APIAddr,
		"advertise":          c.Advertise,
		"cluster-port":       fmt.Sprint(c.ClusterPort),
		"api-tls-cert":       c.APITLSCert,
		"api-tls-key":        c.APITLSKey,
		"docker-host":        c.DockerHost,
		"log-level":          c.LogLevel,
		"log-format":         c.LogFormat,
		"metrics-addr":       c.MetricsAddr,
		"reconcile-interval": c.ReconcileInterval.String(),
		"server":             c.Server,
		"token":              c.Token.Value(),
	}
}

// precedenceCase gives one setting three distinct valid values: a for the
// flag, b for the environment, c for the file.
type precedenceCase struct {
	key     string
	a, b, c string
	def     string
}

var precedenceCases = []precedenceCase{
	{"data-dir", "/d/a", "/d/b", "/d/c", "/home/t/.local/state/hivemind"},
	{"api-addr", "127.0.0.2:5001", "127.0.0.3:5002", "127.0.0.4:5003", "127.0.0.1:4483"},
	{"advertise", "10.0.0.1", "10.0.0.2", "10.0.0.3", ""},
	{"cluster-port", "4491", "4492", "4493", "4484"},
	{"api-tls-cert", "/a.pem", "/b.pem", "/c.pem", ""},
	{"api-tls-key", "/a.key", "/b.key", "/c.key", ""},
	{"docker-host", "unix:///a.sock", "unix:///b.sock", "unix:///c.sock", "unix:///var/run/docker.sock"},
	{"log-level", "error", "warn", "debug", "info"},
	{"log-format", "json", "text", "json", "text"},
	{"metrics-addr", "127.0.0.1:9101", "127.0.0.1:9102", "127.0.0.1:9103", ""},
	{"reconcile-interval", "3s", "4s", "5s", "2s"},
	{"server", "http://127.0.0.1:1", "http://127.0.0.1:2", "http://127.0.0.1:3", "http://127.0.0.1:4483"},
	{"token", "tok-a", "tok-b", "tok-c", ""},
}

// TestPrecedence checks flag > env > file > default for every field, one
// field at a time, with the other fields left at their defaults.
func TestPrecedence(t *testing.T) {
	for _, pc := range precedenceCases {
		pc := pc
		t.Run(pc.key, func(t *testing.T) {
			ff := func(v string) string { return fmt.Sprintf("%s: %q\n", pc.key, v) }
			env := func(v string) map[string]string {
				return map[string]string{"HOME": "/home/t", EnvName(pc.key): v}
			}

			// flag beats env and file
			opts := testOpts(env(pc.b), ff(pc.c))
			fl := newFlags(t, opts)
			if err := fl.Set(pc.key, pc.a); err != nil {
				t.Fatal(err)
			}
			cfg, err := resolve(fl, opts)
			if err != nil {
				t.Fatalf("flag case: %v", err)
			}
			if got := fieldValues(cfg)[pc.key]; got != pc.a {
				t.Errorf("flag over env and file: got %q, want %q", got, pc.a)
			}

			// env beats file
			opts = testOpts(env(pc.b), ff(pc.c))
			cfg, err = resolve(newFlags(t, opts), opts)
			if err != nil {
				t.Fatalf("env case: %v", err)
			}
			if got := fieldValues(cfg)[pc.key]; got != pc.b {
				t.Errorf("env over file: got %q, want %q", got, pc.b)
			}

			// file beats default
			opts = testOpts(map[string]string{"HOME": "/home/t"}, ff(pc.c))
			cfg, err = resolve(newFlags(t, opts), opts)
			if err != nil {
				t.Fatalf("file case: %v", err)
			}
			if got := fieldValues(cfg)[pc.key]; got != pc.c {
				t.Errorf("file over default: got %q, want %q", got, pc.c)
			}

			// default
			opts = testOpts(map[string]string{"HOME": "/home/t"}, "")
			cfg, err = resolve(newFlags(t, opts), opts)
			if err != nil {
				t.Fatalf("default case: %v", err)
			}
			if got := fieldValues(cfg)[pc.key]; got != pc.def {
				t.Errorf("default: got %q, want %q", got, pc.def)
			}
		})
	}
}

func TestEnvNameIsDerivedFromFlag(t *testing.T) {
	cases := map[string]string{
		"api-addr":           "HIVEMIND_API_ADDR",
		"data-dir":           "HIVEMIND_DATA_DIR",
		"reconcile-interval": "HIVEMIND_RECONCILE_INTERVAL",
		"token":              "HIVEMIND_TOKEN",
	}
	for flag, want := range cases {
		if got := EnvName(flag); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", flag, got, want)
		}
	}
}

func TestDefaultsForRootAndUser(t *testing.T) {
	root := testOpts(map[string]string{"HOME": "/root"}, "")
	root.Getuid = func() int { return 0 }
	cfg, err := resolve(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/var/lib/hivemind" {
		t.Errorf("root data dir = %q", cfg.DataDir)
	}
	if cfg.ConfigFile != "/var/lib/hivemind/hivemind.yaml" {
		t.Errorf("root config file = %q", cfg.ConfigFile)
	}

	xdg := testOpts(map[string]string{"HOME": "/home/t", "XDG_STATE_HOME": "/state"}, "")
	cfg, err = resolve(nil, xdg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/state/hivemind" {
		t.Errorf("XDG data dir = %q", cfg.DataDir)
	}

	relXDG := testOpts(map[string]string{"HOME": "/home/t", "XDG_STATE_HOME": "relative"}, "")
	cfg, err = resolve(nil, relXDG)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/home/t/.local/state/hivemind" {
		t.Errorf("relative XDG should be ignored, got %q", cfg.DataDir)
	}
}

func TestDefaultsMatchPlan(t *testing.T) {
	cfg, err := Load(nil, testOpts(map[string]string{"HOME": "/home/t"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIAddr != "127.0.0.1:4483" || cfg.ClusterPort != 4484 || cfg.Advertise != "" {
		t.Errorf("listen defaults: %+v", cfg)
	}
	if cfg.LogLevel != "info" || cfg.LogFormat != "text" || cfg.MetricsAddr != "" {
		t.Errorf("log/metrics defaults: %+v", cfg)
	}
	if cfg.ReconcileInterval != 2*time.Second {
		t.Errorf("reconcile default = %s", cfg.ReconcileInterval)
	}
}

func TestUnknownKeyIsErrorWithLine(t *testing.T) {
	file := "log-level: info\n\napi-adr: 127.0.0.1:4483\n"
	_, err := Load(nil, testOpts(map[string]string{"HOME": "/home/t"}, file))
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	msg := err.Error()
	if !strings.Contains(msg, "api-adr") || !strings.Contains(msg, "line 3") {
		t.Errorf("error should name the key and line 3: %v", err)
	}
}

func TestExplicitMissingFileIsError(t *testing.T) {
	opts := testOpts(map[string]string{"HOME": "/home/t"}, "")
	opts.ReadFile = func(name string) ([]byte, error) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	fl := newFlags(t, opts)
	if err := fl.Set("config", "/etc/hivemind/missing.yaml"); err != nil {
		t.Fatal(err)
	}
	_, err := resolve(fl, opts)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("explicit --config missing: want ErrNotExist, got %v", err)
	}

	// The same through the environment variable.
	opts.Lookup = func(k string) (string, bool) {
		if k == "HIVEMIND_CONFIG" {
			return "/etc/hivemind/missing.yaml", true
		}
		return "", false
	}
	if _, err := resolve(nil, opts); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("HIVEMIND_CONFIG missing: want ErrNotExist, got %v", err)
	}
}

func TestDefaultMissingFileIsFine(t *testing.T) {
	opts := testOpts(map[string]string{"HOME": "/home/t"}, "")
	opts.ReadFile = func(name string) ([]byte, error) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	cfg, err := Load(nil, opts)
	if err != nil {
		t.Fatalf("missing default file should be fine: %v", err)
	}
	if cfg.ConfigFile != "/home/t/.local/state/hivemind/hivemind.yaml" {
		t.Errorf("config path = %q", cfg.ConfigFile)
	}
}

func TestOtherReadErrorIsReturned(t *testing.T) {
	opts := testOpts(map[string]string{"HOME": "/home/t"}, "")
	opts.ReadFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	if _, err := resolve(nil, opts); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("permission error on default file should surface, got %v", err)
	}
}

func TestEmptyFileIsValid(t *testing.T) {
	if _, err := Load(nil, testOpts(map[string]string{"HOME": "/home/t"}, "")); err != nil {
		t.Fatalf("empty file: %v", err)
	}
}

func TestFileDataDirAppliesButDoesNotMoveConfigLookup(t *testing.T) {
	opts := testOpts(map[string]string{"HOME": "/home/t"}, "data-dir: /srv/hv\n")
	var seen []string
	opts.ReadFile = func(name string) ([]byte, error) {
		seen = append(seen, name)
		return []byte("data-dir: /srv/hv\n"), nil
	}
	cfg, err := resolve(nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/srv/hv" {
		t.Errorf("file data-dir not applied: %q", cfg.DataDir)
	}
	if len(seen) != 1 || seen[0] != "/home/t/.local/state/hivemind/hivemind.yaml" {
		t.Errorf("config looked up at %v", seen)
	}
}

func TestTokenFromEnvIsLoaded(t *testing.T) {
	opts := testOpts(map[string]string{"HOME": "/home/t", "HIVEMIND_TOKEN": testToken}, "")
	cfg, err := Load(nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token.Value() != testToken {
		t.Errorf("token not loaded")
	}
}

// TestRedaction checks that no fmt verb and no JSON marshal of Config shows the token.
func TestRedaction(t *testing.T) {
	cfg, err := Load(nil, testOpts(map[string]string{"HOME": "/home/t", "HIVEMIND_TOKEN": testToken}, ""))
	if err != nil {
		t.Fatal(err)
	}
	outs := map[string]string{
		"%v":        fmt.Sprintf("%v", cfg),
		"%+v":       fmt.Sprintf("%+v", cfg),
		"%#v":       fmt.Sprintf("%#v", cfg),
		"ptr %v":    fmt.Sprintf("%v", &cfg),
		"token %v":  fmt.Sprintf("%v", cfg.Token),
		"token %+v": fmt.Sprintf("%+v", cfg.Token),
		"token %#v": fmt.Sprintf("%#v", cfg.Token),
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	outs["json"] = string(raw)
	for name, out := range outs {
		if strings.Contains(out, testToken) || strings.Contains(out, "s3cr3t") {
			t.Errorf("%s leaks the token: %s", name, out)
		}
	}
	if !strings.Contains(outs["%v"], "[redacted]") {
		t.Errorf("expected the mask in %%v output: %s", outs["%v"])
	}
}

func TestEmptyTokenPrintsEmpty(t *testing.T) {
	var s Secret
	if s.String() != "" {
		t.Errorf("empty secret printed %q", s.String())
	}
}

func TestFlagValueParseErrorNamesSource(t *testing.T) {
	opts := testOpts(map[string]string{"HOME": "/home/t", "HIVEMIND_CLUSTER_PORT": "many"}, "")
	_, err := resolve(nil, opts)
	if err == nil || !strings.Contains(err.Error(), "env HIVEMIND_CLUSTER_PORT") {
		t.Fatalf("parse error should name the source: %v", err)
	}
}

func TestYAMLScalarNumbersAndDurations(t *testing.T) {
	file := "cluster-port: 4499\nreconcile-interval: 7s\n"
	cfg, err := Load(nil, testOpts(map[string]string{"HOME": "/home/t"}, file))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClusterPort != 4499 || cfg.ReconcileInterval != 7*time.Second {
		t.Errorf("got port %d interval %s", cfg.ClusterPort, cfg.ReconcileInterval)
	}
}

func TestFileTooLarge(t *testing.T) {
	var fc fileConfig
	data := []byte("log-level: info\n# " + strings.Repeat("x", maxFileSize) + "\n")
	err := decodeFile(data, "big.yaml", &fc)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v, want a size-limit error", err)
	}
}
