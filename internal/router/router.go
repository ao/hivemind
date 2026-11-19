// Package router is the HTTPS router that runs on every node that opts in.
// It serves the routing table that the server builds from the replicas'
// reports: for each host, its routes, and for each route, the private
// address and route host port of every Ready replica. A route is the whole
// host or a path prefix of it; a request goes to the route with the longest
// prefix of its path, and round-robin to that route's replicas through
// net/http/httputil.ReverseProxy; a replica that cannot be dialled is
// skipped for a few seconds, and the next one is tried. A request that none
// of its route's replicas takes tries those of a newer table before it
// gets 502, when the table it had is half a second old (see balancer). The
// router speaks HTTP/1.1 to the replicas of a route, or HTTP/2 with prior
// knowledge to those of an h2c route, such as gRPC servers.
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
	DefaultPoll = time.Second
	// shutdownTimeout is how long Shutdown waits for requests in flight
	// before it closes the connections still open.
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
	Storage Storage
	// ChallengeReadInterval is the least time between two reads of one
	// host's challenge token from Storage, which challenge requests cause.
	// Zero means DefaultChallengeReadInterval.
	ChallengeReadInterval time.Duration
	// MaxConns caps the router's open connections, over both listeners; a
	// connection over it is closed as it is accepted. Zero means
	// DefaultMaxConns(); below zero, no cap.
	MaxConns int
	// MaxConnsPerAddr caps the open connections of one client address, or
	// of one IPv6 /64, over both listeners. Zero means
	// DefaultMaxConnsPerAddr; below zero, no cap.
	MaxConnsPerAddr int
	// WriteTimeout ends an https connection when a write to the client
	// makes no progress for this long. Zero means DefaultWriteTimeout.
	WriteTimeout time.Duration
	// UpgradeIdleTimeout closes an upgraded connection, such as a
	// WebSocket, and ends a request on an h2c route, such as a gRPC stream,
	// that carries no data either way for this long. Zero means
	// DefaultUpgradeIdleTimeout.
	UpgradeIdleTimeout time.Duration
	// Email is the ACME account's contact. Registering the account agrees
	// to the CA's subscriber agreement.
	Email string
	// CA is the ACME directory URL. Empty means LetsEncrypt.
	CA string
	// CARoots, if set, are the roots trusted for the CA's own HTTPS. Nil
	// means the system roots.
	CARoots *x509.CertPool
	// Poll is how often the table is read. Zero means DefaultPoll.
	Poll time.Duration
	// Whole, if set, is closed once the table Routes returns has every
	// replica it should: on the server, once its own node has reported
	// after the start, as the store leaves that node's replicas out of the
	// table until then. The router then reads the table at once. Until
	// that read has ended, or WholeWait after Serve began, a request whose
	// route has no endpoint that takes it, or no endpoint at all, waits for
	// it rather than get 502 or 503. Nil means every table is whole.
	Whole   <-chan struct{}
	Logger  *slog.Logger
	Metrics *Metrics
}

