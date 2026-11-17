// Package cluster is the server's side of node-to-node traffic. It opens two
// listeners on one address of the server, and only there (Config.Bind, or
// the advertise address):
//
//   - the node listener, on the cluster port, TLS 1.3 with a required client
//     certificate. Every node route is served here, so a request reaches a
//     handler only with a certificate signed by the cluster CA that names a
//     node.
//   - the join listener, on the cluster port plus one. It presents the same
//     server certificate but asks for no client certificate, because a
//     joining node has none yet. It serves POST /join and nothing else.
//
// The two listeners are separate so that a mistake in routing can't expose a
// node route without a certificate: the join listener's mux has no node
// routes, and the node listener's TLS config refuses a handshake without a
// verified certificate.
package cluster

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/ao/hivemind/internal/logrelay"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/ratelimit"
	"github.com/ao/hivemind/internal/store"
)

// Timeouts for both listeners. Messages are small.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 16 << 10
	shutdownTimeout   = 10 * time.Second
)

// Defaults for the join limiter in Listen: a source address may fail
// DefaultFailLimit joins in each DefaultFailWindow. A failed join is one that
// was refused.
const (
	DefaultFailLimit  = 5
	DefaultFailWindow = 10 * time.Minute
)

// Config configures Listen.
type Config struct {
	Store *store.Store
	// CA signs node certificates and issues the server's own certificate.
	CA *pki.CA
	// Tokens redeems join tokens. It must be backed by the same store.
	Tokens *pki.Tokens
	// Advertise is the address that nodes dial, an IP address or a DNS name,
	// and that the server's certificate names. It must not be a wildcard;
	// config.Validate refuses one.
	Advertise string
	// Bind is the IP address the listeners bind, which the server's
	// certificate names as well. Empty means Advertise, as net.Listen
	// resolves it. serve sets it to the address it chose for the server's
	// node (--cluster-addr, or what --advertise names or resolves to), so a
	// server can advertise a name whose record does not lead here yet.
	Bind string
	// Port is the node listener's port. The join listener uses Port+1. Zero
	// picks a free port for each listener (tests only).
	Port int
	// LocalNodeID is the server's own node ID, which goes into its certificate.
	LocalNodeID string
	// Limiter counts failed joins per source address. Nil means
	// DefaultFailLimit failures per DefaultFailWindow.
	Limiter *ratelimit.FailLimiter
	// Logger receives one line per join and per rejected node request. Nil
	// means slog.Default().
	Logger *slog.Logger
	// Logs relays log requests to workers and their streams back. The API
	// server must share it. Nil means a private relay, which no API uses.
	Logs *logrelay.Relay
	// RouterReports keeps the router counters workers post, for the API's
	// /metrics, which must share it. Nil drops them.
	RouterReports *nodeapi.RouterReports
	// NodeCertLifetime is the lifetime of the certificates the server
	// issues: at join, at renewal, and its own. Zero means
	// pki.NodeCertLifetime. Only tests set it.
	NodeCertLifetime time.Duration
	// Now is the clock for issuing certificates and checking client
	// certificates on each request. Nil means time.Now. Tests set it; the
	// TLS handshake itself always uses the real time.
	Now func() time.Time
	// Version is the server's hivemind version, sent in
	// nodeapi.VersionHeader on every node API reply. Empty sends none.
	Version string
}

func (cfg Config) now() func() time.Time {
	if cfg.Now != nil {
		return cfg.Now
	}
	return time.Now
}

// Cluster is a pair of bound listeners, ready to serve.
type Cluster struct {
	log   *slog.Logger
	logs  *logrelay.Relay
	mtls  *http.Server
	join  *http.Server
	mln   net.Listener
	jln   net.Listener
	certs *serverCertSource
	stop  context.CancelFunc
	ctx   context.Context
}

// NewNodeID returns a new node ID: "node-" and 12 random hex digits. It is a
// valid pki node ID.
func NewNodeID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "node-" + hex.EncodeToString(b[:]), nil
}

