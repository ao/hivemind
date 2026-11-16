// Package docker implements runtime.Runtime on the Docker Engine HTTP API
// (v1.44). It talks to the daemon directly, without the Docker client module.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

// APIVersion is the Engine API version every request is sent to.
const APIVersion = "1.44"

// DefaultHost is used when neither the host argument nor DOCKER_HOST is set.
const DefaultHost = "unix:///var/run/docker.sock"

// maxErrorBody bounds how much of an error response is read.
const maxErrorBody = 64 << 10

// Client talks to one Docker daemon. It is safe for concurrent use.
type Client struct {
	base string // scheme and authority for requests, without the API version
	http *http.Client
}

var _ runtime.Runtime = (*Client)(nil)

// New returns a client for host. An empty host reads DOCKER_HOST, then falls
// back to DefaultHost. Two forms are accepted:
//
//	unix:///path/to/docker.sock   the usual case
//	tcp://127.0.0.1:2375          plain HTTP, for tests; loopback addresses only
//
// The tcp form is unencrypted, so a remote address is refused.
func New(host string) (*Client, error) {
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		host = DefaultHost
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("docker host %q: %w", host, err)
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{Proxy: nil} // never send the daemon's traffic through an HTTP proxy
	c := &Client{http: &http.Client{Transport: tr}}

	switch u.Scheme {
	case "unix":
		if !strings.HasPrefix(u.Path, "/") || u.Host != "" {
			return nil, fmt.Errorf("docker host %q: unix hosts need an absolute path, such as unix:///var/run/docker.sock", host)
		}
		path := u.Path
		c.base = "http://docker"
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		}
	case "tcp":
		h, _, err := net.SplitHostPort(u.Host)
		if err != nil {
			return nil, fmt.Errorf("docker host %q: want tcp://host:port: %v", host, err)
		}
		if !isLoopback(h) {
			return nil, fmt.Errorf("docker host %q: tcp hosts must be loopback addresses, because the traffic is not encrypted", host)
		}
		c.base = "http://" + u.Host
		tr.DialContext = dialer.DialContext
	default:
		return nil, fmt.Errorf("docker host %q: scheme must be unix or tcp", host)
	}
	return c, nil
}

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// call sends one request and returns the response when its status is 2xx or
// one of allow. Any other status is returned as an *runtime.Error, and the
// body is closed. Transport failures map to runtime.ErrUnavailable, unless the
// context was cancelled, in which case the context error is returned.
func (c *Client) call(ctx context.Context, op, method, path string, query url.Values, body any, allow ...int) (*http.Response, error) {
	return c.callWith(ctx, op, method, path, query, nil, body, allow...)
}

// callWith is call with extra request headers.
func (c *Client) callWith(ctx context.Context, op, method, path string, query url.Values, header http.Header, body any, allow ...int) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, &runtime.Error{Op: op, Message: "encoding request", Cause: err}
		}
		rd = bytes.NewReader(b)
	}
	u := c.base + "/v" + APIVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, &runtime.Error{Op: op, Message: "building request", Cause: err}
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError(ctx, op, err)
	}
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || containsInt(allow, resp.StatusCode) {
		return resp, nil
	}
	defer resp.Body.Close()
	return nil, apiError(op, resp)
}

// transportError maps an error from the connection itself.
func (c *Client) transportError(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// The URL of the request, which url.Error puts in front of the cause,
	// is the same for every call and says nothing to the reader.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &runtime.Error{Op: op, Err: runtime.ErrUnavailable, Message: "lost the connection to the Docker daemon", Cause: err}
}

// readError maps an error from reading a response body. A JSON syntax error
// means the daemon sent something unexpected, not that it went away.
func (c *Client) readError(ctx context.Context, op string, err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return &runtime.Error{Op: op, Message: "unexpected response from the Docker daemon", Cause: err}
	}
	return c.transportError(ctx, op, err)
}

