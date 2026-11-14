// Package router is the HTTPS router that runs on every node that opts in.
// It serves the routing table that the server builds from the replicas'
// reports: for each host, the private address and ephemeral port of every
// Ready replica. Requests for a host go round-robin to its replicas through
// net/http/httputil.ReverseProxy; a replica that cannot be dialled is
// skipped for a few seconds, and the next one is tried.
//
// Certificates come from an ACME CA through certmagic, for the hosts in the
// table only: there is no on-demand issuance. HTTP-01 is the only challenge.
// The port 80 listener answers it, and redirects every other request for a
// known host to https. Certificates, keys, the ACME account and challenge
// tokens are in storage shared through the server (internal/tlsstore), so
// any router can answer a challenge another one started, and a lock lets one
// router issue each certificate.
//
// The router is the one listener in hivemind that may bind every address:
// it serves the apps' own public traffic, without authentication, which is
// its purpose. Its address has no default; without one there is no router.
package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/ao/hivemind/internal/metrics"
)

// Server limits for the router's listeners.
const (
	readHeaderTimeout = 10 * time.Second
	// readTimeout bounds the read of a whole request, body included, so a
	// slow client cannot hold a connection open for ever. Uploads longer
	// than this are cut off.
	readTimeout    = 5 * time.Minute
	idleTimeout    = 2 * time.Minute
	maxHeaderBytes = 64 << 10
	// DefaultPoll is how often the router reads the routing table.
	DefaultPoll     = time.Second
	shutdownTimeout = 10 * time.Second
)

// LetsEncrypt is the default ACME directory.
const LetsEncrypt = certmagic.LetsEncryptProductionCA

// Config configures New.
type Config struct {
	// Addr is the IP address both listeners bind. It may be a wildcard.
	Addr string
	// HTTPPort and HTTPSPort are the ports. Zero picks a free port (tests).
	HTTPPort, HTTPSPort int
	// Routes reads the routing table.
	Routes func(ctx context.Context) (Table, error)
	// Storage is the shared TLS storage.
	Storage certmagic.Storage
	// Email is the ACME account's contact. Registering the account agrees
	// to the CA's subscriber agreement.
	Email string
	// CA is the ACME directory URL. Empty means LetsEncrypt.
	CA string
	// CARoots, if set, are the roots trusted for the CA's own HTTPS. Nil
	// means the system roots.
	CARoots *x509.CertPool
	// Poll is how often the table is read. Zero means DefaultPoll.
	Poll    time.Duration
	Logger  *slog.Logger
	Metrics *Metrics
}

// Router is a pair of bound listeners and the certificate manager.
type Router struct {
	cfg     Config
	log     *slog.Logger
	proxy   *proxy
	magic   *certmagic.Config
	cache   *certmagic.Cache
	issuer  *certmagic.ACMEIssuer
	httpLn  net.Listener
	tlsLn   net.Listener
	httpSrv *http.Server
	tlsSrv  *http.Server

	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// manage starts certificate management for hosts: the certmagic
	// config's ManageAsync. Tests replace it, under mu.
	manage  func(ctx context.Context, hosts []string) error
	managed map[string]bool
}

