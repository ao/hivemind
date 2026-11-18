package nodeapi

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ao/hivemind/internal/pki"
)

// The CA set of a rotation of the cluster CA (docs/ca-rotation.md).
//
// Every node API reply carries CAHeader, a digest of the CA set the server
// holds now. A worker that sees a digest it has not applied reads the set
// from CAPath, checks it, and changes what it trusts. Every request from a
// worker of this build carries CATrustHeader, the fingerprints of the CAs
// it trusts, from which the server learns which nodes trust a new CA. A
// server older than rotation sends no CAHeader, and a worker older than it
// sends no CATrustHeader.
const (
	CAHeader      = "Hivemind-CA"
	CATrustHeader = "Hivemind-CA-Trust"
)

// MaxCABytes caps the CA set's reply: two CA certificates and a
// cross-certificate.
const MaxCABytes = 32 << 10

// maxCATrust is the most fingerprints a CATrustHeader carries.
const maxCATrust = 4

// Messages the node API sends, with 403, for a client certificate from a
// CA it does not accept now, and for a certificate from the CA a rotation
// leaves, for a node that has presented one from the CA it goes to.
const (
	CAUntrustedMessage  = "the client certificate is from a CA this cluster no longer trusts"
	CASupersededMessage = "this node has a certificate from the cluster's new CA, so one from the old CA is refused for it"
)

// ErrCAUntrusted and ErrCASuperseded are what the client returns for them.
var (
	ErrCAUntrusted  = errors.New(CAUntrustedMessage)
	ErrCASuperseded = errors.New(CASupersededMessage)
)

// CAPath is the route a node reads the CA set from.
func CAPath(id string) string { return Prefix + "/nodes/" + id + "/ca" }

// CA phases in a CABundle.
const (
	CAPhaseTrust = "trust"
	CAPhaseRenew = "renew"
)

// CABundle is the reply to GET ca: the CAs a node is to trust, and, in the
// renew phase of a rotation, the CA to renew under now.
type CABundle struct {
	// Phase is "" outside a rotation, CAPhaseTrust or CAPhaseRenew.
	Phase string `json:"phase,omitempty"`
	// Renew is the fingerprint, in hex, of the CA a node renews under now
	// when its certificate is from another one. Empty: renew when due.
	Renew string `json:"renew,omitempty"`
	// CAs are the CAs to trust, the one the rotation comes from first.
	CAs []BundleCA `json:"cas"`
}

// BundleCA is one CA of a CABundle.
type BundleCA struct {
	CertPEM string `json:"cert_pem"`
	// CrossPEM is this CA's name and key signed by the CA the rotation
	// comes from (pki.CrossSign), for a node that does not trust it yet.
	CrossPEM string `json:"cross_pem,omitempty"`
}

// ParsedCA is a BundleCA read and checked: its certificate a CA, its
// cross-certificate, if any, as DER.
type ParsedCA struct {
	Cert  *x509.Certificate
	Cross []byte
}

