package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// A rotation of the cluster CA (docs/ca-rotation.md) has a time in which
// the nodes trust two CAs. Trust is such a set, and the TLS configs below
// read it at each handshake.

// MaxTrustedCAs is the most CAs a trust set holds: the two of a rotation.
const MaxTrustedCAs = 2

// Errors of the trust set and of cross-certificates.
var (
	ErrNotCA          = errors.New("pki: not a CA certificate this cluster makes")
	ErrCrossMismatch  = errors.New("pki: the cross-certificate is not for this CA")
	ErrCrossUntrusted = errors.New("pki: the cross-certificate is not signed by a trusted CA")
	ErrUntrustedCA    = errors.New("pki: the certificate is from a CA this node does not trust")
)

// Trust is a set of CA certificates, at most MaxTrustedCAs, in order. A
// Trust is not changed once made; make a new one instead.
type Trust struct {
	cas  []*x509.Certificate
	pool *x509.CertPool
}

// NewTrust returns the set of cas, without duplicates, in the order given.
// It needs at least one, and each must be a CA (CheckCACert).
func NewTrust(cas ...*x509.Certificate) (*Trust, error) {
	t := &Trust{pool: x509.NewCertPool()}
	for _, c := range cas {
		if c == nil {
			continue
		}
		if err := CheckCACert(c); err != nil {
			return nil, err
		}
		if t.Contains(Fingerprint(c)) {
			continue
		}
		t.cas = append(t.cas, c)
		t.pool.AddCert(c)
	}
	if len(t.cas) == 0 {
		return nil, errors.New("pki: a trust set needs a CA")
	}
	if len(t.cas) > MaxTrustedCAs {
		return nil, fmt.Errorf("pki: a trust set holds at most %d CAs", MaxTrustedCAs)
	}
	return t, nil
}

// MustTrust is NewTrust for CAs already checked, such as a CA this process
// made or loaded. It panics on an error.
func MustTrust(cas ...*x509.Certificate) *Trust {
	t, err := NewTrust(cas...)
	if err != nil {
		panic(err)
	}
	return t
}

// CAs returns the certificates, in order.
func (t *Trust) CAs() []*x509.Certificate { return slices.Clone(t.cas) }

// Pool is the set as a certificate pool. The caller must not change it.
func (t *Trust) Pool() *x509.CertPool { return t.pool }

// Contains reports whether the CA with fingerprint fp is in the set.
func (t *Trust) Contains(fp [32]byte) bool {
	return t.Find(fp) != nil
}

// Find returns the CA with fingerprint fp, or nil.
func (t *Trust) Find(fp [32]byte) *x509.Certificate {
	for _, c := range t.cas {
		if Fingerprint(c) == fp {
			return c
		}
	}
	return nil
}

// Fingerprints returns the fingerprint of each CA, in order.
func (t *Trust) Fingerprints() [][32]byte {
	out := make([][32]byte, len(t.cas))
	for i, c := range t.cas {
		out[i] = Fingerprint(c)
	}
	return out
}

// Same reports whether t and u hold the same CAs, in any order.
func (t *Trust) Same(u *Trust) bool {
	if len(t.cas) != len(u.cas) {
		return false
	}
	for _, c := range t.cas {
		if !u.Contains(Fingerprint(c)) {
			return false
		}
	}
	return true
}

// Issuer returns the CA in the set that signed cert, or nil.
func (t *Trust) Issuer(cert *x509.Certificate) *x509.Certificate {
	for _, c := range t.cas {
		if bytes.Equal(cert.RawIssuer, c.RawSubject) && cert.CheckSignatureFrom(c) == nil {
			return c
		}
	}
	return nil
}

// PEM is the set as PEM certificates, one after another, in order.
func (t *Trust) PEM() []byte {
	var b bytes.Buffer
	for _, c := range t.cas {
		_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	return b.Bytes()
}

// ParseTrustPEM reads a trust set written by PEM: one or more PEM
// certificates, each a CA, and nothing else.
func ParseTrustPEM(b []byte) (*Trust, error) {
	var cas []*x509.Certificate
	rest := b
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("pki: a %s block where a certificate should be", blk.Type)
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		cas = append(cas, c)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("pki: data that is not PEM after the certificates")
	}
	if len(cas) == 0 {
		return nil, errors.New("pki: no PEM certificate")
	}
	return NewTrust(cas...)
}

// CheckCACert returns ErrNotCA unless cert is a CA as this cluster makes
// them: a self-signed ECDSA P-256 certificate that may sign certificates.
func CheckCACert(cert *x509.Certificate) error {
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	switch {
	case !cert.BasicConstraintsValid || !cert.IsCA:
		return fmt.Errorf("%w: it is not a CA", ErrNotCA)
	case !ok || pub.Curve != elliptic.P256():
		return fmt.Errorf("%w: its key is not ECDSA P-256", ErrNotCA)
	case cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0:
		return fmt.Errorf("%w: it may not sign certificates", ErrNotCA)
	case !bytes.Equal(cert.RawIssuer, cert.RawSubject) || cert.CheckSignatureFrom(cert) != nil:
		return fmt.Errorf("%w: it is not self-signed", ErrNotCA)
	}
	return nil
}