// New validates cfg, binds both listeners and sets up certmagic. Nothing is
// served, and no certificate is asked for, until Serve.
func New(cfg Config) (*Router, error) {
	ip, err := netip.ParseAddr(cfg.Addr)
	if err != nil || ip.Zone() != "" {
		return nil, fmt.Errorf("router: address %q is not an IP address", cfg.Addr)
	}
	if cfg.Routes == nil || cfg.Storage == nil {
		return nil, errors.New("router: Routes and Storage are required")
	}
	if cfg.Email == "" {
		return nil, errors.New("router: an ACME email is required")
	}
	if cfg.HTTPPort < 0 || cfg.HTTPPort > 65535 || cfg.HTTPSPort < 0 || cfg.HTTPSPort > 65535 {
		return nil, errors.New("router: ports must be in 0-65535")
	}
	if cfg.CA == "" {
		cfg.CA = LetsEncrypt
	}
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultPoll
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	rt := &Router{cfg: cfg, log: log, managed: map[string]bool{}}
	rt.proxy = newProxy(log, cfg.Metrics)

	// The listeners are bound before certmagic is told about any host, so
	// its HTTP-01 solver finds the port taken and leaves the challenge to
	// the port 80 handler: it tries to bind ListenHost:AltHTTPPort, which is
	// this same address, and on "address already in use" assumes the
	// listener there answers challenges (robustTryListen in solvers.go).
	rt.httpLn, err = net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(cfg.HTTPPort)))
	if err != nil {
		return nil, fmt.Errorf("router: http listener: %w", err)
	}
	rt.tlsLn, err = net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(cfg.HTTPSPort)))
	if err != nil {
		rt.httpLn.Close()
		return nil, fmt.Errorf("router: https listener: %w", err)
	}

	zl := newZap(log.With("component", "certmagic"))
	rt.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return rt.magic, nil },
		Logger:           zl,
	})
	rt.magic = certmagic.New(rt.cache, certmagic.Config{
		Storage: cfg.Storage,
		Logger:  zl,
		// No OCSP fetches: Let's Encrypt ended its OCSP service in 2025,
		// and stapling would make every node call out to the responder.
		OCSP: certmagic.OCSPConfig{DisableStapling: true},
	})
	rt.issuer = certmagic.NewACMEIssuer(rt.magic, certmagic.ACMEIssuer{
		CA:     cfg.CA,
		Email:  cfg.Email,
		Agreed: true,
		// HTTP-01 only, on the port 80 listener.
		DisableTLSALPNChallenge: true,
		ListenHost:              ip.String(),
		AltHTTPPort:             rt.HTTPPort(),
		TrustedRoots:            cfg.CARoots,
		Logger:                  zl,
	})
	rt.magic.Issuers = []certmagic.Issuer{rt.issuer}
	rt.manage = rt.magic.ManageAsync

	rt.httpSrv = &http.Server{
		Handler:           counted(cfg.Metrics, rt.proxy.known, http.HandlerFunc(rt.serveHTTP)),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	rt.tlsSrv = &http.Server{
		Handler:           counted(cfg.Metrics, rt.proxy.known, rt.proxy),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: rt.getCertificate,
			NextProtos:     []string{"h2", "http/1.1"},
		},
	}
	rt.ctx, rt.cancel = context.WithCancel(context.Background())
	return rt, nil
}

// HTTPAddr and HTTPSAddr are the bound addresses.
func (rt *Router) HTTPAddr() string  { return rt.httpLn.Addr().String() }
func (rt *Router) HTTPSAddr() string { return rt.tlsLn.Addr().String() }

// HTTPPort is the bound port of the http listener.
func (rt *Router) HTTPPort() int { return rt.httpLn.Addr().(*net.TCPAddr).Port }

// HTTPSPort is the bound port of the https listener.
func (rt *Router) HTTPSPort() int { return rt.tlsLn.Addr().(*net.TCPAddr).Port }

