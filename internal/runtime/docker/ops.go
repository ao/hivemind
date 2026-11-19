package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
)

// Info returns the daemon's CPU count, memory, version and default log
// driver.
func (c *Client) Info(ctx context.Context) (runtime.Info, error) {
	const op = "info"
	resp, err := c.call(ctx, op, http.MethodGet, "/info", nil, nil)
	if err != nil {
		return runtime.Info{}, err
	}
	defer resp.Body.Close()
	var body struct {
		NCPU          int    `json:"NCPU"`
		MemTotal      int64  `json:"MemTotal"`
		ServerVersion string `json:"ServerVersion"`
		LoggingDriver string `json:"LoggingDriver"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return runtime.Info{}, c.readError(ctx, op, err)
	}
	return runtime.Info{NCPU: body.NCPU, MemTotal: body.MemTotal, ServerVersion: body.ServerVersion, LogDriver: body.LoggingDriver}, nil
}

// Log rotation for the json-file driver: each container keeps at most
// LogFiles files of LogMaxSize, the current one included. These are the
// driver's max-size and max-file options.
const (
	LogMaxSize = "10m"
	LogFiles   = "5"
)

// logDriverTTL is how long the daemon's default log driver is taken as
// read. A daemon restarted with another driver is seen within it.
const logDriverTTL = time.Minute

// newLogConfig returns the LogConfig for a new container. With the daemon's
// default driver json-file, which keeps every line a container writes in
// one file, it is json-file with LogMaxSize and LogFiles; with any other
// driver it is nil, which leaves the daemon's choice. The daemon merges the
// other options of its log-opts into a container's when the driver is the
// same, so only these two are replaced. The driver is read from /info at
// most once a logDriverTTL; an older value is used when /info fails, and
// the error is returned when there is none.
func (c *Client) newLogConfig(ctx context.Context) (*logConfig, error) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	now := c.now()
	if !c.logAt.IsZero() && now.Sub(c.logAt) < logDriverTTL {
		return logConfigFor(c.logDriver), nil
	}
	info, err := c.Info(ctx)
	if err != nil {
		if c.logAt.IsZero() {
			return nil, err
		}
		return logConfigFor(c.logDriver), nil
	}
	c.logDriver, c.logAt = info.LogDriver, now
	return logConfigFor(c.logDriver), nil
}

func logConfigFor(driver string) *logConfig {
	if driver != "json-file" {
		return nil
	}
	return &logConfig{Type: "json-file", Config: map[string]string{"max-size": LogMaxSize, "max-file": LogFiles}}
}

// Pull fetches an image and reads the progress stream to the end. The engine
// reports a failure before the pull starts with an HTTP status, and one
// after it starts inside the stream, with HTTP 200, so both are checked.
//
// A credential goes in the X-Registry-Auth header (EncodeRegistryAuth). The
// daemon uses it for the registry the reference names. When one was sent,
// the error of a refused pull says only that, with a reason in
// parentheses, so that it holds nothing the registry echoed, such as a
// token URL with the username in it; any other error has the password, the
// header and such account parameters taken out. Without a credential, the
// daemon's message is kept: it can hold no secret.
func (c *Client) Pull(ctx context.Context, ref string, auth *runtime.RegistryAuth) error {
	const op = "pull"
	img, err := spec.ParseImage(ref)
	if err != nil {
		return fmt.Errorf("%w: image: %v", runtime.ErrInvalid, err)
	}
	q := url.Values{}
	if img.Digest != "" {
		q.Set("fromImage", img.Repository()+"@"+img.Digest)
	} else {
		q.Set("fromImage", img.Repository())
		q.Set("tag", img.Tag)
	}
	var hdr http.Header
	var header string
	if auth != nil {
		if header, err = EncodeRegistryAuth(*auth); err != nil {
			return &runtime.Error{Op: op, Err: runtime.ErrInvalid, Message: "encoding the registry credential"}
		}
		hdr = http.Header{"X-Registry-Auth": {header}}
	}
	fail := func(status int, msg string) error {
		e := classifyPull(status, msg)
		if auth != nil && errors.Is(e.Err, runtime.ErrUnauthorized) {
			_, _, reason := pullRefusal(status, msg)
			e.Message = "the registry refused the pull (" + reason + ")"
		} else {
			e.Message = redact(e.Message, auth, header)
		}
		return e
	}
	// Every status comes back as a response, so that its message is
	// classified, and redacted, here.
	resp, err := c.callWith(ctx, op, http.MethodPost, "/images/create", q, hdr, nil,
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError)
	if err != nil {
		var re *runtime.Error
		if errors.As(err, &re) {
			re.Message = redact(re.Message, auth, header)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
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
		return fail(resp.StatusCode, msg)
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var m struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		err := dec.Decode(&m)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return c.readError(ctx, op, err)
		}
		msg := m.Error
		if msg == "" {
			msg = m.ErrorDetail.Message
		}
		if msg != "" {
			return fail(0, msg)
		}
	}
}

// EncodeRegistryAuth returns the X-Registry-Auth header for a credential.
// The Engine API (v1.44) asks for "a base64url encoded (JSON) string" of
// {"username", "password", "serveraddress"}, where serveraddress "is a
// domain/IP without a protocol": the Authentication section of
// https://docs.docker.com/reference/api/engine/version/v1.44/#section/Authentication,
// which the X-Registry-Auth parameter of ImageCreate (POST /images/create)
// refers to. base64url is RFC 4648 section 5; the padding is kept, as the
// Docker CLI writes it (registry.EncodeAuthConfig uses
// base64.URLEncoding).
func EncodeRegistryAuth(a runtime.RegistryAuth) (string, error) {
	b, err := json.Marshal(struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
	}{a.Username, a.Password, a.ServerAddress})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// accountRE matches the account parameter of a registry token request,
// which names the user: ?account=<username>&scope=...
var accountRE = regexp.MustCompile(`account=[^&\s"']*`)

// redact takes the password, in each form secretForms lists, the header and
// the username of a token URL out of an error message.
func redact(msg string, auth *runtime.RegistryAuth, header string) string {
	if auth == nil {
		return msg
	}
	const r = "[redacted]"
	forms := secretForms(auth.Username, auth.Password)
	if header != "" {
		forms = append(forms, header)
	}
	// Longest first, so that a form inside a longer one, as unpadded base64
	// is inside padded, does not leave the rest of that one behind.
	slices.SortFunc(forms, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	for _, f := range slices.Compact(forms) {
		msg = strings.ReplaceAll(msg, f, r)
	}
	return accountRE.ReplaceAllString(msg, "account="+r)
}

// secretForms returns the ways a message from the daemon or a registry may
// write a password: as it is; percent-escaped for a query or a path, with
// upper- or lower-case hex; escaped in a JSON string, with and without
// Go's escapes of <, > and &; escaped as Go's %q and %+q write it; and the
// Basic credential user:password in base64, standard or URL-safe, padded
// or not. An empty password has none.
func secretForms(user, password string) []string {
	if password == "" {
		return nil
	}
	query, path := url.QueryEscape(password), url.PathEscape(password)
	forms := []string{password, query, path, lowerHex(query), lowerHex(path),
		jsonString(password, true), jsonString(password, false),
		unquote(strconv.Quote(password)), unquote(strconv.QuoteToASCII(password))}
	basic := []byte(user + ":" + password)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		forms = append(forms, enc.EncodeToString(basic))
	}
	return slices.DeleteFunc(forms, func(s string) bool { return s == "" })
}

// lowerHex lowers the hex digits of each percent escape in s.
func lowerHex(s string) string {
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == '%' {
			for j := i + 1; j <= i+2; j++ {
				if b[j] >= 'A' && b[j] <= 'F' {
					b[j] += 'a' - 'A'
				}
			}
			i += 2
		}
	}
	return string(b)
}

// jsonString is s as it is written inside a JSON string, without the quotes.
func jsonString(s string, escapeHTML bool) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(escapeHTML)
	if err := enc.Encode(s); err != nil {
		return ""
	}
	return unquote(strings.TrimSuffix(b.String(), "\n"))
}

// unquote takes the quotes off a quoted string.
func unquote(q string) string {
	if len(q) < 2 {
		return ""
	}
	return q[1 : len(q)-1]
}

// Create makes a container with the hivemind labels, a restart policy of
// "no", the requested ports and volumes, the CPU and memory limits, and
// log rotation when the daemon's driver is json-file (newLogConfig). Ports
// default to 127.0.0.1.
func (c *Client) Create(ctx context.Context, cfg runtime.ContainerConfig) (string, error) {
	const op = "create"
	cfg, err := runtime.Prepare(cfg)
	if err != nil {
		return "", err
	}
	logs, err := c.newLogConfig(ctx)
	if err != nil {
		return "", err
	}

	env := make([]string, 0, len(cfg.Env))
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	slices.Sort(env)

	exposed := map[string]struct{}{}
	bindings := map[string][]portBinding{}
	for _, p := range cfg.Ports {
		key := strconv.Itoa(p.ContainerPort) + "/" + p.Protocol
		exposed[key] = struct{}{}
		// Every binding names its host port, a route port's too: the engine
		// binds that port again at each start of the container, where an
		// empty HostPort would have it pick a new one each time ("If
		// HostPort is an empty string, the system assigns a port", the
		// PortBinding definition in the Engine API).
		bindings[key] = append(bindings[key], portBinding{
			HostIP:   p.HostIP,
			HostPort: strconv.Itoa(p.HostPort),
		})
	}
	// A mount of type "volume" makes the engine create a missing volume with
	// the given name and options, labels included, as part of this request.
	// That is one call with no window between creating and mounting, and the
	// engine handles two creates of one name. An existing volume is mounted
	// as it is; its labels are not changed.
	mounts := make([]mount, 0, len(cfg.Volumes))
	for _, v := range cfg.Volumes {
		mounts = append(mounts, mount{
			Type:          "volume",
			Source:        runtime.VolumeName(cfg.App, v.Name),
			Target:        v.Path,
			VolumeOptions: &volumeOptions{Labels: runtime.VolumeLabels(cfg.App, v.Name)},
		})
	}

	body := createRequest{
		Image:        cfg.Image,
		Cmd:          cfg.Command,
		Env:          env,
		Labels:       cfg.Labels(),
		Tty:          false, // multiplexed logs; see StdCopy
		ExposedPorts: exposed,
		HostConfig: hostConfig{
			PortBindings:  bindings,
			Mounts:        mounts,
			RestartPolicy: restartPolicy{Name: "no"},
			NanoCpus:      nanoCPUs(cfg.CPU),
			Memory:        cfg.Memory,
			MemorySwap:    cfg.Memory,
			LogConfig:     logs,
		},
	}
	resp, err := c.call(ctx, op, http.MethodPost, "/containers/create",
		url.Values{"name": {cfg.Name}}, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", c.readError(ctx, op, err)
	}
	if out.ID == "" {
		return "", &runtime.Error{Op: op, Message: "the daemon returned no container ID"}
	}
	return out.ID, nil
}

// Start starts a container. A container that is already running is not an error.
func (c *Client) Start(ctx context.Context, id string) error {
	const op = "start"
	p, err := containerPath(op, id)
	if err != nil {
		return err
	}
	resp, err := c.call(ctx, op, http.MethodPost, p+"/start", nil, nil, http.StatusNotModified)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Stop stops a container. The timeout is rounded up to whole seconds, which
// is the engine's resolution. A stopped container is not an error.
func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) error {
	const op = "stop"
	p, err := containerPath(op, id)
	if err != nil {
		return err
	}
	if timeout < 0 {
		return &runtime.Error{Op: op, Err: runtime.ErrInvalid, Message: "timeout must not be negative"}
	}
	secs := int(math.Ceil(timeout.Seconds()))
	resp, err := c.call(ctx, op, http.MethodPost, p+"/stop",
		url.Values{"t": {strconv.Itoa(secs)}}, nil, http.StatusNotModified)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Remove deletes a container and its anonymous volumes, the ones an image's
// VOLUME instruction makes: v=true, "remove anonymous volumes associated
// with the container" (ContainerDelete in the Engine API). The engine never
// removes a named volume this way, so an app's volumes,
// hivemind-<app>_<volume>, stay with their data. Without v, every replaced
// container of such an image left a volume behind, with its data.
func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	const op = "remove"
	p, err := containerPath(op, id)
	if err != nil {
		return err
	}
	q := url.Values{"v": {"true"}, "force": {strconv.FormatBool(force)}}
	resp, err := c.call(ctx, op, http.MethodDelete, p, q, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Inspect returns one container.
func (c *Client) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	const op = "inspect"
	p, err := containerPath(op, id)
	if err != nil {
		return runtime.Container{}, err
	}
	resp, err := c.call(ctx, op, http.MethodGet, p+"/json", nil, nil)
	if err != nil {
		return runtime.Container{}, err
	}
	defer resp.Body.Close()
	var b struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Status     string    `json:"Status"`
			ExitCode   int       `json:"ExitCode"`
			OOMKilled  bool      `json:"OOMKilled"`
			StartedAt  time.Time `json:"StartedAt"`
			FinishedAt time.Time `json:"FinishedAt"`
		} `json:"State"`
		HostConfig struct {
			RestartPolicy struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
		} `json:"HostConfig"`
		NetworkSettings struct {
			IPAddress string `json:"IPAddress"`
			Networks  map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
			Ports map[string][]portBinding `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return runtime.Container{}, c.readError(ctx, op, err)
	}
	return runtime.Container{
		ID:            b.ID,
		Name:          strings.TrimPrefix(b.Name, "/"),
		Image:         b.Config.Image,
		Labels:        b.Config.Labels,
		State:         b.State.Status,
		ExitCode:      b.State.ExitCode,
		OOMKilled:     b.State.OOMKilled,
		RestartPolicy: b.HostConfig.RestartPolicy.Name,
		IPAddress:     ipAddress(b.NetworkSettings.IPAddress, b.NetworkSettings.Networks),
		StartedAt:     b.State.StartedAt,
		FinishedAt:    b.State.FinishedAt,
		Ports:         inspectPorts(b.NetworkSettings.Ports),
	}, nil
}

// inspectPorts reads NetworkSettings.Ports, a map from "80/tcp" to the
// bindings the engine made, into a sorted list. Entries it cannot read are
// left out.
func inspectPorts(m map[string][]portBinding) []runtime.PortBinding {
	var out []runtime.PortBinding
	for key, bs := range m {
		port, proto, ok := strings.Cut(key, "/")
		cp, err := strconv.Atoi(port)
		if !ok || err != nil {
			continue
		}
		for _, b := range bs {
			hp, err := strconv.Atoi(b.HostPort)
			if err != nil || hp < 1 || hp > 65535 {
				continue
			}
			out = append(out, runtime.PortBinding{HostIP: b.HostIP, HostPort: hp, ContainerPort: cp, Protocol: proto})
		}
	}
	sortPorts(out)
	return out
}

func sortPorts(ps []runtime.PortBinding) {
	slices.SortFunc(ps, func(x, y runtime.PortBinding) int {
		if x.ContainerPort != y.ContainerPort {
			return x.ContainerPort - y.ContainerPort
		}
		if x.Protocol != y.Protocol {
			return strings.Compare(x.Protocol, y.Protocol)
		}
		if x.HostIP != y.HostIP {
			return strings.Compare(x.HostIP, y.HostIP)
		}
		return x.HostPort - y.HostPort
	})
}

// List returns every container, stopped ones included, whose labels match.
// The engine's list output has no exit code, restart policy or OOM flag, so
// those fields are zero. Use Inspect for them.
func (c *Client) List(ctx context.Context, labels map[string]string) ([]runtime.Container, error) {
	const op = "list"
	match := make([]string, 0, len(labels))
	for k, v := range labels {
		if v == "" {
			match = append(match, k)
		} else {
			match = append(match, k+"="+v)
		}
	}
	slices.Sort(match)
	filters, err := json.Marshal(map[string][]string{"label": match})
	if err != nil {
		return nil, &runtime.Error{Op: op, Message: "encoding filters", Cause: err}
	}
	resp, err := c.call(ctx, op, http.MethodGet, "/containers/json",
		url.Values{"all": {"1"}, "filters": {string(filters)}}, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
		State  string            `json:"State"`
		// Ports is the list output's Port: "IP", "PrivatePort",
		// "PublicPort" (absent when not published) and "Type".
		Ports []struct {
			IP          string `json:"IP"`
			PrivatePort int    `json:"PrivatePort"`
			PublicPort  int    `json:"PublicPort"`
			Type        string `json:"Type"`
		} `json:"Ports"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, c.readError(ctx, op, err)
	}
	out := make([]runtime.Container, 0, len(list))
	for _, item := range list {
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		var ports []runtime.PortBinding
		for _, p := range item.Ports {
			if p.PublicPort < 1 || p.PublicPort > 65535 {
				continue
			}
			ports = append(ports, runtime.PortBinding{HostIP: p.IP, HostPort: p.PublicPort, ContainerPort: p.PrivatePort, Protocol: p.Type})
		}
		sortPorts(ports)
		out = append(out, runtime.Container{
			ID:     item.ID,
			Name:   name,
			Image:  item.Image,
			Labels: item.Labels,
			State:  item.State,
			Ports:  ports,
		})
	}
	return out, nil
}

// Logs copies the container's log to stdout and stderr. With Follow it blocks
// until the container exits or ctx is cancelled.
func (c *Client) Logs(ctx context.Context, id string, opts runtime.LogOptions, stdout, stderr io.Writer) error {
	const op = "logs"
	p, err := containerPath(op, id)
	if err != nil {
		return err
	}
	if opts.Tail < 0 {
		return &runtime.Error{Op: op, Err: runtime.ErrInvalid, Message: "tail must not be negative"}
	}
	tail := "all"
	if opts.Tail > 0 {
		tail = strconv.Itoa(opts.Tail)
	}
	q := url.Values{
		"stdout": {"1"},
		"stderr": {"1"},
		"follow": {strconv.FormatBool(opts.Follow)},
		"tail":   {tail},
	}
	resp, err := c.call(ctx, op, http.MethodGet, p+"/logs", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	src := &readTracker{r: resp.Body}
	err = StdCopy(stdout, stderr, src)
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// A failed read from the connection, or a frame cut short by the end of
	// the stream, means the daemon went away. A failed write is the caller's.
	if errors.Is(err, io.ErrUnexpectedEOF) || (src.err != nil && src.err != io.EOF) {
		return &runtime.Error{Op: op, Err: runtime.ErrUnavailable, Message: "lost the connection to the Docker daemon during the log stream", Cause: err}
	}
	return &runtime.Error{Op: op, Message: "writing the log output", Cause: err}
}

// Exec runs cmd in a running container. The process's output is discarded:
// probes need the exit code only. The stream is read to its end, which the
// daemon closes when the process exits, and then the exit code is asked for.
// A cancelled context closes the connection; the process is then ended by the
// daemon, not by hivemind.
func (c *Client) Exec(ctx context.Context, id string, cmd []string) (int, error) {
	const op = "exec"
	p, err := containerPath(op, id)
	if err != nil {
		return 0, err
	}
	if len(cmd) == 0 {
		return 0, &runtime.Error{Op: op, Err: runtime.ErrInvalid, Message: "command is empty"}
	}
	resp, err := c.call(ctx, op, http.MethodPost, p+"/exec", nil, execCreateRequest{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return 0, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if err != nil {
		return 0, c.readError(ctx, op, err)
	}
	if created.ID == "" {
		return 0, &runtime.Error{Op: op, Message: "the daemon returned no exec ID"}
	}

	resp, err = c.call(ctx, op, http.MethodPost, "/exec/"+url.PathEscape(created.ID)+"/start", nil,
		execStartRequest{Detach: false, Tty: false})
	if err != nil {
		return 0, err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		return 0, c.readError(ctx, op, err)
	}

	resp, err = c.call(ctx, op, http.MethodGet, "/exec/"+url.PathEscape(created.ID)+"/json", nil, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var info struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return 0, c.readError(ctx, op, err)
	}
	if info.Running {
		return 0, &runtime.Error{Op: op, Message: "the exec is still running after its output ended"}
	}
	return info.ExitCode, nil
}

// readTracker remembers the last error its reader returned, so a failed
// write can be told apart from a failed read.
type readTracker struct {
	r   io.Reader
	err error
}

func (t *readTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil {
		t.err = err
	}
	return n, err
}

// ipAddress returns the container's address. The daemon's legacy top-level
// field comes first; otherwise the first network by name that has an address.
func ipAddress(legacy string, networks map[string]struct {
	IPAddress string `json:"IPAddress"`
}) string {
	if legacy != "" {
		return legacy
	}
	names := make([]string, 0, len(networks))
	for n := range networks {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if ip := networks[n].IPAddress; ip != "" {
			return ip
		}
	}
	return ""
}

// Request bodies for POST /containers/create. Field names are the engine's.
type createRequest struct {
	Image        string              `json:"Image"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Labels       map[string]string   `json:"Labels"`
	Tty          bool                `json:"Tty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   hostConfig          `json:"HostConfig"`
}

// hostConfig is the part of HostConfig hivemind sets. The resource fields are
// from the API's Resources definition (v1.44):
//   - NanoCpus: "CPU quota in units of 10^-9 CPUs."
//   - Memory: "Memory limit in bytes."
//   - MemorySwap: "Total memory limit (memory + swap). Set as -1 to enable
//     unlimited swap."
//
// Zero leaves a field unset, which is no limit. MemorySwap is set equal to
// Memory: left unset, Docker lets the container use as much swap as its
// memory limit on a host with swap, so the limit would be twice what the spec
// says ("If --memory-swap is unset, and --memory is set, the container can
// use as much swap as the --memory setting"; set to the same value, "the
// container doesn't have access to swap", from the Docker docs' resource
// constraints page). On a kernel without swap accounting Docker ignores
// MemorySwap and warns; the memory limit still holds.
type hostConfig struct {
	PortBindings  map[string][]portBinding `json:"PortBindings,omitempty"`
	Mounts        []mount                  `json:"Mounts,omitempty"`
	RestartPolicy restartPolicy            `json:"RestartPolicy"`
	NanoCpus      int64                    `json:"NanoCpus,omitempty"`
	Memory        int64                    `json:"Memory,omitempty"`
	MemorySwap    int64                    `json:"MemorySwap,omitempty"`
	LogConfig     *logConfig               `json:"LogConfig,omitempty"`
}

// logConfig is HostConfig.LogConfig: the log driver and its options.
type logConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config,omitempty"`
}

// nanoCPUs converts a CPU limit in CPUs to the engine's units, rounded to the
// nearest whole unit.
func nanoCPUs(cpu float64) int64 {
	return int64(math.Round(cpu * 1e9))
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type mount struct {
	Type          string         `json:"Type"`
	Source        string         `json:"Source"`
	Target        string         `json:"Target"`
	VolumeOptions *volumeOptions `json:"VolumeOptions,omitempty"`
}

// volumeOptions is the Mount's "optional configuration for the volume type".
// Labels is "user-defined key/value metadata", applied when the mount creates
// the volume.
type volumeOptions struct {
	Labels map[string]string `json:"Labels,omitempty"`
}

type execCreateRequest struct {
	Cmd          []string `json:"Cmd"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
}

type execStartRequest struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

type restartPolicy struct {
	Name string `json:"Name"`
}