// FingerprintHex is a fingerprint as 64 lowercase hex digits, as join
// tokens carry it.
func FingerprintHex(fp [32]byte) string { return hex.EncodeToString(fp[:]) }

// ParseFingerprintHex reads FingerprintHex's form.
func ParseFingerprintHex(s string) ([32]byte, error) {
	var fp [32]byte
	if len(s) != 64 || strings.ToLower(s) != s {
		return fp, errors.New("pki: a fingerprint is 64 lowercase hex digits")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return fp, errors.New("pki: a fingerprint is 64 lowercase hex digits")
	}
	copy(fp[:], b)
	return fp, nil
}

// CrossSign returns a DER cross-certificate for other: other's name and
// public key, signed with ca's key. A node that trusts ca can check with it
// that ca's key vouches for other (VerifyCross). It is a CA certificate
// whose path length is 0, valid until the earlier of the two CAs ends; it
// is checked by its signature only, and never put in a chain: ca's own
// path length of 0 would refuse it there.
func (ca *CA) CrossSign(other *x509.Certificate) ([]byte, error) {
	if err := CheckCACert(other); err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	notAfter := other.NotAfter
	if ca.Cert.NotAfter.Before(notAfter) {
		notAfter = ca.Cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               other.Subject,
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		SubjectKeyId:          other.SubjectKeyId,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, other.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("pki: cross-sign: %w", err)
	}
	return der, nil
}

// VerifyCross checks that crossDER is a cross-certificate for caCert, as
// CrossSign makes, signed by a CA in trusted, and valid at now. It returns
// that CA.
func VerifyCross(caCert *x509.Certificate, crossDER []byte, trusted *Trust, now time.Time) (*x509.Certificate, error) {
	cross, err := x509.ParseCertificate(crossDER)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCrossMismatch, err)
	}
	if !cross.IsCA || !bytes.Equal(cross.RawSubject, caCert.RawSubject) ||
		!bytes.Equal(cross.RawSubjectPublicKeyInfo, caCert.RawSubjectPublicKeyInfo) {
		return nil, ErrCrossMismatch
	}
	if err := CheckValidAt(cross, now); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCrossMismatch, err)
	}
	signer := trusted.Issuer(cross)
	if signer == nil {
		return nil, ErrCrossUntrusted
	}
	return signer, nil
}

// TrustHolder holds a trust set and swaps it without a restart: a server
// TLS config built on it reads it at each handshake.
type TrustHolder struct {
	p atomic.Pointer[Trust]
}

// NewTrustHolder returns a holder with t.
func NewTrustHolder(t *Trust) *TrustHolder {
	h := &TrustHolder{}
	h.Store(t)
	return h
}

// Store makes t the set for new handshakes.
func (h *TrustHolder) Store(t *Trust) { h.p.Store(t) }

// Load returns the current set.
func (h *TrustHolder) Load() *Trust { return h.p.Load() }

// ServerTLSConfigTrust is ServerTLSConfigFrom with the client CAs read from
// trust at each handshake, so a rotation changes which client certificates
// the listener accepts without a restart. A connection opened before a
// change keeps what it was checked against; VerifiedRoot lets a handler
// check that against the set of the moment.
func ServerTLSConfigTrust(trust *TrustHolder, h *CertHolder) *tls.Config {
	conf := func(pool *x509.CertPool) *tls.Config {
		return &tls.Config{
			GetCertificate:        h.GetCertificate,
			ClientAuth:            tls.RequireAndVerifyClientCert,
			ClientCAs:             pool,
			MinVersion:            tls.VersionTLS13,
			VerifyPeerCertificate: requireNodeID,
		}
	}
	base := conf(trust.Load().Pool())
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return conf(trust.Load().Pool()), nil
	}
	return base
}

// ClientTLSConfigTrust is ClientTLSConfigFrom with every CA in trust as a
// root. A node changes its trust set by building a new config.
func ClientTLSConfigTrust(trust *Trust, h *CertHolder, serverName string) (*tls.Config, error) {
	if serverName == "" {
		return nil, errors.New("pki: serverName is required")
	}
	return &tls.Config{
		GetClientCertificate:  h.GetClientCertificate,
		RootCAs:               trust.Pool(),
		ServerName:            serverName,
		MinVersion:            tls.VersionTLS13,
		VerifyPeerCertificate: requireNodeID,
	}, nil
}

// VerifiedRoot returns the CA at the end of the first verified chain of a
// completed handshake: the CA the peer's certificate was checked against.
// It is nil when the peer did not verify.
func VerifiedRoot(cs *tls.ConnectionState) *x509.Certificate {
	if cs == nil || len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return nil
	}
	ch := cs.VerifiedChains[0]
	return ch[len(ch)-1]
}