// Serve reads the table, serves both listeners and manages certificates
// until Shutdown or a listener fails.
func (rt *Router) Serve() error {
	errc := make(chan error, 2)
	go func() { errc <- rt.httpSrv.Serve(rt.httpLn) }()
	go func() { errc <- rt.tlsSrv.ServeTLS(rt.tlsLn, "", "") }()
	go rt.poll()
	rt.log.Info("router listening", "http", rt.HTTPAddr(), "https", rt.HTTPSAddr(), "acme_ca", rt.cfg.CA)
	err := <-errc
	if errors.Is(err, http.ErrServerClosed) {
		err = <-errc
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops certificate work, closes the listeners and waits for
// requests in flight, up to shutdownTimeout.
func (rt *Router) Shutdown() error {
	rt.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := errors.Join(rt.httpSrv.Shutdown(ctx), rt.tlsSrv.Shutdown(ctx))
	rt.cache.Stop()
	return err
}

// poll reads the table every Poll until Shutdown. A failed read keeps the
// last table, so the router goes on serving while the server is away.
func (rt *Router) poll() {
	t := time.NewTicker(rt.cfg.Poll)
	defer t.Stop()
	failing := false
	for {
		ctx, cancel := context.WithTimeout(rt.ctx, 10*time.Second)
		tab, err := rt.cfg.Routes(ctx)
		cancel()
		switch {
		case err != nil && rt.ctx.Err() == nil:
			if !failing {
				rt.log.Warn("reading the routing table; serving the last one", "err", err)
			}
			failing = true
		case err == nil:
			if failing {
				rt.log.Info("reading the routing table again")
			}
			failing = false
			rt.SetTable(tab)
		}
		select {
		case <-rt.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SetTable replaces the routing table, starts managing certificates for new
// hosts, and stops managing hosts that left. Serve calls it after each read;
// tests call it directly.
func (rt *Router) SetTable(tab Table) {
	rt.proxy.setTable(tab)
	rt.mu.Lock()
	var add []string
	for h := range tab {
		if !rt.managed[h] {
			rt.managed[h] = true
			add = append(add, h)
		}
	}
	var drop []certmagic.SubjectIssuer
	for h := range rt.managed {
		if _, ok := tab[h]; !ok {
			delete(rt.managed, h)
			drop = append(drop, certmagic.SubjectIssuer{Subject: h})
		}
	}
	manage := rt.manage
	rt.mu.Unlock()
	if len(drop) > 0 {
		rt.cache.RemoveManaged(drop)
		rt.log.Info("stopped managing certificates", "hosts", subjects(drop))
	}
	if len(add) > 0 {
		slices.Sort(add)
		// ManageAsync loads a certificate from the shared storage, or
		// obtains one under the storage lock, and renews it from then on.
		// Failures are retried with backoff, and logged.
		if err := manage(rt.ctx, add); err != nil && rt.ctx.Err() == nil {
			rt.log.Warn("managing certificates", "hosts", add, "err", err)
		} else {
			rt.log.Info("managing certificates", "hosts", add)
		}
	}
}

func subjects(s []certmagic.SubjectIssuer) []string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		out = append(out, x.Subject)
	}
	slices.Sort(out)
	return out
}

// getCertificate serves certificates for the hosts in the table only. A name
// the table does not have fails the handshake: there is no default
// certificate, and no on-demand issuance.
func (rt *Router) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := requestHost(&http.Request{Host: hello.ServerName})
	if name == "" || !rt.proxy.known(name) {
		return nil, errors.New("no certificate for this server name")
	}
	return rt.magic.GetCertificate(hello)
}

// serveHTTP is the port 80 handler: the HTTP-01 challenges of hosts in the
// table, and a redirect to https for every other request for one. An unknown
// host gets 404, and certmagic is not asked about it.
func (rt *Router) serveHTTP(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	if !rt.proxy.known(host) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if rt.issuer.HandleHTTPChallenge(w, r) {
		return
	}
	target := "https://" + host
	if p := rt.HTTPSPort(); p != 443 {
		target += ":" + strconv.Itoa(p)
	}
	http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusPermanentRedirect)
}

// WriteMetrics adds the router's metrics to w: its counters, and the expiry
// of the certificate it holds for each host.
func (rt *Router) WriteMetrics(w *metrics.Writer) {
	hosts := rt.proxy.hosts()
	expiry := map[string]time.Time{}
	for _, h := range hosts {
		for _, c := range rt.cache.AllMatchingCertificates(h) {
			if c.Leaf != nil && c.Leaf.NotAfter.After(expiry[h]) {
				expiry[h] = c.Leaf.NotAfter
			}
		}
	}
	rt.cfg.Metrics.write(w, expiry, hosts)
}

// Managed returns the hosts whose certificates the router manages, sorted.
func (rt *Router) Managed() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return slices.Sorted(maps.Keys(rt.managed))
}
