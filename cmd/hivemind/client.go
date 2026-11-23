package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/server"
)

const (
	defaultServer           = "http://127.0.0.1:4483"
	defaultClientConfigHint = "$XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml"
	envServer               = "HIVEMIND_SERVER"
	envToken                = "HIVEMIND_TOKEN"
	envClientConfig         = "HIVEMIND_CLIENT_CONFIG"
	envCACert               = "HIVEMIND_CA_CERT"

	requestTimeout  = 60 * time.Second
	maxClientConfig = 64 << 10
	maxResponse     = 16 << 20

	// With one server, the CLI waits 30 s for the TCP connect. With
	// several, it waits 2 s for it, and 3 s in all for a connection, the
	// TLS handshake included, and then tries the next server: one on the
	// network that has not answered in that time is taken as gone. In the
	// eighth soak, with the first of three servers powered off, every call
	// waited 30 s, then 3.1 s, the kernel's time to give up on the address,
	// before it tried the second. In the ninth, with two servers' processes
	// stopped, whose machines took the connections, each cost a call the
	// 5 s the TLS handshake had then.
	connectTimeout     = 30 * time.Second
	listConnectTimeout = 2 * time.Second
	listConnTimeout    = 3 * time.Second
)

// errNoConn ends a request to one of several servers that has had no
// connection for listConnTimeout.
var errNoConn = errors.New("no connection")

// clientKeepAlive probes an idle connection to the server after 15 s, then
// every 5 s, and drops it after 3 probes go unanswered.
var clientKeepAlive = net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3}

// errUnauthorized is what every client command reports for a 401.
var errUnauthorized = errors.New("unauthorized")

// clientConfig is the file init writes for the CLI. It names the token file
// rather than holding the token, so the secret lives in one place. CACert
// names a PEM file of certificates to trust for an https server, instead of
// the system's roots.
type clientConfig struct {
	Server    string `yaml:"server,omitempty"`
	TokenFile string `yaml:"token-file,omitempty"`
	CACert    string `yaml:"ca-cert,omitempty"`
}

// clientConfigPath returns the explicit path, then HIVEMIND_CLIENT_CONFIG,
// then the user config directory.
func clientConfigPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := os.Getenv(envClientConfig); v != "" {
		return v, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "hivemind", "config.yaml"), nil
}

// writeClientConfig creates path with mode 0600, in a directory created with
// mode 0700. An existing file is not replaced: the error wraps fs.ErrExist.
func writeClientConfig(path string, cc clientConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("client config directory: %w", err)
	}
	b, err := yaml.Marshal(cc)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append([]byte("# Written by hivemind init.\n"), b...)); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// readClientConfig reads path. A missing file is an error only when the path
// was given explicitly.
func readClientConfig(path string, explicit bool) (clientConfig, error) {
	var cc clientConfig
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && !explicit {
		return cc, nil
	}
	if err != nil {
		return cc, fmt.Errorf("client config: %w", err)
	}
	if len(b) > maxClientConfig {
		return cc, fmt.Errorf("client config %s is larger than %d bytes", path, maxClientConfig)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cc); err != nil && !errors.Is(err, io.EOF) {
		return cc, fmt.Errorf("client config %s: %w", path, err)
	}
	return cc, nil
}

// clientFlags are the flags every client command takes.
type clientFlags struct {
	server       string
	token        string
	clientConfig string
	caCert       string
	json         bool
	// cmd is the command the flags belong to, for messages that say how
	// to run it again. It may be nil.
	cmd *cobra.Command
}

// addClientFlags registers the connection flags and --json.
func addClientFlags(cmd *cobra.Command) *clientFlags {
	f := addConnFlags(cmd)
	cmd.Flags().BoolVar(&f.json, "json", false, "print JSON instead of a table")
	return f
}