// WholeWait is the longest a router waits for Config.Whole after Serve
// began.
const WholeWait = 2 * time.Second

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
	limits  *connLimits
	httpSrv *http.Server
	tlsSrv  *http.Server

	ctx    context.Context
	cancel context.CancelFunc
	// bg counts the router's own background work, which Shutdown waits for.
	bg sync.WaitGroup

	// refreshMu, lastRefresh and refreshing limit the table reads that
	// challenges for unknown hosts, and requests that no replica took,
	// cause (see refresh). lastRefresh is when the last of those reads
	// started, or is to start. refreshing is closed when the read that has
	// not started yet ends; it is nil when none waits to start.
	refreshMu   sync.Mutex
	lastRefresh time.Time
	refreshing  chan struct{}

	// whole is closed once the table is whole (Config.Whole), or
	// wholeWait after Serve began; from New when Config.Whole is nil.
	whole     chan struct{}
	wholeOnce sync.Once
	// wholeWait is WholeWait, and shutdownWait shutdownTimeout, but for
	// tests.
	wholeWait, shutdownWait time.Duration
	// stopOnce makes Shutdown's work happen once; stopErr is its result.
	stopOnce sync.Once
	stopErr  error

	// mu guards the fields below, and orders Shutdown's cancel with the
	// start of background work.
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
	if cfg.ChallengeReadInterval <= 0 {
		cfg.ChallengeReadInterval = DefaultChallengeReadInterval
	}
	if cfg.MaxConns == 0 {
		cfg.MaxConns = DefaultMaxConns()
	}
	if cfg.MaxConnsPerAddr == 0 {
		cfg.MaxConnsPerAddr = DefaultMaxConnsPerAddr
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.UpgradeIdleTimeout <= 0 {
		cfg.UpgradeIdleTimeout = DefaultUpgradeIdleTimeout
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	rt := &Router{cfg: cfg, log: log, managed: map[string]bool{}, whole: make(chan struct{}),
		wholeWait: WholeWait, shutdownWait: shutdownTimeout}
	rt.proxy = newProxy(log, cfg.Metrics)
	rt.proxy.upgradeIdle = cfg.UpgradeIdleTimeout
	rt.proxy.refresh = rt.refresh
	if cfg.Whole == nil {
		rt.endWhole()
	} else {
		rt.proxy.awaitWhole = rt.awaitWhole
	}
	rt.limits = newConnLimits(cfg.MaxConns, cfg.MaxConnsPerAddr, log, cfg.Metrics)

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
	httpLn, err := net.Listen(network, net.JoinHostPort(ip.String(), strconv.Itoa(cfg.HTTPPort)))
	if err != nil {
		return nil, fmt.Errorf("router: http listener: %w", err)
	}
	tlsLn, err := net.Listen(network, net.JoinHostPort(ip.String(), strconv.Itoa(cfg.HTTPSPort)))
	if err != nil {
		httpLn.Close()
		return nil, fmt.Errorf("router: https listener: %w", err)
	}
	// Both listeners count against the same caps. Only the https one has
	// the write limit: the http server's WriteTimeout bounds its replies,
	// which are redirects and challenge answers.
	rt.httpLn = &limitListener{Listener: httpLn, limits: rt.limits}
	rt.tlsLn = &limitListener{Listener: tlsLn, limits: rt.limits, writeTimeout: cfg.WriteTimeout}
	rt.proxy.httpsPort = rt.HTTPSPort()
	rt.ctx, rt.cancel = context.WithCancel(context.Background())

	zl := newZap(log.With("component", "certmagic"))
	rt.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return rt.magic, nil },
		Logger:           zl,
	})
	rt.magic = certmagic.New(rt.cache, certmagic.Config{
		Storage: challengeStorage{Storage: cfg.Storage,
			reads: newChallengeReads(rt.ctx, cfg.Storage.Load, cfg.ChallengeReadInterval, log)},
		Logger: zl,
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
	wholeBy := time.Now().Add(rt.wholeWait)
	// The table and the certificates already in storage first: a router
	// that took TLS connections before it had them failed the first
	// handshakes after every start. The listener is bound, so connections
	// that arrive meanwhile wait in its backlog rather than fail.
	rt.warm()
	rt.readWhenWhole(wholeBy)
	go func() { errc <- rt.tlsSrv.ServeTLS(rt.tlsLn, "", "") }()
	go rt.poll()
	rt.log.Info("router listening", "http", rt.HTTPAddr(), "https", rt.HTTPSAddr(), "acme_ca", rt.cfg.CA,
		"max_conns", rt.cfg.MaxConns, "max_conns_per_ip", loggedPerAddr(rt.cfg.MaxConnsPerAddr))
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
// requests in flight, up to shutdownTimeout. Then it closes the connections
// still open, and logs how many at info: those of requests that have not
// ended, such as streams, and the upgraded ones, such as WebSockets, which
// http.Server leaves to the handler that took them over and does not wait
// for. Before, the router returned the wait's "context deadline exceeded",
// which the caller logged as a warning that said nothing of the
// connections (seventh soak), and left them to the process's exit. It may
// be called on a router that was never served, and more than once: later
// calls return the first one's result.
func (rt *Router) Shutdown() error {
	rt.stopOnce.Do(func() { rt.stopErr = rt.shutdown() })
	return rt.stopErr
}

func (rt *Router) shutdown() error {
	// Under mu, so that no background work starts after the cancel (see
	// renewAtStart) and bg's count is complete before the Wait below.
	rt.mu.Lock()
	rt.cancel()
	rt.mu.Unlock()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), rt.shutdownWait)
	defer cancel()
	err := errors.Join(waited(rt.httpSrv.Shutdown(ctx)), waited(rt.tlsSrv.Shutdown(ctx)))
	if n := rt.limits.closeAll(); n > 0 {
		rt.log.Info("router stopped; closed the connections still open after its wait for requests in flight",
			"connections", n, "waited", time.Since(start).Round(time.Millisecond), "wait", rt.shutdownWait)
	}
	if ctx.Err() != nil {
		_ = rt.httpSrv.Close()
		_ = rt.tlsSrv.Close()
	}
	// A listener that was never served is not closed by Shutdown.
	_ = rt.httpLn.Close()
	_ = rt.tlsLn.Close()
	rt.bg.Wait()
	rt.cache.Stop()
	return err
}

// waited is a server's Shutdown error, without the end of the wait, which
// shutdown deals with.
func waited(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
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
	var due []string
	for h := range tab {
		cert, err := rt.magic.CacheManagedCertificate(ctx, h)
		if err != nil {
			continue
		}
		loaded++
		if cert.NeedsRenewal(rt.magic) {
			due = append(due, h)
		}
	}
	rt.SetTable(tab)
	rt.log.Info("router ready", "hosts", len(tab), "certificates_loaded", loaded)
	if len(due) > 0 {
		rt.renewAtStart(due)
	}
}

