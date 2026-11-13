package nodeapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// clientTimeout bounds one request to the server.
const clientTimeout = 30 * time.Second

// ErrNotRegistered means the server has no record of this node. It happens
// when the server's database was replaced. A worker registers again.
var ErrNotRegistered = errors.New("this node is not registered with the server")

// Client calls the node routes of one server with a node certificate. It is
// safe for concurrent use. It implements the agent's Source: assignments, status
// reports and the status a node last reported.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for the server at base, such as
// https://10.0.0.5:4484. tlsCfg presents the node's certificate and trusts the
// cluster CA (see pki.ClientTLSConfig).
func NewClient(base string, tlsCfg *tls.Config) (*Client, error) {
	if !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("server URL %q must start with https://", base)
	}
	tr := &http.Transport{TLSClientConfig: tlsCfg}
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Transport: tr, Timeout: clientTimeout}}, nil
}

// Register tells the server the node's name and capacity.
func (c *Client) Register(ctx context.Context, nodeID string, g Register) error {
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, RegisterPath(nodeID), b, nil)
}

// AssignmentsForNode returns the replicas placed on the node. It implements
// agent.Source.
func (c *Client) AssignmentsForNode(ctx context.Context, nodeID string) ([]store.Assignment, error) {
	var list AssignmentList
	if err := c.do(ctx, http.MethodGet, AssignmentsPath(nodeID), nil, &list); err != nil {
		return nil, err
	}
	if err := list.Validate(); err != nil {
		return nil, fmt.Errorf("server sent assignments that do not validate: %w", err)
	}
	out := make([]store.Assignment, 0, len(list.Assignments))
	for _, a := range list.Assignments {
		out = append(out, a.ToStore())
	}
	return out, nil
}

// ReportStatus replaces what the node reported with st, and renews its lease.
// It implements agent.Source.
func (c *Client) ReportStatus(ctx context.Context, nodeID string, st []store.Status) error {
	b, err := json.Marshal(StatusFromStore(st))
	if err != nil {
		return err
	}
	if len(b) > MaxStatusBytes {
		return fmt.Errorf("status report is %d bytes, more than the %d the server takes", len(b), MaxStatusBytes)
	}
	return c.do(ctx, http.MethodPost, StatusPath(nodeID), b, nil)
}

// StatusForNode returns what the node last reported. It implements the
// agent's optional status reader, which seeds restart counts at start.
func (c *Client) StatusForNode(ctx context.Context, nodeID string) ([]store.Status, error) {
	var rep StatusReport
	if err := c.do(ctx, http.MethodGet, StatusPath(nodeID), nil, &rep); err != nil {
		return nil, err
	}
	if err := rep.Validate(); err != nil {
		return nil, fmt.Errorf("server sent a status report that does not validate: %w", err)
	}
	sts := rep.ToStore()
	for i := range sts {
		sts[i].NodeID = nodeID
	}
	return sts, nil
}

// do makes one request. A 2xx reply is decoded into out when out is set. Any
// other status becomes an error that carries the server's message.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling the server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxStatusBytes))
		return nil
	}
	if err := Decode(resp.Body, MaxAssignmentsBytes, out); err != nil {
		return fmt.Errorf("reading the server's reply: %w", err)
	}
	return nil
}

// statusError turns a refusal into an error. A 404 on a node route means the
// node is not registered; the server's own text says why.
func statusError(resp *http.Response) error {
	var e ErrorBody
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	msg := resp.Status
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotRegistered, msg)
	}
	return fmt.Errorf("the server refused the request (%s): %s", resp.Status, msg)
}

// Join presents token and csr to the join listener at addr, which is host:port
// of the server's join port. The client trusts only the CA whose fingerprint the
// token carries. The reply is checked before Join returns: its CA must have
// that fingerprint, and its certificate must chain to that CA and name the
// reply's node ID.
func Join(ctx context.Context, addr, token string, csr []byte) (JoinReply, error) {
	tok, err := pki.ParseJoinToken(token)
	if err != nil {
		return JoinReply{}, errors.New("the join token is malformed")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return JoinReply{}, fmt.Errorf("join address %q must be host:port: %w", addr, err)
	}
	cfg, err := pki.JoinClientTLSConfig(tok.Fingerprint, host)
	if err != nil {
		return JoinReply{}, err
	}
	hc := &http.Client{Timeout: clientTimeout, Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}}

	b, err := json.Marshal(JoinRequest{Token: token, CSR: csr})
	if err != nil {
		return JoinReply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+JoinPath, bytes.NewReader(b))
	if err != nil {
		return JoinReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return JoinReply{}, fmt.Errorf("joining %s: %w", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return JoinReply{}, statusError(resp)
	}
	var reply JoinReply
	if err := Decode(resp.Body, MaxJoinBytes, &reply); err != nil {
		return JoinReply{}, fmt.Errorf("reading the join reply: %w", err)
	}
	if err := checkJoinReply(reply, tok.Fingerprint); err != nil {
		return JoinReply{}, err
	}
	return reply, nil
}

// checkJoinReply verifies what the server sent back against the token's CA.
func checkJoinReply(reply JoinReply, fp [32]byte) error {
	capem, _ := pem.Decode([]byte(reply.CAPEM))
	if capem == nil || capem.Type != "CERTIFICATE" {
		return errors.New("join reply has no CA certificate")
	}
	caCert, err := x509.ParseCertificate(capem.Bytes)
	if err != nil {
		return fmt.Errorf("join reply CA: %w", err)
	}
	if !caCert.IsCA || pki.Fingerprint(caCert) != fp {
		return errors.New("join reply CA does not match the token's fingerprint")
	}
	certpem, _ := pem.Decode([]byte(reply.CertPEM))
	if certpem == nil || certpem.Type != "CERTIFICATE" {
		return errors.New("join reply has no node certificate")
	}
	leaf, err := x509.ParseCertificate(certpem.Bytes)
	if err != nil {
		return fmt.Errorf("join reply certificate: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("join reply certificate does not chain to the CA: %w", err)
	}
	id, err := pki.NodeIDFromCert(leaf)
	if err != nil {
		return err
	}
	if id != reply.NodeID {
		return fmt.Errorf("join reply names node %q, but its certificate names %q", reply.NodeID, id)
	}
	return nil
}