// addConnFlags registers the flags that say which server to call and how,
// for a command whose output has no JSON form.
func addConnFlags(cmd *cobra.Command) *clientFlags {
	f := &clientFlags{cmd: cmd}
	fl := cmd.Flags()
	fl.StringVar(&f.server, "server", "", "server URL, or the URLs of several servers, comma separated, tried in order from the one that answered last (env "+envServer+"; default from the client config, then "+defaultServer+")")
	fl.StringVar(&f.token, "token", "", "API token (env "+envToken+"; default from the client config's token file)")
	fl.StringVar(&f.clientConfig, "client-config", "", "client config file (env "+envClientConfig+"; default "+defaultClientConfigHint+")")
	fl.StringVar(&f.caCert, "ca-cert", "", "PEM file of the CA certificates to trust for an https server, instead of the system's (env "+envCACert+"; default from the client config)")
	return f
}

// apiClient talks to one hivemind server, or to the first of a list that
// answers.
type apiClient struct {
	base string
	// bases is every server given, base among them; one for most.
	bases []string
	token string
	// tokenFrom says where the token came from, for the error when the
	// server refuses it: "--token", "HIVEMIND_TOKEN" or the token file.
	tokenFrom string
	// caPath is the --ca-cert file, or "" for the system's roots. Errors
	// name it.
	caPath string
	http   *http.Client
	// lastSaved is the server of a list that the file of lastServerPath
	// names, or "" for none (lastServer).
	lastSaved string
}

// client resolves the server and token: flag, then environment, then the
// client config, then the default server.
func (f *clientFlags) client() (*apiClient, error) {
	ccPath, err := clientConfigPath(f.clientConfig)
	explicit := f.clientConfig != "" || os.Getenv(envClientConfig) != ""
	var cc clientConfig
	if err == nil {
		if cc, err = readClientConfig(ccPath, explicit); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				if oerr := othersFilesError(ccPath, commandLine(f.cmd)); oerr != nil {
					return nil, oerr
				}
			}
			return nil, err
		}
	} else if explicit {
		return nil, err
	}

	// A cluster of several servers can be given as a list, comma
	// separated: the CLI tries them in order until one answers
	// (docs/ha.md#the-api).
	var bases []string
	var u *url.URL
	for _, s := range strings.Split(firstNonEmpty(f.server, os.Getenv(envServer), cc.Server, defaultServer), ",") {
		s = strings.TrimSpace(s)
		pu, err := url.Parse(s)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			return nil, fmt.Errorf("server %q must be an http or https URL, such as %s", s, defaultServer)
		}
		if u == nil {
			u = pu
		}
		bases = append(bases, strings.TrimRight(s, "/"))
	}
	srv := bases[0]

	token, tokenFrom := f.token, "--token"
	if token == "" {
		token, tokenFrom = os.Getenv(envToken), envToken
	}
	if token == "" && cc.TokenFile != "" {
		tokenFrom = fmt.Sprintf("the token file %s, which the client config %s names", cc.TokenFile, ccPath)
		if filepath.Base(cc.TokenFile) != auth.TokenFileName {
			return nil, fmt.Errorf("client config %s: token-file must name an %s file", ccPath, auth.TokenFileName)
		}
		if token, err = auth.ReadAdminToken(filepath.Dir(cc.TokenFile)); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				if oerr := othersFilesError(cc.TokenFile, commandLine(f.cmd)); oerr != nil {
					return nil, oerr
				}
			}
			return nil, err
		}
	}
	if token == "" {
		if err := workerHereError(config.DataDir(), "it has no API token"); err != nil {
			return nil, err
		}
		// Without a token, init is the advice only when no one else set
		// up hivemind here: on a server that root set up, init would make
		// a second cluster beside it.
		if err := othersClusterError(commandLine(f.cmd)); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("no API token: pass --token, set %s, or run `hivemind init` on this machine", envToken)
	}

	caPath := firstNonEmpty(f.caCert, os.Getenv(envCACert))
	if caPath == "" && cc.CACert != "" {
		caPath = cc.CACert
		if !filepath.IsAbs(caPath) {
			caPath = filepath.Join(filepath.Dir(ccPath), caPath)
		}
	}
	tlsCfg, err := apiTLSConfig(u, caPath)
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil && len(bases) > 1 {
		// Each server's certificate names that server: the transport
		// checks the host of each request's URL.
		tlsCfg.ServerName = ""
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = requestTimeout
	tr.TLSClientConfig = tlsCfg
	// A followed log can be quiet for hours, so it has no read timeout.
	// TCP keepalives find a server machine that went away, which closes
	// nothing, in about 30 s, where the defaults take some minutes.
	dial := &net.Dialer{Timeout: connectTimeout, KeepAliveConfig: clientKeepAlive}
	var last string
	if len(bases) > 1 {
		dial.Timeout = listConnectTimeout
		tr.TLSHandshakeTimeout = listConnTimeout
		// The server that answered the last command of this list is tried
		// first.
		if last = lastServer(bases); last != "" {
			srv = last
		}
	}
	tr.DialContext = dial.DialContext
	return &apiClient{base: srv, bases: bases, token: token, tokenFrom: tokenFrom, caPath: caPath, http: &http.Client{Transport: tr},
		lastSaved: last}, nil
}

