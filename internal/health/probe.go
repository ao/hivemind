// Package health runs a replica's health check and decides when the replica is
// unhealthy. The probes only connect to the container: an HTTP or TCP probe
// dials the container's address on its Docker network, and an exec probe runs
// a command inside the container. Nothing is sent to hosts outside it.
package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// Target is the container a probe checks.
type Target struct {
	// ContainerID is the engine ID, used by exec probes.
	ContainerID string
	// IP is the container's address on its Docker network. HTTP and TCP
	// probes dial IP:port, where port is the container port in the spec.
	IP string
}

// Execer runs a command in a container. The Docker runtime implements it.
type Execer interface {
	Exec(ctx context.Context, id string, cmd []string) (int, error)
}

// Check runs the probe in h once against t, and returns nil when it passes.
// The probe gives up after h.Timeout. The error never contains a response
// body.
func Check(ctx context.Context, h spec.Health, t Target, rt Execer) error {
	timeout := h.Timeout.D()
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	switch {
	case h.HTTP != nil:
		return checkHTTP(ctx, t.IP, h.HTTP.Port, h.HTTP.Path, timeout)
	case h.TCP != nil:
		return checkTCP(ctx, t.IP, h.TCP.Port, timeout)
	case len(h.Exec) > 0:
		return checkExec(ctx, rt, t.ContainerID, h.Exec, timeout)
	default:
		return errors.New("health: no probe is set")
	}
}

// checkHTTP passes when GET http://ip:port/path answers with 2xx or 3xx. A
// redirect is not followed, so a 3xx passes on its own.
func checkHTTP(ctx context.Context, ip string, port int, path string, timeout time.Duration) error {
	if ip == "" {
		return errors.New("http: the container has no address")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+net.JoinHostPort(ip, strconv.Itoa(port))+path, nil)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http GET %s: %w", path, transportErr(ctx, err))
	}
	defer resp.Body.Close()
	// The body is not read. Only the status counts.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		return fmt.Errorf("http GET %s: status %d", path, resp.StatusCode)
	}
	return nil
}

// checkTCP passes when a connection to ip:port opens within timeout.
func checkTCP(ctx context.Context, ip string, port int, timeout time.Duration) error {
	if ip == "" {
		return errors.New("tcp: the container has no address")
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("tcp port %d: %w", port, transportErr(ctx, err))
	}
	return conn.Close()
}

// checkExec passes when the command exits with 0 within timeout.
func checkExec(ctx context.Context, rt Execer, id string, cmd []string, timeout time.Duration) error {
	if rt == nil {
		return errors.New("exec: no runtime to run the command with")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, err := rt.Exec(ctx, id, cmd)
	if err != nil {
		return fmt.Errorf("exec: %w", transportErr(ctx, err))
	}
	if code != 0 {
		return fmt.Errorf("exec: command exited with %d", code)
	}
	return nil
}

// transportErr says "timed out" for a probe that ran out of time, rather than
// the lower-level text.
func transportErr(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return errors.New("timed out")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("timed out")
	}
	return err
}
