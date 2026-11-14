package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// NodeURIScheme and nodeURIPrefix form the node-ID URI SAN: hivemind://node/<id>.
	NodeURIScheme = "hivemind"
	nodeURIPrefix = NodeURIScheme + "://node/"

	// NodeCertLifetime is the longest a node certificate may run.
	NodeCertLifetime = 365 * 24 * time.Hour
)

var (
	ErrInvalidNodeID    = errors.New("pki: invalid node ID")
	ErrNoNodeID         = errors.New("pki: certificate has no node ID")
	ErrMultipleNodeIDs  = errors.New("pki: certificate has more than one node ID")
	ErrNoPeerCert       = errors.New("pki: no verified peer certificate")
	ErrNodeIDMismatch   = errors.New("pki: peer node ID does not match the expected node ID")
	ErrInvalidCSR       = errors.New("pki: invalid certificate signing request")
	ErrInvalidLifetime  = errors.New("pki: certificate lifetime out of range")
	ErrInvalidAdvertise = errors.New("pki: advertise address is not an IP or DNS name")
)

// nodeIDPattern allows lowercase DNS-label style IDs of 1 to 63 characters.
var nodeIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateNodeID reports whether id is a valid node ID.
func ValidateNodeID(id string) error {
	if !nodeIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidNodeID, id)
	}
	return nil
}

// NodeURI returns the node-ID URI for id, e.g. hivemind://node/n1.
func NodeURI(id string) (string, error) {
	if err := ValidateNodeID(id); err != nil {
		return "", err
	}
	return nodeURIPrefix + id, nil
}

// ParseNodeURI parses a node-ID URI and returns the ID. It accepts only the
// exact form NodeURI produces, so ParseNodeURI(s) succeeding means NodeURI(id)
// returns s.
func ParseNodeURI(s string) (string, error) {
	if !strings.HasPrefix(s, nodeURIPrefix) {
		return "", fmt.Errorf("%w: %q", ErrInvalidNodeID, s)
	}
	id := s[len(nodeURIPrefix):]
	if err := ValidateNodeID(id); err != nil {
		return "", err
	}
	return id, nil
}

// NodeIDFromCert returns the single node ID in the certificate's URI SANs. It
// fails if there is none, more than one, or a malformed one.
func NodeIDFromCert(cert *x509.Certificate) (string, error) {
	var ids []string
	for _, u := range cert.URIs {
		s := u.String()
		if !strings.HasPrefix(s, nodeURIPrefix) {
			continue
		}
		id, err := ParseNodeURI(s)
		if err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	switch len(ids) {
	case 0:
		return "", ErrNoNodeID
	case 1:
		return ids[0], nil
	default:
		return "", ErrMultipleNodeIDs
	}
}

// NewNodeKey makes a new ECDSA P-256 key for a node or server.
func NewNodeKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// NewCSR returns a DER certificate signing request for key. The request
// carries no names: IssueNodeCert sets the subject and SANs itself.
func NewCSR(key *ecdsa.PrivateKey) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
}

// NodeCertOptions controls one issuance.
type NodeCertOptions struct {
	// NodeID goes into the URI SAN and the subject CN.
	NodeID string
	// Advertise is the address peers dial, an IP literal or a DNS name. Set it
	// for the server certificate. Leave it empty for an agent-only node.
	Advertise string
	// Lifetime is the certificate's validity. Zero means NodeCertLifetime.
	// Values above NodeCertLifetime are refused.
	Lifetime time.Duration
	// Now is the issue time. Zero means the current time. Tests use it.
	Now time.Time
}

// IssueNodeCert signs the public key in csrDER and returns a DER certificate
// for the node. The certificate is good for client and server auth. Only the
// public key is taken from the CSR, after its self-signature is checked. Names
// and extensions the CSR asks for are ignored, so a node can't pick its own ID.
func (ca *CA) IssueNodeCert(csrDER []byte, o NodeCertOptions) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCSR, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrInvalidCSR, err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: key must be ECDSA P-256", ErrInvalidCSR)
	}
	if err := ValidateNodeID(o.NodeID); err != nil {
		return nil, err
	}
	nodeURI, err := NodeURI(o.NodeID)
	if err != nil {
		return nil, err
	}
	nodeURL, err := url.Parse(nodeURI)
	if err != nil {
		return nil, err
	}

	lifetime := o.Lifetime
	if lifetime == 0 {
		lifetime = NodeCertLifetime
	}
	if lifetime < 0 || lifetime > NodeCertLifetime {
		return nil, fmt.Errorf("%w: %s", ErrInvalidLifetime, lifetime)
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	notAfter := now.Add(lifetime)
	if notAfter.After(ca.Cert.NotAfter) {
		return nil, fmt.Errorf("%w: certificate would outlive the CA", ErrInvalidLifetime)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: o.NodeID},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{nodeURL},
	}
	if o.Advertise != "" {
		if ip := net.ParseIP(o.Advertise); ip != nil {
			tmpl.IPAddresses = []net.IP{ip}
		} else if validHostname(o.Advertise) {
			tmpl.DNSNames = []string{o.Advertise}
		} else {
			return nil, fmt.Errorf("%w: %q", ErrInvalidAdvertise, o.Advertise)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("pki: sign node certificate: %w", err)
	}
	return der, nil
}

// validHostname checks the DNS name shape: dot-separated labels of letters,
// digits and inner hyphens, at most 253 characters in total.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			if !isAlnum && c != '-' {
				return false
			}
		}
	}
	return true
}
