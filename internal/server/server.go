// Package server is the hivemind HTTP API. Every route sits behind
// auth.Authenticator.Middleware, which admits only GET /healthz without the
// admin token or a session cookie. The package builds the handler and the
// http.Server settings; the caller opens the listener.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/carotate"
	"github.com/ao/hivemind/internal/logrelay"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/ratelimit"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/store"
)

// Limits and timeouts.
const (
	// MaxBodyBytes caps every request body. An app spec is a few kilobytes.
	MaxBodyBytes = 1 << 20

	ReadHeaderTimeout = 10 * time.Second
	// ReadTimeout covers the headers and the body. Bodies are small.
	ReadTimeout = 30 * time.Second
	// WriteTimeout bounds every response except a followed log stream, whose
	// handler clears the deadline.
	WriteTimeout   = 60 * time.Second
	IdleTimeout    = 120 * time.Second
	MaxHeaderBytes = 64 << 10

	// maxTail caps the tail parameter of the logs route.
	maxTail = 100000
)

// Config configures a Server.
type Config struct {
	Store   *store.Store
	Runtime runtime.Runtime
	// Started is when serve started. A container report older than it was
	// made to an earlier server process, perhaps on another machine before a
	// restore, so the status shows its replica as "unknown" until the node
	// reports again. The zero time turns this off. Whether a node that has
	// not reported since is "waiting" rather than "lost" is the store's
	// start grace (store.SetStarted).
	Started time.Time
	// NodeID is the local node. Logs of its replicas are read from Runtime.
	NodeID string
	// Logs relays log requests for replicas on other nodes to their workers.
	// The cluster listener must share it. Nil means the server has no cluster
	// listener, and the logs of those replicas cannot be read.
	Logs *logrelay.Relay
	Auth *auth.Authenticator
	// Trigger, if set, is called after a change to desired state, so the
	// local agent reconciles at once instead of at its next tick.
	Trigger func()
	// Logger receives one line per request. Nil means slog.Default().
	Logger *slog.Logger
	// Metrics receives the request counts and holds the reconcile counters
	// that GET /metrics reports. Nil means a new, private set.
	Metrics *Metrics
	// IssueJoinToken makes a join token that expires after ttl. Nil means
	// this server has no cluster CA, and POST /v1/join-tokens answers 503.
	IssueJoinToken func(ttl time.Duration) (token string, expires time.Time, err error)
	// LoginLimiter counts failed logins (POST /v1/session) per source
	// address. Nil means LoginFailLimit failures per LoginFailWindow.
	LoginLimiter *ratelimit.FailLimiter
	// Backup writes a backup archive of the data directory to w. Nil means
	// POST /v1/backup answers 503.
	Backup func(ctx context.Context, w io.Writer) error
	// RouterReport, if set, gives this node's router's counters, and
	// RouterReports keeps the ones workers post (the cluster listener must
	// share it). GET /metrics shows them all, labelled by node.
	RouterReport  func() nodeapi.RouterReport
	RouterReports *nodeapi.RouterReports
	// RouteReaders keeps how the workers' routers read the routing table
	// (the cluster listener must share it). GET /v1/routes and the status
	// of the apps routed on a host with path routes warn about the routers
	// that cannot read paths. Nil means none are known.
	RouteReaders *nodeapi.RouteReaders
	// Version is the server's hivemind version. Every reply to an
	// authenticated request carries it in nodeapi.VersionHeader; GET
	// /healthz, which needs no token, does not. Empty sends none.
	Version string
	// Advertise is serve's --advertise, the address the workers dial for
	// the server, as given: GET /v1/nodes shows it on the server's node.
	// Empty without --advertise.
	Advertise string
	// CA holds the cluster's CAs and runs a rotation of them, for GET
	// /v1/ca and POST /v1/ca/rotation/{step}. Nil means this server has no
	// cluster CA, and those routes answer 503.
	CA *carotate.Manager
	// Servers lists the servers of the cluster for GET /v1/servers, and
	// RemoveServer removes one for DELETE /v1/servers/{id}, returning what
	// to do with its node (docs/ha.md#membership). Nil answers 503.
	Servers      func(ctx context.Context) (ServersReply, error)
	RemoveServer func(ctx context.Context, id string, force bool) (string, error)
	// IssueServerJoinToken makes a server join token, with a warning to
	// pass on, such as that the cluster would have two servers. Nil means
	// this server cannot add servers, and the request answers 503.
	IssueServerJoinToken func(ttl time.Duration) (token string, expires time.Time, warning string, err error)
}

