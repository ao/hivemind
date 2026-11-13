package pki

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
)

// ErrPinnedCAMissing is returned when a joining node's server does not send
// the CA the join token names.
var ErrPinnedCAMissing = errors.New("pki: server did not present a CA")

// NewTLSCertificate pairs a DER node certificate with its key and appends the
// CA certificate to the chain. A joining node receives the CA from the server
// this way, so it can pin the CA by fingerprint.
func NewTLSCertificate(certDER []byte, key *ecdsa.PrivateKey, ca *CA) (tls.Certificate, error) {
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{certDER, ca.Cert.Raw},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// ServerTLSConfig is the config for the cluster listener. It requires a client
// certificate that chains to ca and carries a node ID. TLS 1.3 only.
func ServerTLSConfig(ca *CA, cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		ClientAuth:            tls.RequireAndVerifyClientCert,
		ClientCAs:             caPool(ca),
		MinVersion:            tls.VersionTLS13,
		VerifyPeerCertificate: requireNodeID,
	}
}

// ClientTLSConfig is the config for a node calling the cluster port. It
// presents the node's certificate and trusts only ca. serverName is checked
// against the server certificate's DNS or IP SANs.
func ClientTLSConfig(ca *CA, cert tls.Certificate, serverName string) (*tls.Config, error) {
	if serverName == "" {
		return nil, errors.New("pki: serverName is required")
	}
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		RootCAs:               caPool(ca),
		ServerName:            serverName,
		MinVersion:            tls.VersionTLS13,
		VerifyPeerCertificate: requireNodeID,
	}, nil
}

// JoinServerTLSConfig is the config for the join listener. It presents the
// server certificate and asks for no client certificate, because the joining
// node has none yet. Serve only the join route on this config, never the
// mTLS routes.
func JoinServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
}

// JoinClientTLSConfig is the config for a joining node. It has no CA yet, so it
// can't verify the server against one. It pins the CA by fingerprint, which
// comes from the join token. The server must present that CA in its chain, and
// the server certificate must chain to it for serverName. Any other chain is
// rejected with ErrFingerprintMismatch or ErrPinnedCAMissing.
func JoinClientTLSConfig(fingerprint [32]byte, serverName string) (*tls.Config, error) {
	if serverName == "" {
		return nil, errors.New("pki: serverName is required")
	}
	return &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
		// Standard chain verification would use system roots, which don't
		// know this CA. VerifyConnection does the pinned check instead, and
		// it runs whether or not InsecureSkipVerify is set.
		InsecureSkipVerify: true, //nolint:gosec // replaced by the pinned check below
		VerifyConnection:   pinCA(fingerprint, serverName),
	}, nil
}

func pinCA(fingerprint [32]byte, serverName string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) < 2 {
			return ErrPinnedCAMissing
		}
		var pinned *x509.Certificate
		for _, c := range cs.PeerCertificates[1:] {
			if c.IsCA && Fingerprint(c) == fingerprint {
				pinned = c
				break
			}
		}
		if pinned == nil {
			return fmt.Errorf("%w: server chain does not include the CA named by the join token", ErrFingerprintMismatch)
		}
		roots := x509.NewCertPool()
		roots.AddCert(pinned)
		inter := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			inter.AddCert(c)
		}
		leaf := cs.PeerCertificates[0]
		if _, err := leaf.Verify(x509.VerifyOptions{
			DNSName:       serverName,
			Roots:         roots,
			Intermediates: inter,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			return fmt.Errorf("pki: server certificate does not chain to the pinned CA: %w", err)
		}
		if _, err := NodeIDFromCert(leaf); err != nil {
			return err
		}
		return nil
	}
}

// PeerNodeID returns the node ID from the verified peer certificate of a
// completed handshake. It fails if the peer did not verify.
func PeerNodeID(cs tls.ConnectionState) (string, error) {
	if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 {
		return "", ErrNoPeerCert
	}
	return NodeIDFromCert(cs.PeerCertificates[0])
}

// RequestNodeID returns the node ID of the verified peer of an HTTP request
// that arrived over TLS.
func RequestNodeID(r *http.Request) (string, error) {
	if r.TLS == nil {
		return "", ErrNoPeerCert
	}
	return PeerNodeID(*r.TLS)
}

// CheckPeerNodeID returns nil only if the verified peer's node ID is expected.
// A handler should call CheckRequestNodeID with the node ID the request names.
func CheckPeerNodeID(cs tls.ConnectionState, expected string) error {
	if err := ValidateNodeID(expected); err != nil {
		return err
	}
	got, err := PeerNodeID(cs)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("%w: peer is %q, request names %q", ErrNodeIDMismatch, got, expected)
	}
	return nil
}

// CheckRequestNodeID is CheckPeerNodeID for an HTTP request.
func CheckRequestNodeID(r *http.Request, expected string) error {
	if r.TLS == nil {
		return ErrNoPeerCert
	}
	return CheckPeerNodeID(*r.TLS, expected)
}

// requireNodeID is a VerifyPeerCertificate hook. It rejects a chain whose
// leaf carries no node ID, even when the chain itself is valid.
func requireNodeID(_ [][]byte, verified [][]*x509.Certificate) error {
	if len(verified) == 0 || len(verified[0]) == 0 {
		return ErrNoPeerCert
	}
	_, err := NodeIDFromCert(verified[0][0])
	return err
}

func caPool(ca *CA) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return pool
}