// renewAtStart runs certmagic's renewal check now, in the background, for a
// router that started with certificates in their renewal window. A
// certificate warm cached is already managed, so the ManageAsync that
// SetTable calls returns early for it (certmagic's manageOne), without the
// renewal check it makes for a certificate it loads itself; certmagic's own
// check runs only every 10 minutes, so without this a router restarted more
// often than that would never renew one. The check is certmagic's: a
// certificate that another router has renewed in the shared storage is
// loaded from there, and the rest are renewed under the storage lock, with
// certmagic's retry schedule.
func (rt *Router) renewAtStart(hosts []string) {
	slices.Sort(hosts)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.ctx.Err() != nil {
		return
	}
	rt.log.Info("certificates in their renewal window at start; renewing them now", "hosts", hosts)
	rt.bg.Add(1)
	go func() {
		defer rt.bg.Done()
		if err := rt.cache.RenewManagedCertificates(rt.ctx); err != nil && rt.ctx.Err() == nil {
			rt.log.Warn("renewing certificates at start", "hosts", hosts, "err", err)
		}
	}()
}

// readWhenWhole reads the table, in the background, once Config.Whole is
// closed, and then lets the requests that wait for it go on (awaitWhole). At
// by, or at Shutdown, it lets them go on without a read. It does nothing
// without Config.Whole. On the server, Whole closes after its own node's
// first pass, and the read is the first to have that node's replicas: the
// router's first table, read as it starts, often comes before that report.
// If Whole is closed already, warm's read may have had them, and this one
// costs a read.
func (rt *Router) readWhenWhole(by time.Time) {
	if rt.cfg.Whole == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.ctx.Err() != nil {
		rt.endWhole()
		return
	}
	rt.bg.Add(1)
	go func() {
		defer rt.bg.Done()
		defer rt.endWhole()
		t := time.NewTimer(time.Until(by))
		defer t.Stop()
		select {
		case <-rt.cfg.Whole:
		case <-t.C:
			rt.log.Info("the routing table does not have this node's own replicas yet; requests no longer wait for them",
				"waited", rt.wholeWait)
			return
		case <-rt.ctx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(rt.ctx, refreshTimeout)
		defer cancel()
		if tab, err := rt.cfg.Routes(ctx); err == nil {
			rt.SetTable(tab)
		}
	}()
}

// endWhole lets the requests that wait for the whole table go on.
func (rt *Router) endWhole() { rt.wholeOnce.Do(func() { close(rt.whole) }) }

// awaitWhole waits, while the table is not whole (readWhenWhole), until it
// is, ctx ends or the router stops, and reports whether it waited.
func (rt *Router) awaitWhole(ctx context.Context) bool {
	select {
	case <-rt.whole:
		return false
	default:
	}
	select {
	case <-rt.whole:
	case <-ctx.Done():
	case <-rt.ctx.Done():
	}
	return true
}

// acmeChallengePath is where an HTTP-01 challenge is fetched (RFC 8555, 8.3).
const acmeChallengePath = "/.well-known/acme-challenge/"

// refreshEvery limits the table reads that challenges for unknown hosts,
// and requests that no replica took (newerTable), cause, so that requests
// for made-up hosts, or for a route whose replicas are all down, cannot make
// the router read the table more than once a second. refreshTimeout bounds
// one such read.
const (
	refreshEvery   = time.Second
	refreshTimeout = 5 * time.Second
)

// refresh reads the table, at most one read starting every refreshEvery,
// and returns once a read that started after the call has ended. A call
// that comes less than refreshEvery after the last read started waits for
// the next one, at refreshEvery after it, and every call until that read
// starts waits for the same one. Before, such a call read nothing, and the
// router answered 404 for a host it had not read: in the second scale run,
// 8 of 12 hosts deployed tens of milliseconds apart failed their first
// HTTP-01 validation, and got their certificates a minute late. The read
// runs under the router's context, not the caller's, so a client that goes
// away does not cancel it: if it did, the router would go without the new
// table until the next read, and a CA's validation in between would fail.
// Each caller stops waiting when its own ctx ends.
func (rt *Router) refresh(ctx context.Context) {
	rt.refreshMu.Lock()
	done := rt.refreshing
	if done == nil {
		at := time.Now()
		if next := rt.lastRefresh.Add(refreshEvery); next.After(at) {
			at = next
		}
		rt.lastRefresh = at
		done = make(chan struct{})
		rt.refreshing = done
		go rt.readTable(at, done)
	}
	rt.refreshMu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// readTable is refresh's read, which starts at at. Callers that come once
// it has started wait for the next one. It closes done when it ends.
func (rt *Router) readTable(at time.Time, done chan struct{}) {
	defer close(done)
	if d := time.Until(at); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-rt.ctx.Done():
			t.Stop()
			rt.refreshMu.Lock()
			rt.refreshing = nil
			rt.refreshMu.Unlock()
			return
		}
	}
	rt.refreshMu.Lock()
	rt.refreshing = nil
	rt.refreshMu.Unlock()
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
	// certmagic reads the challenge token under a key made from Host, so
	// the table's spelling keeps one key, and one read limit, per host.
	r.Host = host
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

// loggedPerAddr is the per-address cap as the router logs it: 0 for no cap,
// as --route-max-conns-per-ip spells it, where Config has a negative number.
func loggedPerAddr(n int) int { return max(n, 0) }
