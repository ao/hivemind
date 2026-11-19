// Package config reads the node and client settings. For each setting a flag
// beats a HIVEMIND_* environment variable, which beats the YAML config file,
// which beats the built-in default.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

const (
	// DefaultConfigName is the config file name looked up in the data directory
	// when --config is not given.
	DefaultConfigName = "hivemind.yaml"

	secretMask = "[redacted]"
)

// Secret is a string that prints as [redacted] in every fmt verb and in JSON.
// Read it with Value, and only pass it to code that needs the real text.
type Secret string

// Value returns the secret text.
func (s Secret) Value() string { return string(s) }

// String implements fmt.Stringer. An empty secret prints as empty so an unset
// token is visible; a set one prints as [redacted].
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return secretMask
}

// GoString implements fmt.GoStringer for %#v.
func (s Secret) GoString() string { return strconv.Quote(s.String()) }

// MarshalText writes the masked form.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// MarshalJSON writes the masked form.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// Config is the resolved settings for one hivemind process or client call.
type Config struct {
	ConfigFile        string        `json:"config_file"`
	DataDir           string        `json:"data_dir"`
	APIAddr           string        `json:"api_addr"`
	Advertise         string        `json:"advertise"`
	ClusterAddr       string        `json:"cluster_addr"`
	ClusterPort       int           `json:"cluster_port"`
	APITLSCert        string        `json:"api_tls_cert"`
	APITLSKey         string        `json:"api_tls_key"`
	DockerHost        string        `json:"docker_host"`
	LogLevel          string        `json:"log_level"`
	LogFormat         string        `json:"log_format"`
	MetricsAddr       string        `json:"metrics_addr"`
	ReconcileInterval time.Duration `json:"reconcile_interval"`
	MinReady          time.Duration `json:"min_ready"`
	// StartGrace is how long after serve starts a worker that was up before
	// the start, and has not reported since, waits rather than being lost.
	StartGrace     time.Duration `json:"start_grace"`
	RouteAddr      string        `json:"route_addr"`
	RouteHTTPPort  int           `json:"route_http_port"`
	RouteHTTPSPort int           `json:"route_https_port"`
	// RouteMaxConnsPerIP caps the router's connections from one client
	// address; 0 means no cap.
	RouteMaxConnsPerIP int    `json:"route_max_conns_per_ip"`
	ACMEEmail          string `json:"acme_email"`
	ACMECA             string `json:"acme_ca"`
	Server             string `json:"server"`
	Token              Secret `json:"token"`
	// sources is where each setting that is not at its default came from
	// (Source). Only resolve sets it.
	sources map[string]string
}

// Source says where the setting name came from: "flag --name", "env
// HIVEMIND_...", or "config file <path>". It is "" for a setting at its
// default.
func (c *Config) Source(name string) string { return c.sources[name] }

// DefaultACMECA is the default ACME directory: Let's Encrypt's production CA.
const DefaultACMECA = "https://acme-v02.api.letsencrypt.org/directory"

// Options injects the process environment so tests need no os.Setenv.
// A nil field means the os package function.
type Options struct {
	// Lookup reads an environment variable.
	Lookup func(key string) (string, bool)
	// ReadFile reads the config file.
	ReadFile func(name string) ([]byte, error)
	// Getuid returns the effective user ID.
	Getuid func() int
}

func (o Options) withDefaults() Options {
	if o.Lookup == nil {
		o.Lookup = os.LookupEnv
	}
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	if o.Getuid == nil {
		o.Getuid = os.Getuid
	}
	return o
}

// EnvName returns the environment variable for a flag name, such as
// HIVEMIND_API_ADDR for api-addr.
func EnvName(flag string) string {
	return "HIVEMIND_" + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}

// setting describes one option: its flag, its default, how the YAML file
// names it, and how a string value is stored in Config.
type setting struct {
	name  string
	usage string
	def   string
	file  func(*fileConfig) *string
	set   func(*Config, string) error
	// client marks a setting of the client commands, which take it from
	// their own flags. Bind leaves it out.
	client bool
}