// userCacheDir is os.UserCacheDir, but for tests.
var userCacheDir = os.UserCacheDir

// lastServerPath is the file that names the server of the list bases that
// answered last: in the user's cache directory, one file per list, named
// by a hash of the list, so that two clusters' lists, or two orders of one
// list, do not share one.
func lastServerPath(bases []string) (string, error) {
	d, err := userCacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.Join(bases, "\n")))
	return filepath.Join(d, "hivemind", "server-"+hex.EncodeToString(sum[:8])), nil
}

// lastServer is the server of bases that answered the last command given
// that list, or "" when no file names one. A file that names a server the
// list does not have is ignored: it can only change which of the servers
// given is tried first.
func lastServer(bases []string) string {
	p, err := lastServerPath(bases)
	if err != nil {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return ""
	}
	if s := strings.TrimSpace(string(b)); slices.Contains(bases, s) {
		return s
	}
	return ""
}

// saveLastServer records base as the server of bases that answered last,
// in a file of mode 0600 in a directory of mode 0700, replaced in one
// rename. It is a hint for the next command, so a failure is ignored.
func saveLastServer(bases []string, base string) {
	p, err := lastServerPath(bases)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".server-*")
	if err != nil {
		return
	}
	_, werr := f.WriteString(base + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil {
		os.Remove(f.Name())
		return
	}
	if err := os.Rename(f.Name(), p); err != nil {
		os.Remove(f.Name())
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// While a server answers that no server can take the request now, as
// during an election, the CLI tries the servers again every leaderRetry,
// for up to leaderWait after the first such answer: a new leader leads 1
// to 2.5 s after the old one is lost (eighth soak).
var (
	leaderWait  = 3 * time.Second
	leaderRetry = 500 * time.Millisecond
)

// send makes one request and returns the response for a 2xx status. Any
// other status becomes an error with the server's message. With a list of
// servers, it starts at the one that answered last, and passes for the
// next one a server it cannot connect to, and one that answers that it
// did not act on the request and another server may (a 503 marked no
// leader). It also passes a server whose connection broke after the
// request was sent, but only for a GET or a HEAD: a write sent again
// could be applied twice. When some server answered that none leads, it
// tries them all again for a few seconds (leaderWait), and if no server
// takes the request by then, returns the last such answer, which says why,
// rather than the error of a server that did not answer. In the ninth
// soak, with no leader and two servers' processes stopped, the CLI
// printed the last server's TLS handshake timeout. Given one server that
// answers that it has known no leader for leaderWait or longer
// (nodeapi.NoLeaderForHeader), it returns that answer at once: the fifth
// acceptance run, with one of two servers stopped, waited 3 s for each
// call before the same answer. One that has known no leader for less, as
// during an election, or knows a leader it could not reach, as while the
// leadership passes on, or does not say, as before 2.4.0, is tried again.
func (c *apiClient) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	safe := method == http.MethodGet || method == http.MethodHead
	var err, noLeaderErr error
	var firstNoLeader time.Time
	// why is what send returns when no server took the request.
	why := func() error {
		if noLeaderErr != nil {
			return noLeaderErr
		}
		return err
	}
	for {
		noLeader := false
		start := 0
		for i, s := range c.bases {
			if s == c.base {
				start = i
			}
		}
		for i := 0; i < max(len(c.bases), 1); i++ {
			base := c.base
			if len(c.bases) > 0 {
				base = c.bases[(start+i)%len(c.bases)]
			}
			var resp *http.Response
			resp, err = c.sendTo(ctx, base, method, path, b, body != nil)
			if err == nil {
				c.base = base
				if len(c.bases) > 1 && base != c.lastSaved {
					saveLastServer(c.bases, base)
					c.lastSaved = base
				}
				return resp, nil
			}
			var re *reachError
			var ae *apiError
			switch {
			case ctx.Err() != nil:
				return nil, why()
			case errors.As(err, &ae) && ae.noLeader && len(c.bases) <= 1 && ae.noLeaderFor >= leaderWait:
				// One server, which has known no leader for longer
				// than an election takes: there is no other to try, and
				// its answer says why.
				return nil, err
			case errors.As(err, &ae) && ae.noLeader:
				noLeader, noLeaderErr = true, err
				if firstNoLeader.IsZero() {
					firstNoLeader = time.Now()
				}
			case errors.As(err, &re) && (!re.sent || safe):
			default:
				return nil, err
			}
		}
		if !noLeader || time.Since(firstNoLeader) >= leaderWait {
			return nil, why()
		}
		t := time.NewTimer(leaderRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, why()
		case <-t.C:
		}
	}
}

// reachError is a request that got no answer from a server: it could not
// connect, or the connection broke. sent is true once the whole request
// was written to the connection, after which the server may have acted on
// it.
type reachError struct {
	msg  string
	sent bool
}

func (e *reachError) Error() string { return e.msg }

// closeThen is a reply's body that calls then once it is closed: the reply
// is read under the request's context, which then ends.
type closeThen struct {
	io.ReadCloser
	then func()
}

func (c *closeThen) Close() error {
	err := c.ReadCloser.Close()
	c.then()
	return err
}

// sendTo is send to one server. With several servers, the request ends if
// it has no connection to this one after listConnTimeout, whatever it
// waits for: the TCP connect, the TLS handshake, or a connection another
// request is opening. A server whose process is stopped, as in the ninth
// soak, takes the TCP connection in its kernel and never answers the
// handshake.
func (c *apiClient) sendTo(ctx context.Context, base, method, path string, b []byte, hasBody bool) (*http.Response, error) {
	var rd io.Reader
	if hasBody {
		rd = bytes.NewReader(b)
	}
	var sent atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			sent.Store(true)
		}
	}}
	rctx, done := ctx, func() {}
	if len(c.bases) > 1 {
		var cancel context.CancelCauseFunc
		rctx, cancel = context.WithCancelCause(ctx)
		slow := time.AfterFunc(listConnTimeout, func() { cancel(errNoConn) })
		trace.GotConn = func(httptrace.GotConnInfo) { slow.Stop() }
		done = func() {
			slow.Stop()
			cancel(nil)
		}
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(rctx, trace), method, base+path, rd)
	if err != nil {
		done()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		done()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(context.Cause(rctx), errNoConn) {
			return nil, &reachError{msg: fmt.Sprintf("cannot reach hivemind at %s: no connection in %s", base, listConnTimeout)}
		}
		if msg := tlsReason(err, c.caPath); msg != "" {
			return nil, &reachError{msg: fmt.Sprintf("cannot reach hivemind at %s: %s", base, msg)}
		}
		re := &reachError{msg: fmt.Sprintf("cannot reach hivemind at %s: %s", base, reachReason(err)), sent: sent.Load()}
		if re.sent && method != http.MethodGet && method != http.MethodHead {
			re.msg = fmt.Sprintf("lost the connection to hivemind at %s after the request was sent (%s), so it may or may not have been applied: check, and try again if it was not",
				base, reachReason(err))
		}
		return nil, re
	}
	if resp.StatusCode/100 == 2 {
		resp.Body = &closeThen{ReadCloser: resp.Body, then: done}
		return resp, nil
	}
	defer done()
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, c.refused()
	}
	var e server.ErrorBody
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusBadRequest && bytes.Contains(rb, []byte("Client sent an HTTP request to an HTTPS server")) {
		// Go's reply on a TLS port to a request in plain HTTP.
		return nil, fmt.Errorf("the server at %s speaks https: use --server %s", base, "https://"+strings.TrimPrefix(base, "http://"))
	}
	v := nodeapi.CleanVersion(resp.Header.Get(nodeapi.VersionHeader))
	ae := &apiError{status: resp.StatusCode, msg: "server returned " + resp.Status, version: v, noLeaderFor: -1}
	ae.noLeader = resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get(nodeapi.NoLeaderHeader) != ""
	if s, err := strconv.ParseInt(resp.Header.Get(nodeapi.NoLeaderForHeader), 10, 64); ae.noLeader && err == nil && s >= 0 && s < 1<<31 {
		ae.noLeaderFor = time.Duration(s) * time.Second
	}
	if json.Unmarshal(rb, &e) == nil && e.Error != "" {
		ae.msg = termSafe(e.Error)
	}
	return nil, ae
}