// Listen validates cfg, issues the server's certificate and binds both
// listeners. Nothing is served until Serve is called. On error nothing stays
// bound.
func Listen(cfg Config) (*Cluster, error) {
	if cfg.Store == nil || cfg.CA == nil || cfg.Tokens == nil || cfg.LocalNodeID == "" {
		return nil, errors.New("cluster: Store, CA, Tokens and LocalNodeID are required")
	}
	if err := pki.ValidateNodeID(cfg.LocalNodeID); err != nil {
		return nil, err
	}
	if cfg.Advertise == "" {
		return nil, errors.New("cluster: no advertise address, so there is no cluster listener")
	}
	if cfg.Port < 0 || cfg.Port > 65534 {
		return nil, fmt.Errorf("cluster: port %d leaves no room for the join port", cfg.Port)
	}
	bind := cfg.Advertise
	if cfg.Bind != "" {
		if _, err := netip.ParseAddr(cfg.Bind); err != nil {
			return nil, fmt.Errorf("cluster: bind address %q is not an IP address", cfg.Bind)
		}
		bind = cfg.Bind
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	limiter := cfg.Limiter
	if limiter == nil {
		limiter = ratelimit.New(DefaultFailLimit, DefaultFailWindow)
	}

	certs := &serverCertSource{cfg: cfg, log: log, now: cfg.now()}
	tlsCert, err := certs.issue()
	if err != nil {
		return nil, fmt.Errorf("cluster: server certificate: %w", err)
	}
	certs.holder = pki.NewCertHolder(tlsCert)

	mln, err := listenAdvertise(bind, cfg.Port)
	if err != nil {
		return nil, fmt.Errorf("cluster listener on %s: %w", bind, err)
	}
	joinPort := 0
	if cfg.Port != 0 {
		joinPort = cfg.Port + 1
	}
	jln, err := listenAdvertise(bind, joinPort)
	if err != nil {
		mln.Close()
		return nil, fmt.Errorf("join listener on %s: %w", bind, err)
	}

	if cfg.Logs == nil {
		cfg.Logs = logrelay.New(logrelay.Options{})
	}
	c := &Cluster{log: log, logs: cfg.Logs, mln: mln, jln: jln, certs: certs}
	c.ctx, c.stop = context.WithCancel(context.Background())
	c.mtls = newHTTPServer(nodeRoutes(cfg, log))
	c.join = newHTTPServer(joinRoutes(cfg, limiter, log))
	// Both listeners read the server certificate from the holder at each
	// handshake, so a renewal takes effect without a restart.
	c.mtls.TLSConfig = pki.ServerTLSConfigFrom(cfg.CA, certs.holder)
	c.join.TLSConfig = pki.JoinServerTLSConfigFrom(certs.holder)
	certs.check() // logs the CA warning at start, if it applies
	return c, nil
}

// ServerCert returns the server certificate new handshakes use.
func (c *Cluster) ServerCert() *tls.Certificate { return c.certs.holder.Load() }

// listenAdvertise binds host:port, the bind or advertise address. Port 0
// picks a free port. A listener that ends up on a wildcard address is
// refused.
func listenAdvertise(host string, port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok || tcp.IP.IsUnspecified() {
		ln.Close()
		return nil, fmt.Errorf("bound %s, which is a wildcard address", ln.Addr())
	}
	return ln, nil
}

func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

// NodeAddr is the address of the node listener, as bound.
func (c *Cluster) NodeAddr() string { return c.mln.Addr().String() }

// JoinAddr is the address of the join listener, as bound.
func (c *Cluster) JoinAddr() string { return c.jln.Addr().String() }

// Serve serves both listeners until Shutdown is called or one of them fails.
// It returns the first error that is not the result of Shutdown.
func (c *Cluster) Serve() error {
	go func() {
		c.certs.run(c.ctx)
	}()
	errc := make(chan error, 2)
	go func() { errc <- c.mtls.ServeTLS(c.mln, "", "") }()
	go func() { errc <- c.join.ServeTLS(c.jln, "", "") }()
	err := <-errc
	if errors.Is(err, http.ErrServerClosed) {
		err = <-errc
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown closes both listeners and waits for requests in flight, up to
// shutdownTimeout. It closes the log relay first, so log polls and streams end
// at once instead of holding the shutdown.
func (c *Cluster) Shutdown() error {
	c.stop()
	c.logs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return errors.Join(c.mtls.Shutdown(ctx), c.join.Shutdown(ctx))
}