// apiError builds an error from a non-2xx response and closes its body.
func apiError(op string, resp *http.Response) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := strings.TrimSpace(string(raw))
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Message != "" {
		msg = body.Message
	}
	if msg == "" {
		msg = resp.Status
	}
	return classify(op, resp.StatusCode, msg)
}

// classify maps an engine status and message to a typed error.
func classify(op string, status int, msg string) *runtime.Error {
	e := &runtime.Error{Op: op, Message: msg}
	switch {
	case status == http.StatusNotFound:
		e.Err = runtime.ErrNotFound
	case status == http.StatusConflict, portInUse(msg):
		// Engines have reported "port is already allocated" with 500 and with 409.
		e.Err = runtime.ErrConflict
	}
	return e
}

func portInUse(msg string) bool {
	return strings.Contains(msg, "port is already allocated") || strings.Contains(msg, "address already in use")
}

// pullNotFound reports whether a pull error means the image does not exist,
// or is not visible to anonymous pulls, which is the usual result of a typo.
func pullNotFound(msg string) bool {
	for _, s := range []string{"manifest unknown", "not found", "pull access denied", "repository does not exist"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// pullDeniedRE matches a registry's DENIED error code as the daemon words
// it: "...: denied" or "denied: ...", but not "permission denied" from the
// daemon's own file system.
var pullDeniedRE = regexp.MustCompile(`(^|: )denied(:|$)`)

// pullRefusal reads a failed pull's HTTP status (0 for an error in the
// progress stream) and message. refused is true when the registry refused
// the pull for want of a credential, or refused the one sent. ambiguous is
// true when the registry said so in words it also uses for an image that
// does not exist, as Docker Hub does for a private repository. reason is a
// short, fixed description, with nothing the registry said in it.
//
// The words are the daemon's and the registries'. With the classic image
// store: "no basic auth credentials", "unauthorized: authentication
// required", and Docker Hub's "pull access denied for <repo>, repository
// does not exist or may require 'docker login': denied: requested access to
// the resource is denied". With the containerd image store (the default of
// a new Docker 29 daemon, docker:dind included): "pull access denied,
// repository does not exist or may require authorization: authorization
// failed: no basic auth credentials" without a credential, and "unexpected
// status from HEAD request to <url>: 401 Unauthorized" with a wrong one.
// ECR says "denied: Your authorization token has expired".
func pullRefusal(status int, msg string) (refused, ambiguous bool, reason string) {
	m := strings.ToLower(msg)
	has := func(ss ...string) bool {
		for _, s := range ss {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("no basic auth credentials"):
		return true, false, "no credential"
	case has("token has expired"):
		return true, false, "the token has expired"
	case has("incorrect username or password"):
		return true, false, "incorrect username or password"
	case status == http.StatusUnauthorized || has("unauthorized", "authentication required"):
		return true, false, "unauthorized"
	case status == http.StatusForbidden || has("403 forbidden"):
		return true, false, "forbidden"
	case has("pull access denied", "may require 'docker login'", "may require authorization",
		"requested access to the resource is denied") || pullDeniedRE.MatchString(m):
		return true, true, "no such image, or no access to it"
	}
	return false, false, ""
}

// classifyPull maps a failed pull, from its HTTP status (0 for an error in
// the progress stream) and message, to a typed error.
func classifyPull(status int, msg string) *runtime.Error {
	e := classify("pull", status, msg)
	refused, ambiguous, _ := pullRefusal(status, msg)
	notFound := ambiguous || (!refused && (errors.Is(e.Err, runtime.ErrNotFound) || pullNotFound(msg)))
	switch {
	case refused && notFound:
		e.Err = errors.Join(runtime.ErrNotFound, runtime.ErrUnauthorized)
	case refused:
		e.Err = runtime.ErrUnauthorized
	case notFound:
		e.Err = runtime.ErrNotFound
	}
	return e
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// containerPath returns the API path for a container, escaping the ID.
func containerPath(op, id string) (string, error) {
	if id == "" {
		return "", &runtime.Error{Op: op, Err: runtime.ErrInvalid, Message: "container ID is empty"}
	}
	return "/containers/" + url.PathEscape(id), nil
}