// Bind registers one string flag per node setting on fs: every setting but
// server and token, which only the client commands take, from flags of
// their own. The flag defaults are the built-in defaults. Values are parsed
// when Load reads them. Load still reads a server or token flag that a
// command registers itself, and the HIVEMIND_SERVER and HIVEMIND_TOKEN
// environment variables.
func Bind(fs *pflag.FlagSet, opts Options) {
	for _, s := range settings(opts.withDefaults()) {
		if !s.client {
			fs.String(s.name, s.def, s.usage)
		}
	}
}

// workerSettings are the settings a worker reads, for join: its data
// directory and config file, its Docker daemon, its log, its agent's
// interval and its router. advertise and cluster-addr are there too, so
// that join can refuse them with the reason, rather than as unknown flags.
var workerSettings = map[string]bool{
	"config": true, "data-dir": true, "docker-host": true, "log-level": true, "log-format": true, "reconcile-interval": true,
	"route-addr": true, "route-http-port": true, "route-https-port": true, "route-max-conns-per-ip": true,
	"acme-email": true, "acme-ca": true,
	"advertise": true, "cluster-addr": true,
}

// BindWorker binds the flags of the settings a worker reads, for join. The
// server's settings, such as --api-addr or --start-grace, are left out. Load
// still reads every setting from the environment and the config file.
func BindWorker(fs *pflag.FlagSet, opts Options) {
	for _, s := range settings(opts.withDefaults()) {
		if !s.client && workerSettings[s.name] {
			fs.String(s.name, s.def, s.usage)
		}
	}
}

