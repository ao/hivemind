package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
)

const (
	// SessionCookieName is the cookie that carries a UI session ID.
	SessionCookieName = "hivemind_session"

	// CSRFHeader carries the CSRF token from script. A plain HTML form sends
	// the same value in a field named csrf_token instead.
	CSRFHeader = "X-CSRF-Token"
	csrfField  = "csrf_token"

	// healthPath is the only route that needs no credentials.
	healthPath = "/healthz"
)

const (
	bodyUnauthorized = `{"error":"unauthorized"}`
	bodyCSRF         = `{"error":"csrf_token_invalid"}`
)

// Authenticator admits a request that carries either the admin token as a
// bearer header or a live session cookie. Bearer requests are API clients and
// are not subject to CSRF. Cookie requests must carry the session's CSRF token
// on POST, PUT, PATCH and DELETE.
type Authenticator struct {
	adminHash TokenHash
	sessions  *Sessions
}

// NewAuthenticator returns an Authenticator that checks bearer tokens against
// adminHash and sessions against sessions.
func NewAuthenticator(adminHash TokenHash, sessions *Sessions) (*Authenticator, error) {
	if adminHash == (TokenHash{}) {
		return nil, errors.New("auth: no admin token hash; refusing to run without a credential")
	}
	if sessions == nil {
		return nil, errors.New("auth: nil session store")
	}
	return &Authenticator{adminHash: adminHash, sessions: sessions}, nil
}

// Middleware wraps next so that every request must be authenticated. The one
// exception is GET /healthz, matched exactly: HEAD, POST and a trailing slash
// are not exempt. A request that fails gets 401 with {"error":"unauthorized"},
// and the response never echoes the credential. The middleware does not log.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == healthPath {
			next.ServeHTTP(w, r)
			return
		}
		// A request that sends an Authorization header must authenticate with
		// it. A stale cookie does not rescue a bad header.
		if len(r.Header.Values("Authorization")) > 0 {
			if a.bearerAuthorized(r) {
				next.ServeHTTP(w, r)
				return
			}
			writeUnauthorized(w)
			return
		}
		csrf, ok := a.sessionCSRF(r)
		if !ok {
			writeUnauthorized(w)
			return
		}
		if unsafeMethod(r.Method) && !csrfMatches(r, csrf) {
			writeJSON(w, http.StatusForbidden, bodyCSRF)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LoginHandler exchanges the admin token for a session. The API routes it as
// POST /v1/session behind Middleware. The request must carry the admin token
// as its bearer header. The handler checks that header itself, so a session
// cookie cannot be used to mint a new session. The response sets the session
// cookie and returns the CSRF token as {"csrf_token": "..."}.
func (a *Authenticator) LoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.bearerAuthorized(r) {
			writeUnauthorized(w)
			return
		}
		id, csrf, err := a.sessions.Create()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, `{"error":"internal"}`)
			return
		}
		http.SetCookie(w, sessionCookie(r, id, int(a.sessions.TTL().Seconds())))
		body, _ := json.Marshal(struct {
			CSRFToken string `json:"csrf_token"`
		}{csrf})
		writeJSON(w, http.StatusOK, string(body))
	})
}

// LogoutHandler ends the session named by the cookie, if there is one, and
// clears the cookie. The API routes it as DELETE /v1/session behind
// Middleware, so a cookie request needs the same CSRF token as any other
// unsafe cookie request.
func (a *Authenticator) LogoutHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(SessionCookieName); err == nil {
			a.sessions.Delete(c.Value)
		}
		http.SetCookie(w, sessionCookie(r, "", -1))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
}

// bearerAuthorized reports whether the request has exactly one Authorization
// header, that it is a Bearer token, and that the token is the admin token.
func (a *Authenticator) bearerAuthorized(r *http.Request) bool {
	vals := r.Header.Values("Authorization")
	if len(vals) != 1 {
		return false
	}
	tok, ok := ParseBearer(vals[0])
	return ok && VerifyToken(tok, a.adminHash)
}

// sessionCSRF returns the CSRF token of the live session named by the cookie.
func (a *Authenticator) sessionCSRF(r *http.Request) (string, bool) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return "", false
	}
	return a.sessions.Lookup(c.Value)
}

// csrfMatches reports whether the request presents the session's CSRF token,
// in the X-CSRF-Token header or in the csrf_token form field. A missing token
// does not match.
func csrfMatches(r *http.Request, want string) bool {
	presented := r.Header.Get(CSRFHeader)
	if presented == "" {
		presented = r.PostFormValue(csrfField)
	}
	if presented == "" {
		return false
	}
	return sameSecret(presented, want)
}

// sameSecret compares two secrets in constant time. Each side is hashed
// first, so the comparison runs on equal-length inputs and the length of
// the secret does not leak either.
func sameSecret(a, b string) bool {
	da := sha256.Sum256([]byte(a))
	db := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(da[:], db[:]) == 1
}

func unsafeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// sessionCookie builds the UI session cookie. Secure is set only when the
// request arrived over TLS, so loopback HTTP still works. The cookie is never
// visible to script, and SameSite=Strict keeps it off cross-site requests.
func sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	}
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="hivemind"`)
	writeJSON(w, http.StatusUnauthorized, bodyUnauthorized)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
