package nodeapi

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/ao/hivemind/internal/pki"
)

// A cluster of several servers (docs/ha.md).

// JoinServerPath is the join listener's route for a server's join, beside
// JoinPath for a worker's.
const JoinServerPath = "/join-server"

// ServersHeader is on every node API reply of a cluster of several
// servers: the node listener of each server, host:port, comma separated, in
// the order they were added. A worker keeps the list in node.json and moves
// to the next server when the one it uses does not answer, or answers that
// it has no leader. A cluster of one sends none.
const ServersHeader = "Hivemind-Servers"

// NoLeaderHeader marks a 503 that says no server leads, or that the server
// asked no longer does. It is ha.HeaderNoLeader.
const NoLeaderHeader = "Hivemind-No-Leader"

// MaxServers bounds the servers a list holds.
const MaxServers = 7

// FormatServers is the ServersHeader value of a list.
func FormatServers(addrs []string) string { return strings.Join(addrs, ",") }

// ParseServers reads a ServersHeader value. A value with an address that
// does not validate, or with more than MaxServers, is nil.
func ParseServers(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	if len(parts) > MaxServers {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if ValidateServerAddress(p) != nil {
			return nil
		}
		out = append(out, p)
	}
	return out
}

// JoinServerRequest is the body of POST /join-server: the server join
// token, and where the joining server will be: the address it advertises,
// which its certificate will name, and its node listener's port. Its
// server port is that port plus two.
type JoinServerRequest struct {
	Token     string `json:"token"`
	Advertise string `json:"advertise"`
	NodePort  int    `json:"node_port"`
	// Version is the joining server's hivemind version, which must be the
	// leader's.
	Version string `json:"version"`
}

// ServerInfo is one server of the cluster, as a join reply names it.
type ServerInfo struct {
	ID        string `json:"id"`
	Advertise string `json:"advertise"`
	NodePort  int    `json:"node_port"`
	RaftAddr  string `json:"raft_addr"`
}

// NodeAddr is the host:port of the server's node listener.
func (s ServerInfo) NodeAddr() string {
	return net.JoinHostPort(s.Advertise, strconv.Itoa(s.NodePort))
}

// JoinServerReply is the reply to POST /join-server: the new server's node
// ID, the cluster CA and its private key, and the servers, the new one
// included. The joining machine pins the CA by the token's fingerprint
// before it sends the token, and checks the reply against it.
type JoinServerReply struct {
	NodeID   string       `json:"node_id"`
	CAPEM    string       `json:"ca_pem"`
	CAKeyPEM string       `json:"ca_key_pem"`
	Servers  []ServerInfo `json:"servers"`
}

// ValidateAdvertise checks the address a joining server advertises: an IP
// address or a DNS name, as a certificate can name.
func ValidateAdvertise(a string) error {
	if a == "" || len(a) > 253 {
		return fmt.Errorf("%w: the advertise address must be 1 to 253 characters", ErrInvalid)
	}
	if ip := net.ParseIP(a); ip != nil {
		if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
			return fmt.Errorf("%w: the advertise address %s cannot be reached from the other servers", ErrInvalid, a)
		}
		return nil
	}
	for _, label := range strings.Split(a, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("%w: the advertise address %q is not an IP address or a DNS name", ErrInvalid, a)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("%w: the advertise address %q is not an IP address or a DNS name", ErrInvalid, a)
			}
		}
	}
	return nil
}

// JoinServer presents a server join token to the join listener at addr,
// host:port of a server's join port, pinning the CA the token names, and
// checks the reply: its CA must be that CA, its key the CA's, and its
// servers must include the new one.
func JoinServer(ctx context.Context, addr string, req JoinServerRequest) (JoinServerReply, error) {
	tok, err := pki.ParseJoinToken(req.Token)
	if err != nil {
		return JoinServerReply{}, errors.New("the join token is malformed")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return JoinServerReply{}, fmt.Errorf("join address %q must be host:port: %w", addr, err)
	}
	cfg, err := pki.JoinClientTLSConfig(tok.Fingerprint, host)
	if err != nil {
		return JoinServerReply{}, err
	}
	hc := &http.Client{Timeout: clientTimeout * 2, Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}}
	b, err := json.Marshal(req)
	if err != nil {
		return JoinServerReply{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+JoinServerPath, bytes.NewReader(b))
	if err != nil {
		return JoinServerReply{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(hr)
	if err != nil {
		return JoinServerReply{}, clockHint(fmt.Errorf("joining %s: %w", addr, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return JoinServerReply{}, fmt.Errorf("the server at %s takes no server joins: it is older than 2.3.0", addr)
	}
	if resp.StatusCode != http.StatusOK {
		return JoinServerReply{}, statusError(resp)
	}
	var reply JoinServerReply
	if err := Decode(resp.Body, MaxJoinBytes*4, &reply); err != nil {
		return JoinServerReply{}, fmt.Errorf("reading the join reply: %w", err)
	}
	if err := checkJoinServerReply(reply, tok.Fingerprint, req); err != nil {
		return JoinServerReply{}, err
	}
	return reply, nil
}

func checkJoinServerReply(reply JoinServerReply, fp [32]byte, req JoinServerRequest) error {
	if err := pki.ValidateNodeID(reply.NodeID); err != nil {
		return fmt.Errorf("join reply: %w", err)
	}
	block, _ := pem.Decode([]byte(reply.CAPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("join reply has no CA certificate")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("join reply CA: %w", err)
	}
	if !caCert.IsCA || pki.Fingerprint(caCert) != fp {
		return errors.New("join reply CA does not match the token's fingerprint")
	}
	if _, err := pki.ParseCAPair([]byte(reply.CAPEM), []byte(reply.CAKeyPEM)); err != nil {
		return fmt.Errorf("join reply CA key: %w", err)
	}
	found := false
	for _, s := range reply.Servers {
		if s.ID == reply.NodeID && s.Advertise == req.Advertise && s.NodePort == req.NodePort {
			found = true
		}
		if err := pki.ValidateNodeID(s.ID); err != nil || ValidateServerAddress(s.NodeAddr()) != nil || ValidateServerAddress(s.RaftAddr) != nil {
			return fmt.Errorf("join reply names a server that does not validate: %+v", s)
		}
	}
	if !found {
		return errors.New("join reply does not list the new server")
	}
	return nil
}