// Load resolves the settings and validates them. fs may be nil when no flags
// were bound.
func Load(fs *pflag.FlagSet, opts Options) (*Config, error) {
	cfg, err := resolve(fs, opts.withDefaults())
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// layer is one explicit source of a value: a flag or an environment variable.
type layer struct {
	value  string
	source string
}

// resolve applies the precedence rules without validating the result.
func resolve(fl *pflag.FlagSet, opts Options) (*Config, error) {
	ss := settings(opts)

	explicit := make(map[string]layer, len(ss))
	for _, s := range ss {
		if l, ok := explicitValue(fl, opts, s.name); ok {
			explicit[s.name] = l
		}
	}

	// The config file's location depends on the data directory, so the data
	// directory is taken from the flag, env or default. A data-dir key in the
	// file still applies to the resolved config.
	dataDir := byName(ss, "data-dir").def
	if l, ok := explicit["data-dir"]; ok {
		dataDir = l.value
	}
	cfgPath, explicitPath := filepath.Join(dataDir, DefaultConfigName), false
	if l, ok := explicit["config"]; ok && l.value != "" {
		cfgPath, explicitPath = l.value, true
	}

	var fc fileConfig
	data, err := opts.ReadFile(cfgPath)
	switch {
	case err == nil:
		if err := decodeFile(data, cfgPath, &fc); err != nil {
			return nil, err
		}
	case errors.Is(err, fs.ErrNotExist) && !explicitPath:
		// A missing default file is fine: the defaults apply.
	default:
		return nil, fmt.Errorf("read config file %s: %w", cfgPath, err)
	}

	cfg := &Config{}
	for _, s := range ss {
		val, src := s.def, "default"
		if s.file != nil {
			if p := s.file(&fc); p != nil {
				val, src = *p, "config file "+cfgPath
			}
		}
		if l, ok := explicit[s.name]; ok {
			val, src = l.value, l.source
		}
		if err := s.set(cfg, val); err != nil {
			return nil, fmt.Errorf("%s (from %s): %w", s.name, src, err)
		}
		if src != "default" {
			if cfg.sources == nil {
				cfg.sources = map[string]string{}
			}
			cfg.sources[s.name] = src
		}
	}
	cfg.ConfigFile = cfgPath
	return cfg, nil
}

func explicitValue(fl *pflag.FlagSet, opts Options, name string) (layer, bool) {
	if fl != nil {
		if f := fl.Lookup(name); f != nil && f.Changed {
			return layer{f.Value.String(), "flag --" + name}, true
		}
	}
	env := EnvName(name)
	if v, ok := opts.Lookup(env); ok {
		return layer{v, "env " + env}, true
	}
	return layer{}, false
}

func byName(ss []setting, name string) setting {
	for _, s := range ss {
		if s.name == name {
			return s
		}
	}
	panic("config: no setting named " + name)
}

// DataDir is the data directory of a command that takes no --data-dir, such
// as the client commands: HIVEMIND_DATA_DIR, or the default for this user.
func DataDir() string {
	opts := Options{}.withDefaults()
	if v, ok := opts.Lookup(EnvName("data-dir")); ok && v != "" {
		return v
	}
	return defaultDataDir(opts)
}

// defaultDataDir is /var/lib/hivemind for root, and $XDG_STATE_HOME/hivemind
// or ~/.local/state/hivemind for everyone else.
func defaultDataDir(opts Options) string {
	if opts.Getuid() == 0 {
		return "/var/lib/hivemind"
	}
	if x, ok := opts.Lookup("XDG_STATE_HOME"); ok && filepath.IsAbs(x) {
		return filepath.Join(x, "hivemind")
	}
	if h, ok := opts.Lookup("HOME"); ok && h != "" {
		return filepath.Join(h, ".local", "state", "hivemind")
	}
	return ""
}

// settings lists every option. Every flag name is also its YAML key.
func settings(opts Options) []setting {
	return []setting{
		str("config", "YAML config file (default <data-dir>/hivemind.yaml)", "",
			func(c *Config) *string { return &c.ConfigFile }, nil),
		str("data-dir", "directory for node state", defaultDataDir(opts),
			func(c *Config) *string { return &c.DataDir },
			func(f *fileConfig) *string { return f.DataDir }),
		str("api-addr", "address of the API, host:port", "127.0.0.1:4483",
			func(c *Config) *string { return &c.APIAddr },
			func(f *fileConfig) *string { return f.APIAddr }),
		str("advertise", "IP or host name other nodes use to reach this server; serve opens the cluster listeners on it", "",
			func(c *Config) *string { return &c.Advertise },
			func(f *fileConfig) *string { return f.Advertise }),
		str("cluster-addr", "IP address of this machine the cluster listeners bind; default: the address --advertise names or resolves to", "",
			func(c *Config) *string { return &c.ClusterAddr },
			func(f *fileConfig) *string { return f.ClusterAddr }),
		num("cluster-port", "port of the node-to-node listener; the join listener uses the port above it", "4484",
			func(c *Config) *int { return &c.ClusterPort },
			func(f *fileConfig) *string { return f.ClusterPort }),
		str("api-tls-cert", "TLS certificate for the API (PEM)", "",
			func(c *Config) *string { return &c.APITLSCert },
			func(f *fileConfig) *string { return f.APITLSCert }),
		str("api-tls-key", "TLS private key for the API (PEM)", "",
			func(c *Config) *string { return &c.APITLSKey },
			func(f *fileConfig) *string { return f.APITLSKey }),
		str("docker-host", "Docker Engine endpoint", "unix:///var/run/docker.sock",
			func(c *Config) *string { return &c.DockerHost },
			func(f *fileConfig) *string { return f.DockerHost }),
		str("log-level", "log level: debug, info, warn or error", "info",
			func(c *Config) *string { return &c.LogLevel },
			func(f *fileConfig) *string { return f.LogLevel }),
		str("log-format", "log format: text or json", "text",
			func(c *Config) *string { return &c.LogFormat },
			func(f *fileConfig) *string { return f.LogFormat }),
		str("metrics-addr", "separate loopback address for /metrics, host:port; empty means API only", "",
			func(c *Config) *string { return &c.MetricsAddr },
			func(f *fileConfig) *string { return f.MetricsAddr }),
		dur("reconcile-interval", "how often the agent reconciles containers and renews its lease (at most 5s)", "2s",
			func(c *Config) *time.Duration { return &c.ReconcileInterval },
			func(f *fileConfig) *string { return f.ReconcileInterval }),
		dur("min-ready", "how long a node must be ready before it gets new replicas; 0s means no wait", "30s",
			func(c *Config) *time.Duration { return &c.MinReady },
			func(f *fileConfig) *string { return f.MinReady }),
		// The default is store.DefaultStartGrace.
		dur("start-grace", "how long after serve starts a worker that was up before it waits for its first report, keeping its replicas and its routes, before it is lost; when no such worker has reported by then, they all wait until one does, and the others that long again from then; at least 15s", "2m",
			func(c *Config) *time.Duration { return &c.StartGrace },
			func(f *fileConfig) *string { return f.StartGrace }),
		str("route-addr", "IP address the HTTPS router listens on, such as 0.0.0.0 for every interface; empty means no router on this node", "",
			func(c *Config) *string { return &c.RouteAddr },
			func(f *fileConfig) *string { return f.RouteAddr }),
		num("route-http-port", "port of the router's HTTP listener, for ACME HTTP-01 challenges and redirects to https", "80",
			func(c *Config) *int { return &c.RouteHTTPPort },
			func(f *fileConfig) *string { return f.RouteHTTPPort }),
		num("route-https-port", "port of the router's HTTPS listener", "443",
			func(c *Config) *int { return &c.RouteHTTPSPort },
			func(f *fileConfig) *string { return f.RouteHTTPSPort }),
		// The default is router.DefaultMaxConnsPerAddr.
		num("route-max-conns-per-ip", "most connections the router keeps open from one client IP address (an IPv6 /64), over both its ports; 0 means no limit, for a router behind a proxy that does not pass client addresses", "256",
			func(c *Config) *int { return &c.RouteMaxConnsPerIP },
			func(f *fileConfig) *string { return f.RouteMaxConnsPerIP }),
		str("acme-email", "contact email of the ACME account; required with --route-addr, and setting it accepts the CA's terms", "",
			func(c *Config) *string { return &c.ACMEEmail },
			func(f *fileConfig) *string { return f.ACMEEmail }),
		str("acme-ca", "ACME directory URL the routers get certificates from", DefaultACMECA,
			func(c *Config) *string { return &c.ACMECA },
			func(f *fileConfig) *string { return f.ACMECA }),
		clientSetting(str("server", "server URL for the client commands", "http://127.0.0.1:4483",
			func(c *Config) *string { return &c.Server },
			func(f *fileConfig) *string { return f.Server })),
		{
			name:   "token",
			usage:  "API token for the client commands (not logged; never read from the config file)",
			set:    func(c *Config, v string) error { c.Token = Secret(v); return nil },
			client: true,
		},
	}
}

func clientSetting(s setting) setting {
	s.client = true
	return s
}

func str(name, usage, def string, field func(*Config) *string, file func(*fileConfig) *string) setting {
	return setting{name: name, usage: usage, def: def, file: file,
		set: func(c *Config, v string) error { *field(c) = v; return nil }}
}

func num(name, usage, def string, field func(*Config) *int, file func(*fileConfig) *string) setting {
	return setting{name: name, usage: usage, def: def, file: file,
		set: func(c *Config, v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%q is not an integer", v)
			}
			*field(c) = n
			return nil
		}}
}

func dur(name, usage, def string, field func(*Config) *time.Duration, file func(*fileConfig) *string) setting {
	return setting{name: name, usage: usage, def: def, file: file,
		set: func(c *Config, v string) error {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%q is not a duration such as 2s", v)
			}
			*field(c) = d
			return nil
		}}
}
