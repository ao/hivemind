package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
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
	// The cluster listeners bind one address, never a wildcard. serve checks
	// that --advertise goes with it; join refuses it.
	if c.ClusterAddr != "" {
		ip, err := netip.ParseAddr(c.ClusterAddr)
		switch {
		case err != nil || ip.Zone() != "":
			fail("--cluster-addr %q must be an IP address of this machine, without a port", c.ClusterAddr)
		case ip.IsUnspecified():
			fail("--cluster-addr %s is a wildcard address: give the address of this machine that workers reach", c.ClusterAddr)
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

	// The join listener takes the next port, so the cluster port stops at 65534.
	if c.ClusterPort < 1 || c.ClusterPort > 65534 {
		fail("--cluster-port %d is not in 1-65534 (the join listener uses the next port)", c.ClusterPort)
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
	} else if c.ReconcileInterval > MaxReconcileInterval {
		fail("--reconcile-interval %s is longer than %s: each pass renews the node's lease, and a node that misses its lease for 15 s is treated as lost", c.ReconcileInterval, MaxReconcileInterval)
	}
	if c.MinReady < 0 {
		fail("--min-ready must not be negative, got %s", c.MinReady)
	}

	// The router is the one listener that may bind every address: it serves
	// the apps' public traffic. It has no default address.
	if c.RouteAddr != "" {
		if ip, err := netip.ParseAddr(c.RouteAddr); err != nil || ip.Zone() != "" {
			fail("--route-addr %q must be an IP address, such as 0.0.0.0 or 10.0.0.5, without a port", c.RouteAddr)
		}
		if c.ACMEEmail == "" {
			fail("--route-addr needs --acme-email, the contact of the ACME account that gets the routers' certificates")
		}
	}
	if c.ACMEEmail != "" && !validEmail(c.ACMEEmail) {
		fail("--acme-email %q is not an email address", c.ACMEEmail)
	}
	if u, err := url.Parse(c.ACMECA); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		fail("--acme-ca %q must be an https URL of an ACME directory", c.ACMECA)
	}
	for _, p := range []struct {
		flag string
		port int
	}{{"--route-http-port", c.RouteHTTPPort}, {"--route-https-port", c.RouteHTTPSPort}} {
		if p.port < 1 || p.port > 65535 {
			fail("%s %d is not in 1-65535", p.flag, p.port)
		}
	}
	if c.RouteHTTPPort == c.RouteHTTPSPort {
		fail("--route-http-port and --route-https-port must differ, both are %d", c.RouteHTTPPort)
	}
	if c.RouteMaxConnsPerIP < 0 {
		fail("--route-max-conns-per-ip must be 0 (no limit) or more, got %d", c.RouteMaxConnsPerIP)
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

// validEmail is a light check of an ACME contact: one @ with text on both
// sides, a dot in the domain, no spaces or control characters, at most 254
// bytes. The CA does its own check.
func validEmail(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || len(s) > 254 || strings.Contains(domain, "@") || !strings.Contains(domain, ".") {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) < 0 && validHostname(domain)
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

// MaxReconcileInterval bounds --reconcile-interval. A worker renews its lease
// once per reconcile pass and the server treats a node as lost after 15 s
// without a renewal (scheduler.DefaultLeaseTimeout), so a pass at most every
// 5 s survives two missed passes.
const MaxReconcileInterval = 5 * time.Second