// Defaults for Config.LoginLimiter: an address may fail LoginFailLimit
// logins in each LoginFailWindow, and is then refused with 429 until the
// window ends.
const (
	LoginFailLimit  = 5
	LoginFailWindow = 10 * time.Minute
)

// sessionPath is the login and logout route.
const sessionPath = "/v1/session"

// Server serves the API.
type Server struct {
	cfg           Config
	log           *slog.Logger
	metrics       *Metrics
	router        *auth.Router
	metricsRouter *auth.Router
	handler       http.Handler
	metricsOnly   http.Handler
	streams       context.Context
	cancelStreams context.CancelFunc
}

// New builds the API handler.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Runtime == nil || cfg.Auth == nil || cfg.NodeID == "" {
		return nil, errors.New("server: Store, Runtime, Auth and NodeID are required")
	}
	if cfg.Trigger == nil {
		cfg.Trigger = func() {}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NewMetrics()
	}
	if cfg.LoginLimiter == nil {
		cfg.LoginLimiter = ratelimit.New(LoginFailLimit, LoginFailWindow)
	}
	s := &Server{cfg: cfg, log: log, metrics: cfg.Metrics, router: auth.NewRouter(), metricsRouter: auth.NewRouter()}
	s.streams, s.cancelStreams = context.WithCancel(context.Background())
	s.routes()
	s.metricsRouter.HandleFunc("GET /metrics", s.metricsHandler)
	s.handler = s.wrap(s.router)
	s.metricsOnly = s.wrap(s.metricsRouter)
	return s, nil
}

// wrap puts a router behind the request log and counter, the JSON error
// rewriter, the login limit and the credential check. A request that passes
// the check gets the server's version on its reply.
func (s *Server) wrap(r *auth.Router) http.Handler {
	return s.logRequests(r, jsonErrors(s.limitLogins(s.cfg.Auth.Middleware(s.withVersion(r)))))
}

// withVersion sets the server's version on the reply. It runs behind the
// credential check, which lets only GET /healthz through without one, so
// that route is left out: the version is not for anyone who can reach the
// port.
func (s *Server) withVersion(next http.Handler) http.Handler {
	v := nodeapi.CleanVersion(s.cfg.Version)
	if v == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.Header().Set(nodeapi.VersionHeader, v)
		}
		next.ServeHTTP(w, r)
	})
}