// refused is the error for a 401: the server refused the token. It says
// where the token came from, so that a token meant for another cluster, as
// the client config's is when --server names another server, is found.
func (c *apiClient) refused() error {
	if c.tokenFrom == "" {
		return errUnauthorized
	}
	msg := fmt.Sprintf("the server at %s refused the token from %s", c.base, c.tokenFrom)
	if c.tokenFrom != "--token" && c.tokenFrom != envToken {
		msg += "; for another cluster's server, give its token with --token or " + envToken
	}
	return fmt.Errorf("%w: %s", errUnauthorized, msg)
}

// apiError is an error reply from the server. Its text is the server's
// message; status lets a command tell a 404 from other failures. version is
// the server's, or "" when it sends none.
type apiError struct {
	status  int
	msg     string
	version string
	// noLeader is a 503 that says no server leads (docs/ha.md), and
	// noLeaderFor how long the server had known none, or -1 when it does
	// not say.
	noLeader    bool
	noLeaderFor time.Duration
}

func (e *apiError) Error() string { return e.msg }

// noRoute reports whether the server has no handler for the route: the
// router's own 404 or 405, which the API sends as "not found" and "method
// not allowed". A handler's 404 names what it did not find.
func (e *apiError) noRoute() bool {
	return e.status == http.StatusNotFound && e.msg == "not found" ||
		e.status == http.StatusMethodNotAllowed && e.msg == "method not allowed"
}

