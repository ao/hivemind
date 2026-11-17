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
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/ao/hivemind/internal/nodeapi"
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

	// refreshMu, lastRefresh and refreshing limit the table reads that
	// challenges for unknown hosts cause (see refresh). refreshing is closed
	// when the read in flight ends; it is nil when none runs.
	refreshMu   sync.Mutex
	lastRefresh time.Time
	refreshing  chan struct{}

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
	//
	// The network is the address's family: net.Listen("tcp", "0.0.0.0:443")
	// would open a dual-stack socket on [::], which takes IPv6 as well.
	network := "tcp4"
	if !ip.Is4() {
		network = "tcp6"
	}
	rt.httpLn, err = net.Listen(network, net.JoinHostPort(ip.String(), strconv.Itoa(cfg.HTTPPort)))
	if err != nil {
		return nil, fmt.Errorf("router: http listener: %w", err)
	}
	rt.tlsLn, err = net.Listen(network, net.JoinHostPort(ip.String(), strconv.Itoa(cfg.HTTPSPort)))
	if err != nil {
		rt.httpLn.Close()
		return nil, fmt.Errorf("router: https listener: %w", err)
	}
	rt.proxy.httpsPort = rt.HTTPSPort()

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
	// The table and the certificates already in storage first: a router
	// that took TLS connections before it had them failed the first
	// handshakes after every start. The listener is bound, so connections
	// that arrive meanwhile wait in its backlog rather than fail.
	rt.warm()
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
// requests in flight, up to shutdownTimeout. It may be called on a router
// that was never served.
func (rt *Router) Shutdown() error {
	rt.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := errors.Join(rt.httpSrv.Shutdown(ctx), rt.tlsSrv.Shutdown(ctx))
	// A listener that was never served is not closed by Shutdown.
	_ = rt.httpLn.Close()
	_ = rt.tlsLn.Close()
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

// warmTimeout bounds what a starting router waits for before it takes TLS
// connections: one read of the table, and loading its hosts' certificates
// from storage.
const warmTimeout = 5 * time.Second

// warm reads the table once, loads from storage the certificate of each of
// its hosts that has one, and then sets the table, which starts managing the
// hosts as usual. A host with no certificate yet is left to that. A table
// that cannot be read, as when the server is away, leaves the router to its
// poll, as before.
func (rt *Router) warm() {
	ctx, cancel := context.WithTimeout(rt.ctx, warmTimeout)
	defer cancel()
	tab, err := rt.cfg.Routes(ctx)
	if err != nil {
		rt.log.Info("starting without a routing table; reading it again shortly", "err", err)
		return
	}
	loaded := 0
	for h := range tab {
		if _, err := rt.magic.CacheManagedCertificate(ctx, h); err == nil {
			loaded++
		}
	}
	rt.SetTable(tab)
	rt.log.Info("router ready", "hosts", len(tab), "certificates_loaded", loaded)
}

// acmeChallengePath is where an HTTP-01 challenge is fetched (RFC 8555, 8.3).
const acmeChallengePath = "/.well-known/acme-challenge/"

// refreshEvery limits the table reads that challenges for unknown hosts
// cause, so that requests for made-up hosts cannot make the router read the
// table more than once a second. refreshTimeout bounds one such read.
const (
	refreshEvery   = time.Second
	refreshTimeout = 5 * time.Second
)

// refresh reads the table now, unless a read started less than refreshEvery
// ago. The read runs under the router's context, not the caller's, so a
// client that goes away does not cancel it: if it did, the router would go
// without the new table until the next read refreshEvery later, and a CA's
// validation in between would fail. Callers that arrive while a read runs
// wait for it and then use its table; each stops waiting when its own ctx
// ends.
func (rt *Router) refresh(ctx context.Context) {
	rt.refreshMu.Lock()
	done := rt.refreshing
	if done == nil {
		if time.Since(rt.lastRefresh) < refreshEvery {
			rt.refreshMu.Unlock()
			return
		}
		rt.lastRefresh = time.Now()
		done = make(chan struct{})
		rt.refreshing = done
		go rt.readTable(done)
	}
	rt.refreshMu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// readTable is refresh's read. It closes done when it ends.
func (rt *Router) readTable(done chan struct{}) {
	defer func() {
		rt.refreshMu.Lock()
		rt.refreshing = nil
		rt.refreshMu.Unlock()
		close(done)
	}()
	ctx, cancel := context.WithTimeout(rt.ctx, refreshTimeout)
	defer cancel()
	if tab, err := rt.cfg.Routes(ctx); err == nil {
		rt.SetTable(tab)
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
	if host != "" && !rt.proxy.known(host) && strings.HasPrefix(r.URL.Path, acmeChallengePath) {
		// The router that ordered the certificate may have read the table
		// with the new host before this one did, and the CA can validate
		// through any router. Read it now rather than fail the challenge.
		rt.refresh(r.Context())
	}
	if !rt.proxy.known(host) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if rt.issuer.HandleHTTPChallenge(w, r) {
		return
	}
	http.Redirect(w, r, "https://"+rt.proxy.authority(host)+r.URL.RequestURI(), http.StatusPermanentRedirect)
}

// Managed returns the hosts whose certificates the router manages, sorted.
func (rt *Router) Managed() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return slices.Sorted(maps.Keys(rt.managed))
}

// Report returns the router's counters since it started, and the expiry of
// the certificate it serves for each host, for a worker to send to the
// server, whose /metrics shows every router's.
func (rt *Router) Report() nodeapi.RouterReport {
	return rt.cfg.Metrics.report(rt.certExpiry())
}

// certExpiry is the expiry of the latest certificate the router holds for
// each host in its table.
func (rt *Router) certExpiry() map[string]time.Time {
	expiry := map[string]time.Time{}
	for _, h := range rt.proxy.hosts() {
		for _, c := range rt.cache.AllMatchingCertificates(h) {
			if c.Leaf != nil && c.Leaf.NotAfter.After(expiry[h]) {
				expiry[h] = c.Leaf.NotAfter
			}
		}
	}
	return expiry
}
