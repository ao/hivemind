package config

import (
	"github.com/ao/hivemind/internal/scheduler"
	"strings"
	"testing"
	"time"
)

// validConfig is a config that passes Validate.
func validConfig() *Config {
	return &Config{
		DataDir:           "/var/lib/hivemind",
		APIAddr:           "127.0.0.1:4483",
		ClusterPort:       4484,
		DockerHost:        "unix:///var/run/docker.sock",
		LogLevel:          "info",
		LogFormat:         "text",
		ReconcileInterval: 2 * time.Second,
		Server:            "http://127.0.0.1:4483",
	}
}

func TestValidConfigPasses(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidate(t *testing.T) {
	type tc struct {
		name   string
		mutate func(c *Config)
		want   string // substring of the error; "" means valid
	}
	cases := []tc{
		// api-addr
		{"api host:port loopback ok", func(c *Config) { c.APIAddr = "127.0.0.1:4483" }, ""},
		{"api localhost ok", func(c *Config) { c.APIAddr = "localhost:4483" }, ""},
		{"api 127.0.0.2 is loopback", func(c *Config) { c.APIAddr = "127.0.0.2:4483" }, ""},
		{"api ::1 is loopback", func(c *Config) { c.APIAddr = "[::1]:4483" }, ""},
		{"api IPv4-mapped loopback ok", func(c *Config) { c.APIAddr = "[::ffff:127.0.0.1]:4483" }, ""},
		{"api missing port", func(c *Config) { c.APIAddr = "127.0.0.1" }, "must be host:port"},
		{"api bare port", func(c *Config) { c.APIAddr = "4483" }, "must be host:port"},
		{"api port 0", func(c *Config) { c.APIAddr = "127.0.0.1:0" }, "not in 1-65535"},
		{"api port too big", func(c *Config) { c.APIAddr = "127.0.0.1:70000" }, "not in 1-65535"},
		{"api port not a number", func(c *Config) { c.APIAddr = "127.0.0.1:http" }, "not in 1-65535"},
		{"api 0.0.0.0 without TLS", func(c *Config) { c.APIAddr = "0.0.0.0:4483" },
			"--api-addr 0.0.0.0:4483 needs --api-tls-cert and --api-tls-key, or bind 127.0.0.1"},
		{"api [::] without TLS", func(c *Config) { c.APIAddr = "[::]:4483" },
			"needs --api-tls-cert and --api-tls-key"},
		{"api empty host is wildcard", func(c *Config) { c.APIAddr = ":4483" },
			"needs --api-tls-cert and --api-tls-key"},
		{"api hostname not localhost", func(c *Config) { c.APIAddr = "example.test:4483" },
			"needs --api-tls-cert and --api-tls-key"},
		{"api LAN address without TLS", func(c *Config) { c.APIAddr = "10.0.0.5:4483" },
			"needs --api-tls-cert and --api-tls-key"},
		{"api 0.0.0.0 with TLS ok", func(c *Config) {
			c.APIAddr = "0.0.0.0:4483"
			c.APITLSCert, c.APITLSKey = "/tls/cert.pem", "/tls/key.pem"
		}, ""},
		{"api hostname with TLS ok", func(c *Config) {
			c.APIAddr = "example.test:4483"
			c.APITLSCert, c.APITLSKey = "/tls/cert.pem", "/tls/key.pem"
		}, ""},
		{"TLS cert without key", func(c *Config) {
			c.APIAddr = "127.0.0.1:4483"
			c.APITLSCert = "/tls/cert.pem"
		}, "must be set together"},
		{"TLS key without cert", func(c *Config) {
			c.APIAddr = "127.0.0.1:4483"
			c.APITLSKey = "/tls/key.pem"
		}, "must be set together"},

		// advertise
		{"advertise empty means no listener", func(c *Config) { c.Advertise = "" }, ""},
		{"advertise IPv4", func(c *Config) { c.Advertise = "10.0.0.5" }, ""},
		{"advertise IPv6", func(c *Config) { c.Advertise = "fd00::5" }, ""},
		{"advertise host name", func(c *Config) { c.Advertise = "node-1.example.test" }, ""},
		{"advertise 0.0.0.0", func(c *Config) { c.Advertise = "0.0.0.0" }, "wildcard address"},
		{"advertise ::", func(c *Config) { c.Advertise = "::" }, "wildcard address"},
		{"advertise [::]", func(c *Config) { c.Advertise = "[::]" }, "must be an IP address or a host name"},
		{"advertise with port", func(c *Config) { c.Advertise = "10.0.0.5:4484" }, "without a port"},
		{"advertise underscore", func(c *Config) { c.Advertise = "bad_host" }, "must be an IP address or a host name"},
		{"advertise label starts with dash", func(c *Config) { c.Advertise = "-node.test" }, "must be an IP address or a host name"},

		// metrics-addr
		{"metrics empty ok", func(c *Config) { c.MetricsAddr = "" }, ""},
		{"metrics loopback ok", func(c *Config) { c.MetricsAddr = "127.0.0.1:9100" }, ""},
		{"metrics ::1 ok", func(c *Config) { c.MetricsAddr = "[::1]:9100" }, ""},
		{"metrics 127.0.0.2 ok", func(c *Config) { c.MetricsAddr = "127.0.0.2:9100" }, ""},
		{"metrics 0.0.0.0 rejected", func(c *Config) { c.MetricsAddr = "0.0.0.0:9100" }, "must be a loopback address"},
		{"metrics wildcard rejected", func(c *Config) { c.MetricsAddr = ":9100" }, "must be a loopback address"},
		{"metrics hostname rejected", func(c *Config) { c.MetricsAddr = "example.test:9100" }, "must be a loopback address"},
		{"metrics missing port", func(c *Config) { c.MetricsAddr = "127.0.0.1" }, "must be host:port"},

		// cluster port
		{"cluster port 0", func(c *Config) { c.ClusterPort = 0 }, "not in 1-65534"},
		{"cluster port too big", func(c *Config) { c.ClusterPort = 65536 }, "not in 1-65534"},

		// log settings
		{"log level debug ok", func(c *Config) { c.LogLevel = "debug" }, ""},
		{"log level warn ok", func(c *Config) { c.LogLevel = "warn" }, ""},
		{"log level error ok", func(c *Config) { c.LogLevel = "error" }, ""},
		{"log level trace rejected", func(c *Config) { c.LogLevel = "trace" }, "must be one of"},
		{"log level upper case rejected", func(c *Config) { c.LogLevel = "INFO" }, "must be one of"},
		{"log format json ok", func(c *Config) { c.LogFormat = "json" }, ""},
		{"log format xml rejected", func(c *Config) { c.LogFormat = "xml" }, "must be text or json"},

		// durations
		{"reconcile zero rejected", func(c *Config) { c.ReconcileInterval = 0 }, "must be positive"},
		{"reconcile negative rejected", func(c *Config) { c.ReconcileInterval = -time.Second }, "must be positive"},
		{"reconcile 5s ok", func(c *Config) { c.ReconcileInterval = 5 * time.Second }, ""},
		{"reconcile above the lease budget rejected", func(c *Config) { c.ReconcileInterval = 6 * time.Second }, "longer than 5s"},
		{"cluster port 65534 ok", func(c *Config) { c.ClusterPort = 65534 }, ""},
		{"cluster port 65535 leaves no join port", func(c *Config) { c.ClusterPort = 65535 }, "not in 1-65534"},

		// server and data dir
		{"server http ok", func(c *Config) { c.Server = "http://10.0.0.5:4483" }, ""},
		{"server https ok", func(c *Config) { c.Server = "https://hv.example.test" }, ""},
		{"server empty ok", func(c *Config) { c.Server = "" }, ""},
		{"server ftp rejected", func(c *Config) { c.Server = "ftp://x" }, "must be an http or https URL"},
		{"server no scheme rejected", func(c *Config) { c.Server = "127.0.0.1:4483" }, "must be an http or https URL"},
		{"data dir empty rejected", func(c *Config) { c.DataDir = "" }, "data-dir is empty"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want valid, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

// TestValidateReportsAllProblems checks that errors are joined, not just the first.
func TestValidateReportsAllProblems(t *testing.T) {
	c := validConfig()
	c.APIAddr = "0.0.0.0:4483"
	c.Advertise = "::"
	c.LogLevel = "loud"
	err := c.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"needs --api-tls-cert", "wildcard address", "--log-level"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

// TestLoadRejectsInvalid checks that Load runs Validate.
func TestLoadRejectsInvalid(t *testing.T) {
	opts := testOpts(map[string]string{"HOME": "/home/t", "HIVEMIND_API_ADDR": "0.0.0.0:4483"}, "")
	if _, err := Load(nil, opts); err == nil || !strings.Contains(err.Error(),
		"--api-addr 0.0.0.0:4483 needs --api-tls-cert and --api-tls-key, or bind 127.0.0.1") {
		t.Fatalf("Load should fail with the fix-it message, got %v", err)
	}
}

// The reconcile bound must leave room for two missed passes within the lease.
func TestMaxReconcileIntervalFitsTheLease(t *testing.T) {
	if 3*MaxReconcileInterval > scheduler.DefaultLeaseTimeout {
		t.Fatalf("3 x %s exceeds the %s lease timeout", MaxReconcileInterval, scheduler.DefaultLeaseTimeout)
	}
}
