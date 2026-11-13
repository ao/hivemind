package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/server"
)

const (
	defaultServer           = "http://127.0.0.1:4483"
	defaultClientConfigHint = "$XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml"
	envServer               = "HIVEMIND_SERVER"
	envToken                = "HIVEMIND_TOKEN"
	envClientConfig         = "HIVEMIND_CLIENT_CONFIG"

	requestTimeout  = 60 * time.Second
	maxClientConfig = 64 << 10
	maxResponse     = 16 << 20
)

// errUnauthorized is what every client command reports for a 401.
var errUnauthorized = errors.New("unauthorized")

// clientConfig is the file init writes for the CLI. It names the token file
// rather than holding the token, so the secret lives in one place.
type clientConfig struct {
	Server    string `yaml:"server,omitempty"`
	TokenFile string `yaml:"token-file,omitempty"`
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
	json         bool
}

func addClientFlags(cmd *cobra.Command) *clientFlags {
	f := &clientFlags{}
	fl := cmd.Flags()
	fl.StringVar(&f.server, "server", "", "server URL (env "+envServer+"; default from the client config, then "+defaultServer+")")
	fl.StringVar(&f.token, "token", "", "API token (env "+envToken+"; default from the client config's token file)")
	fl.StringVar(&f.clientConfig, "client-config", "", "client config file (env "+envClientConfig+"; default "+defaultClientConfigHint+")")
	fl.BoolVar(&f.json, "json", false, "print JSON instead of a table")
	return f
}

// apiClient talks to one hivemind server.
type apiClient struct {
	base  string
	token string
	http  *http.Client
}

// client resolves the server and token: flag, then environment, then the
// client config, then the default server.
func (f *clientFlags) client() (*apiClient, error) {
	ccPath, err := clientConfigPath(f.clientConfig)
	explicit := f.clientConfig != "" || os.Getenv(envClientConfig) != ""
	var cc clientConfig
	if err == nil {
		if cc, err = readClientConfig(ccPath, explicit); err != nil {
			return nil, err
		}
	} else if explicit {
		return nil, err
	}

	srv := firstNonEmpty(f.server, os.Getenv(envServer), cc.Server, defaultServer)
	u, err := url.Parse(srv)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("server %q must be an http or https URL, such as %s", srv, defaultServer)
	}

	token := firstNonEmpty(f.token, os.Getenv(envToken))
	if token == "" && cc.TokenFile != "" {
		if filepath.Base(cc.TokenFile) != auth.TokenFileName {
			return nil, fmt.Errorf("client config %s: token-file must name an %s file", ccPath, auth.TokenFileName)
		}
		if token, err = auth.ReadAdminToken(filepath.Dir(cc.TokenFile)); err != nil {
			return nil, err
		}
	}
	if token == "" {
		return nil, fmt.Errorf("no API token: pass --token, set %s, or run `hivemind init` on this machine", envToken)
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = requestTimeout
	return &apiClient{base: strings.TrimRight(srv, "/"), token: token, http: &http.Client{Transport: tr}}, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// send makes one request and returns the response for a 2xx status. Any
// other status becomes an error with the server's message.
func (c *apiClient) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("cannot reach hivemind at %s: %s", c.base, reachReason(err))
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errUnauthorized
	}
	var e server.ErrorBody
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return nil, errors.New(e.Error)
	}
	return nil, fmt.Errorf("server returned %s", resp.Status)
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