// Parse checks the bundle's form: a known phase, one or two CAs, each a CA
// certificate this cluster makes, at most one cross-certificate each, and
// a Renew that names one of them.
func (b CABundle) Parse() ([]ParsedCA, error) {
	if b.Phase != "" && b.Phase != CAPhaseTrust && b.Phase != CAPhaseRenew {
		return nil, fmt.Errorf("%w: CA phase %q", ErrInvalid, b.Phase)
	}
	if len(b.CAs) == 0 || len(b.CAs) > pki.MaxTrustedCAs {
		return nil, fmt.Errorf("%w: a CA set has 1 or %d CAs, not %d", ErrInvalid, pki.MaxTrustedCAs, len(b.CAs))
	}
	out := make([]ParsedCA, 0, len(b.CAs))
	seen := map[[32]byte]bool{}
	renewFound := b.Renew == ""
	for _, c := range b.CAs {
		cert, err := onePEM(c.CertPEM)
		if err != nil {
			return nil, fmt.Errorf("%w: CA certificate: %v", ErrInvalid, err)
		}
		ca, err := x509.ParseCertificate(cert)
		if err != nil {
			return nil, fmt.Errorf("%w: CA certificate: %v", ErrInvalid, err)
		}
		if err := pki.CheckCACert(ca); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		fp := pki.Fingerprint(ca)
		if seen[fp] {
			return nil, fmt.Errorf("%w: a CA twice in the set", ErrInvalid)
		}
		seen[fp] = true
		if pki.FingerprintHex(fp) == b.Renew {
			renewFound = true
		}
		p := ParsedCA{Cert: ca}
		if c.CrossPEM != "" {
			if p.Cross, err = onePEM(c.CrossPEM); err != nil {
				return nil, fmt.Errorf("%w: cross-certificate: %v", ErrInvalid, err)
			}
		}
		out = append(out, p)
	}
	if !renewFound {
		return nil, fmt.Errorf("%w: the CA to renew under is not in the set", ErrInvalid)
	}
	return out, nil
}

func onePEM(s string) ([]byte, error) {
	blk, rest := pem.Decode([]byte(s))
	if blk == nil || blk.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("not one PEM certificate")
	}
	return blk.Bytes, nil
}

// Digest is the CAHeader value for the bundle: 16 hex digits of a SHA-256
// over the phase, the CA to renew under, and each CA's fingerprint and
// whether it has a cross-certificate. A cross-certificate is signed anew
// at each start of the server, so its bytes are left out.
func (b CABundle) Digest() string {
	h := sha256.New()
	fmt.Fprintf(h, "phase=%s;renew=%s", b.Phase, b.Renew)
	for _, c := range b.CAs {
		der, err := onePEM(c.CertPEM)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(der)
		fmt.Fprintf(h, ";ca=%x,%t", sum, c.CrossPEM != "")
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// ValidCADigest reports whether v has Digest's form.
func ValidCADigest(v string) bool {
	if len(v) != 16 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil && strings.ToLower(v) == v
}

// FormatCATrust is the CATrustHeader value for the fingerprints.
func FormatCATrust(fps [][32]byte) string {
	parts := make([]string, len(fps))
	for i, fp := range fps {
		parts[i] = pki.FingerprintHex(fp)
	}
	return strings.Join(parts, ",")
}

// ParseCATrust reads a CATrustHeader value. sent is false when the header
// is absent, as from a build without rotation. A value that does not
// parse, or has more than a few fingerprints, gives no fingerprints but
// sent true: the node speaks the protocol, and trusts nothing the server
// can name.
func ParseCATrust(v string) (fps [][32]byte, sent bool) {
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ",")
	if len(parts) > maxCATrust {
		return nil, true
	}
	for _, p := range parts {
		fp, err := pki.ParseFingerprintHex(strings.TrimSpace(p))
		if err != nil {
			return nil, true
		}
		fps = append(fps, fp)
	}
	return fps, true
}

// CABundle reads the CA set from the server, with the CA the connection's
// server certificate was checked against. A server older than rotation
// lacks the route: ErrNoRoute.
func (c *Client) CABundle(ctx context.Context, nodeID string) (CABundle, *x509.Certificate, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+CAPath(nodeID), nil)
	if err != nil {
		return CABundle{}, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return CABundle{}, nil, &UnreachableError{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return CABundle{}, nil, statusError(resp)
	}
	root := pki.VerifiedRoot(resp.TLS)
	if root == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxCABytes))
		return CABundle{}, nil, errors.New("the CA set came over a connection whose server certificate was not checked")
	}
	var b CABundle
	if err := Decode(resp.Body, MaxCABytes, &b); err != nil {
		return CABundle{}, nil, fmt.Errorf("reading the server's CA set: %w", err)
	}
	return b, root, nil
}
