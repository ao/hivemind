package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/config"
)

// serveConfig is the config a plain serve with args resolves.
func serveConfig(t *testing.T, args ...string) *config.Config {
	t.Helper()
	fs := pflag.NewFlagSet("serve", pflag.ContinueOnError)
	config.Bind(fs, config.Options{})
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fs, config.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// init writes the serve flags it was given to <data-dir>/hivemind.yaml, with
// absolute certificate paths, so a serve given only the data directory runs
// with them, as the report's TLS setup expected.
func TestInitWritesTheServeFlags(t *testing.T) {
	home := isolate(t)
	t.Chdir(home)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ca := newTestCA(t, "test ca")
	certPEM, keyPEM := ca.issue(t, []net.IP{net.ParseIP("127.0.0.1")})
	writeFile(t, "api.crt", certPEM)
	writeFile(t, "api.key", keyPEM)
	data := filepath.Join(home, "data")

	out, _, err := run(t, "", "init", "--data-dir", data, "--api-addr", "127.0.0.1:4490", "--api-tls-cert", "api.crt",
		"--api-tls-key", "api.key", "--advertise", "127.0.0.1", "--min-ready", "5s")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(data, config.DefaultConfigName)
	if m := mode(t, file); m != 0o600 {
		t.Errorf("%s mode %04o, want 0600", file, m)
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), `api-tls-cert: "`+filepath.Join(wd, "api.crt")+`"`) || strings.Contains(string(b), "data-dir") {
		t.Fatalf("%s:\n%s", file, b)
	}
	if !strings.Contains(out, "(--api-addr, --advertise, --api-tls-cert, --api-tls-key, --min-ready) are in "+file) ||
		!strings.Contains(out, "Start the node with `hivemind serve --data-dir "+data+"`") {
		t.Fatalf("init output:\n%s", out)
	}
	cfg := serveConfig(t, "--data-dir", data)
	if cfg.APIAddr != "127.0.0.1:4490" || cfg.APITLSCert != filepath.Join(wd, "api.crt") || cfg.APITLSKey != filepath.Join(wd, "api.key") ||
		cfg.Advertise != "127.0.0.1" || cfg.MinReady != 5*time.Second {
		t.Fatalf("serve's config from the file: %+v", cfg)
	}

	// Without serve flags there is no file.
	plain := filepath.Join(home, "plain")
	if _, _, err := run(t, "", "init", "--data-dir", plain); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plain, config.DefaultConfigName)); err == nil {
		t.Fatal("init with no serve flags wrote a config file")
	}

	// A file given with --config exists, so serve flags are refused, and
	// the last lines name --config.
	cf := writeFile(t, filepath.Join(home, "hivemind.yaml"), []byte("log-level: debug\n"))
	if _, _, err := run(t, "", "init", "--data-dir", filepath.Join(home, "other"), "--config", cf, "--log-level", "warn"); err == nil ||
		!strings.Contains(err.Error(), cf+" exists") {
		t.Fatalf("init --config with serve flags: %v", err)
	}
	out, _, err = run(t, "", "init", "--data-dir", filepath.Join(home, "other"), "--config", cf)
	if err != nil || !strings.Contains(out, "Start the node with `hivemind serve --config "+cf+"`") {
		t.Fatalf("init --config: %q, %v", out, err)
	}
}

// A config file that exists is not changed: init refuses its serve flags,
// before it creates the cluster.
func TestInitRefusesFlagsOverAnExistingConfigFile(t *testing.T) {
	home := isolate(t)
	data := filepath.Join(home, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	file := writeFile(t, filepath.Join(data, config.DefaultConfigName), []byte("log-level: warn\n"))
	_, _, err := run(t, "", "init", "--data-dir", data, "--advertise", "127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), file+" exists, and init writes the serve settings it is given (--advertise) to it") {
		t.Fatalf("init over a config file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, auth.TokenFileName)); err == nil {
		t.Fatal("the refused init created a cluster")
	}
	if b, _ := os.ReadFile(file); string(b) != "log-level: warn\n" {
		t.Fatalf("the config file changed: %q", b)
	}
	// Without flags, init uses the file as it is.
	if _, _, err := run(t, "", "init", "--data-dir", data); err != nil {
		t.Fatalf("init with the file and no flags: %v", err)
	}
}

// init's last lines offer the CLI on another machine only an API it can
// reach: not a loopback one, and for a wildcard one, with a placeholder for
// this machine's address.
func TestInitSaysWhereTheCLICanReachTheAPI(t *testing.T) {
	home := isolate(t)
	ca := newTestCA(t, "test ca")
	certPEM, keyPEM := ca.issue(t, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5")})
	cert := writeFile(t, filepath.Join(home, "api.crt"), certPEM)
	key := writeFile(t, filepath.Join(home, "api.key"), keyPEM)
	for i, c := range []struct {
		flags []string
		want  []string
		not   string
	}{
		{nil, []string{"From another shell on this machine, use", "export HIVEMIND_SERVER=http://127.0.0.1:4483",
			"The API listens on 127.0.0.1:4483, a loopback address, so the CLI on another machine cannot reach it"}, "From another shell or machine"},
		{[]string{"--api-addr", "0.0.0.0:4483", "--api-tls-cert", cert, "--api-tls-key", key},
			[]string{"From another shell or machine, use", "export HIVEMIND_SERVER=https://<an address of this machine>:4483",
				"export HIVEMIND_CA_CERT=<a copy of " + cert}, "loopback"},
		{[]string{"--api-addr", "10.0.0.5:4483", "--api-tls-cert", cert, "--api-tls-key", key},
			[]string{"From another shell or machine, use", "export HIVEMIND_SERVER=https://10.0.0.5:4483"}, "loopback"},
	} {
		args := append([]string{"init", "--data-dir", filepath.Join(home, fmt.Sprintf("d%d", i)), "--client-config",
			filepath.Join(home, fmt.Sprintf("c%d.yaml", i))}, c.flags...)
		out, _, err := run(t, "", args...)
		if err != nil {
			t.Fatalf("%v: %v", c.flags, err)
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: no %q in:\n%s", c.flags, w, out)
			}
		}
		if strings.Contains(out, c.not) {
			t.Errorf("%v: %q in:\n%s", c.flags, c.not, out)
		}
	}
}