// limitLogins counts failed logins per source address, and refuses a login
// from an address over the limit with 429 before its token is checked. A
// failed login is a POST /v1/session answered 401, whether the credential
// check or the login handler refused it. Other routes are not limited: the
// admin token is 256 random bits, and API clients send it on every request.
func (s *Server) limitLogins(next http.Handler) http.Handler {
	lim := s.cfg.LoginLimiter
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != sessionPath {
			next.ServeHTTP(w, r)
			return
		}
		src := ratelimit.SourceAddr(r)
		if !lim.Allow(src) {
			w.Header().Set("Retry-After", strconv.Itoa(int(lim.RetryAfter(src).Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "too many failed logins from this address; try again later")
			s.log.Warn("login refused by rate limit", "remote", src)
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.code == http.StatusUnauthorized {
			lim.Fail(src)
			s.log.Warn("login failed", "remote", src)
		}
	})
}

func (s *Server) routes() {
	r := s.router
	r.HandleFunc("GET /healthz", s.healthz)
	// Login exchanges the bearer token for a session cookie and returns the
	// CSRF token. Logout needs the CSRF token like any unsafe cookie request.
	r.Handle("POST "+sessionPath, s.cfg.Auth.LoginHandler())
	r.Handle("DELETE "+sessionPath, s.cfg.Auth.LogoutHandler())
	r.HandleFunc("GET /v1/apps", s.listApps)
	r.HandleFunc("POST /v1/apps", s.postApp)
	r.HandleFunc("GET /v1/apps/{name}", s.getApp)
	r.HandleFunc("PUT /v1/apps/{name}", s.putApp)
	r.HandleFunc("DELETE /v1/apps/{name}", s.deleteApp)
	r.HandleFunc("POST /v1/apps/{name}/scale", s.scaleApp)
	r.HandleFunc("GET /v1/apps/{name}/status", s.appStatus)
	r.HandleFunc("GET /v1/apps/{name}/logs", s.appLogs)
	r.HandleFunc("GET /v1/status", s.status)
	r.HandleFunc("POST /v1/join-tokens", s.postJoinToken)
	r.HandleFunc("GET /v1/nodes", s.listNodes)
	r.HandleFunc("DELETE /v1/nodes/{id}", s.removeNode)
	r.HandleFunc("POST /v1/nodes/{id}/eligibility", s.setEligibility)
	r.HandleFunc("POST /v1/nodes/{id}/drain", s.postDrain)
	r.HandleFunc("GET /v1/nodes/{id}/drain", s.getDrain)
	r.HandleFunc("GET /v1/routes", s.listRoutes)
	r.HandleFunc("GET /v1/registries", s.listRegistries)
	r.HandleFunc("PUT /v1/registries/{host}", s.putRegistry)
	r.HandleFunc("DELETE /v1/registries/{host}", s.deleteRegistry)
	r.HandleFunc("POST /v1/backup", s.postBackup)
	r.HandleFunc("GET /v1/ca", s.getCA)
	r.HandleFunc("POST /v1/ca/rotation/{step}", s.postCARotation)
	r.HandleFunc("GET /v1/servers", s.listServers)
	r.HandleFunc("DELETE /v1/servers/{id}", s.removeServer)
	r.HandleFunc("GET /metrics", s.metricsHandler)
}

// Handler returns the API handler, with authentication and request logging.
func (s *Server) Handler() http.Handler { return s.handler }

// MetricsHandler returns a handler that serves only GET /metrics, for the
// separate --metrics-addr listener. It needs the admin token like every API
// route, and counts its requests in the same Metrics.
func (s *Server) MetricsHandler() http.Handler { return s.metricsOnly }

// Patterns returns every registered route pattern.
func (s *Server) Patterns() []string { return s.router.Patterns() }

// MetricsPatterns returns the route patterns of MetricsHandler.
func (s *Server) MetricsPatterns() []string { return s.metricsRouter.Patterns() }

// CloseStreams ends every followed log stream, and every log stream relayed
// from a worker. Call it before http.Server.Shutdown, which waits for active
// requests and would otherwise wait for a stream that never ends.
func (s *Server) CloseStreams() {
	s.cancelStreams()
	if s.cfg.Logs != nil {
		s.cfg.Logs.Close()
	}
}

// NewHTTPServer returns an http.Server for h with the package's timeouts.
func NewHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// statusWriter records the status code for the request log.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs the method, path, status and duration of each request,
// and counts it by method, route pattern and status. It never logs headers,
// query strings or bodies, so no token, cookie, CSRF token or env value
// reaches the log. The route is looked up before the request is served,
// because the auth check may answer before the router runs.
func (s *Server) logRequests(router *auth.Router, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		route := routeLabel(router, r)
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		code := sw.code
		if code == 0 {
			code = http.StatusOK
		}
		s.metrics.observeRequest(r.Method, route, code)
		s.log.Info("http request", "method", r.Method, "path", r.URL.Path, "route", route, "status", code,
			"duration", time.Since(start).Round(time.Microsecond))
	})
}

// jsonErrors rewrites the plain-text 404 and 405 replies of http.ServeMux as
// JSON. A handler in this package always sets a JSON content type before it
// writes an error, so its replies pass through unchanged.
func jsonErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&errorRewriter{ResponseWriter: w}, r)
	})
}

type errorRewriter struct {
	http.ResponseWriter
	swallow bool
}

func (w *errorRewriter) WriteHeader(code int) {
	if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) &&
		!strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		w.swallow = true
		msg := "not found"
		if code == http.StatusMethodNotAllowed {
			msg = "method not allowed"
		}
		h := w.Header()
		h.Del("X-Content-Type-Options")
		writeError(w.ResponseWriter, code, msg)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *errorRewriter) Write(b []byte) (int, error) {
	if w.swallow {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func (w *errorRewriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