// olderServer turns a refusal of a route the server lacks into what to do.
// what names the command, such as "node drain", and since is the release
// that added its route. Any other error is returned as it is.
func olderServer(err error, what, since string) error {
	var ae *apiError
	if !errors.As(err, &ae) || !ae.noRoute() {
		return err
	}
	if ae.version != "" {
		return fmt.Errorf("the server runs %s, which has no %s; upgrade the server", ae.version, what)
	}
	return fmt.Errorf("the server is older than %s and has no %s; upgrade the server", since, what)
}

// call makes a request and decodes the JSON reply into out.
func (c *apiClient) call(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(out); err != nil {
		return fmt.Errorf("reading the reply from %s: %w", c.base, err)
	}
	return nil
}

// serverVersion asks the server for its version: GET /v1/status, whose reply
// carries it in nodeapi.VersionHeader, as every authenticated reply does. ""
// means the server reports none.
func (c *apiClient) serverVersion(ctx context.Context) (string, error) {
	resp, err := c.send(ctx, "GET", "/v1/status", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	return nodeapi.CleanVersion(resp.Header.Get(nodeapi.VersionHeader)), nil
}

// reachReason shortens a transport error to its cause, such as
// "connection refused".
func reachReason(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func appPath(name string, rest ...string) string {
	p := "/v1/apps/" + url.PathEscape(name)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}
