// Package server is the hivemind HTTP API. Every route sits behind
// auth.Authenticator.Middleware, which admits only GET /healthz without the
// admin token. The package builds the handler and the http.Server settings;
// the caller opens the listener.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/auth"
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
	// NodeID is the local node. Logs are read from its runtime.
	NodeID string
	Auth   *auth.Authenticator
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
}

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
	s := &Server{cfg: cfg, log: log, metrics: cfg.Metrics, router: auth.NewRouter(), metricsRouter: auth.NewRouter()}
	s.streams, s.cancelStreams = context.WithCancel(context.Background())
	s.routes()
	s.metricsRouter.HandleFunc("GET /metrics", s.metricsHandler)
	s.handler = s.wrap(s.router)
	s.metricsOnly = s.wrap(s.metricsRouter)
	return s, nil
}

// wrap puts a router behind the request log and counter, the JSON error
// rewriter and the admin-token check.
func (s *Server) wrap(r *auth.Router) http.Handler {
	return s.logRequests(r, jsonErrors(s.cfg.Auth.Middleware(r)))
}

func (s *Server) routes() {
	r := s.router
	r.HandleFunc("GET /healthz", s.healthz)
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

// CloseStreams ends every followed log stream. Call it before
// http.Server.Shutdown, which waits for active requests and would otherwise
// wait for a stream that never ends.
func (s *Server) CloseStreams() { s.cancelStreams() }

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
