package auth

import "net/http"

// Router is an http.ServeMux that records the patterns registered on it. A
// router keeps the list so a test can enumerate every route and check that
// each one sits behind Authenticator.Middleware. Patterns use the ServeMux
// syntax, such as "GET /api/apps/{name}".
type Router struct {
	mux      *http.ServeMux
	patterns []string
}

// NewRouter returns an empty Router.
func NewRouter() *Router {
	return &Router{mux: http.NewServeMux()}
}

// Handle registers a handler for pattern and records the pattern.
func (r *Router) Handle(pattern string, h http.Handler) {
	r.patterns = append(r.patterns, pattern)
	r.mux.Handle(pattern, h)
}

// HandleFunc registers a handler function for pattern and records the pattern.
func (r *Router) HandleFunc(pattern string, f func(http.ResponseWriter, *http.Request)) {
	r.Handle(pattern, http.HandlerFunc(f))
}

// Patterns returns the registered patterns in registration order.
func (r *Router) Patterns() []string {
	return append([]string(nil), r.patterns...)
}

// ServeHTTP dispatches to the registered handlers.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}
