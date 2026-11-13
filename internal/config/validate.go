package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Validate reports every rule the config breaks, joined into one error.
// A nil error means the config can be used as is.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.DataDir == "" {
		fail("data-dir is empty: set --data-dir, or HOME for a non-root user")
	}

	host, _, err := splitListen(c.APIAddr)
	switch {
	case err != nil:
		fail("--api-addr %q must be host:port, such as 127.0.0.1:4483 (%v)", c.APIAddr, err)
	case !isLoopback(host) && (c.APITLSCert == "" || c.APITLSKey == ""):
		fail("--api-addr %s needs --api-tls-cert and --api-tls-key, or bind 127.0.0.1", c.APIAddr)
	}
	if (c.APITLSCert == "") != (c.APITLSKey == "") {
		fail("--api-tls-cert and --api-tls-key must be set together")
	}

	if c.Advertise != "" {
		if ip, err := netip.ParseAddr(c.Advertise); err == nil {
			if ip.IsUnspecified() {
				fail("--advertise %s is a wildcard address: give an IP or host name that other nodes can reach", c.Advertise)
			}
		} else if !validHostname(c.Advertise) {
			fail("--advertise %q must be an IP address or a host name, without a port", c.Advertise)
		}
	}

	if c.MetricsAddr != "" {
		mh, _, err := splitListen(c.MetricsAddr)
		switch {
		case err != nil:
			fail("--metrics-addr %q must be host:port, such as 127.0.0.1:9100 (%v)", c.MetricsAddr, err)
		case !isLoopback(mh):
			fail("--metrics-addr %s must be a loopback address, such as 127.0.0.1:9100", c.MetricsAddr)
		}
	}

	if c.ClusterPort < 1 || c.ClusterPort > 65535 {
		fail("--cluster-port %d is not in 1-65535", c.ClusterPort)
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fail("--log-level %q must be one of debug, info, warn, error", c.LogLevel)
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		fail("--log-format %q must be text or json", c.LogFormat)
	}

	if c.ReconcileInterval <= 0 {
		fail("--reconcile-interval must be positive, got %s", c.ReconcileInterval)
	}
	if c.MinReady < 0 {
		fail("--min-ready must not be negative, got %s", c.MinReady)
	}

	if c.Server != "" {
		u, err := url.Parse(c.Server)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fail("--server %q must be an http or https URL", c.Server)
		}
	}

	return errors.Join(errs...)
}

// splitListen splits host:port and checks the port range.
func splitListen(addr string) (string, int, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, fmt.Errorf("port %q is not in 1-65535", port)
	}
	return host, n, nil
}

// isLoopback is true only for localhost and loopback IP literals. An empty
// host (a wildcard bind such as :4483), 0.0.0.0, :: and any other host name
// are not loopback.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// validHostname checks RFC 1123 host name syntax.
func validHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
			if !ok {
				return false
			}
		}
	}
	return true
}
