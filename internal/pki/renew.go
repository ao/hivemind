package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync/atomic"
	"time"
)

// CAWarnBefore is how long before the CA expires the server starts to warn.
// The CA cannot be rotated: when it expires, no node certificate verifies.
const CAWarnBefore = 365 * 24 * time.Hour

// minClippedLifetime is the shortest certificate IssueNodeCert issues when it
// shortens one to end with the CA.
const minClippedLifetime = time.Hour

// CertHolder holds a node's current certificate and swaps it without a
// restart. A tls.Config built on it reads the certificate at each handshake,
// so a new handshake uses the certificate stored last. Connections already
// open keep the certificate they were set up with. It is safe for concurrent
// use.
type CertHolder struct {
	p atomic.Pointer[tls.Certificate]
}

// NewCertHolder returns a holder with cert. cert.Leaf must be set, as
// NewTLSCertificate does.
func NewCertHolder(cert tls.Certificate) *CertHolder {
	h := &CertHolder{}
	h.Store(cert)
	return h
}

// Store makes cert the certificate for new handshakes.
func (h *CertHolder) Store(cert tls.Certificate) {
	c := cert
	h.p.Store(&c)
}

// Load returns the current certificate.
func (h *CertHolder) Load() *tls.Certificate { return h.p.Load() }

// Leaf returns the parsed current certificate.
func (h *CertHolder) Leaf() *x509.Certificate { return h.p.Load().Leaf }

// GetCertificate is a tls.Config.GetCertificate hook for a server.
func (h *CertHolder) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return h.current()
}

// GetClientCertificate is a tls.Config.GetClientCertificate hook for a client.
func (h *CertHolder) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return h.current()
}

func (h *CertHolder) current() (*tls.Certificate, error) {
	c := h.p.Load()
	if c == nil {
		return nil, errors.New("pki: no certificate")
	}
	return c, nil
}

// issuedAt is when the certificate was issued. IssueNodeCert backdates
// NotBefore by clockSkew, so the issue time is clockSkew later.
func issuedAt(cert *x509.Certificate) time.Time {
	t := cert.NotBefore.Add(clockSkew)
	if !t.Before(cert.NotAfter) {
		return cert.NotBefore
	}
	return t
}

// RenewAt is when a certificate should be renewed: once two thirds of its
// lifetime, counted from its issue time, have passed. For a 365-day
// certificate that is day 243, with 122 days left, so a node that cannot
// reach the server has months to come back.
func RenewAt(cert *x509.Certificate) time.Time {
	from := issuedAt(cert)
	return from.Add(cert.NotAfter.Sub(from) * 2 / 3)
}

// RenewDue reports whether cert should be renewed at now.
func RenewDue(cert *x509.Certificate, now time.Time) bool {
	return !now.Before(RenewAt(cert))
}

// RenewCheckInterval is how often a holder of cert checks whether it is due:
// a twentieth of its lifetime, between one second and one hour. A 365-day
// certificate is checked hourly. Tests issue certificates that last minutes.
func RenewCheckInterval(cert *x509.Certificate) time.Duration {
	d := cert.NotAfter.Sub(issuedAt(cert)) / 20
	return min(max(d, time.Second), time.Hour)
}

// CheckValidAt returns an error if cert is not valid at now: not yet valid,
// or expired.
func CheckValidAt(cert *x509.Certificate, now time.Time) error {
	switch {
	case now.Before(cert.NotBefore):
		return ErrCertNotYetValid
	case now.After(cert.NotAfter):
		return ErrCertExpired
	}
	return nil
}

// Errors from CheckValidAt.
var (
	ErrCertExpired     = errors.New("pki: certificate has expired")
	ErrCertNotYetValid = errors.New("pki: certificate is not valid yet")
)

// ServerTLSConfigFrom is ServerTLSConfig with the certificate read from h at
// each handshake, so a renewed certificate is used without a restart.
func ServerTLSConfigFrom(ca *CA, h *CertHolder) *tls.Config {
	return &tls.Config{
		GetCertificate:        h.GetCertificate,
		ClientAuth:            tls.RequireAndVerifyClientCert,
		ClientCAs:             caPool(ca),
		MinVersion:            tls.VersionTLS13,
		VerifyPeerCertificate: requireNodeID,
	}
}

// JoinServerTLSConfigFrom is JoinServerTLSConfig with the certificate read
// from h at each handshake.
func JoinServerTLSConfigFrom(h *CertHolder) *tls.Config {
	return &tls.Config{
		GetCertificate: h.GetCertificate,
		MinVersion:     tls.VersionTLS13,
	}
}

// ClientTLSConfigFrom is ClientTLSConfig with the client certificate read
// from h at each handshake, so a worker that renews its certificate presents
// the new one on its next connection.
func ClientTLSConfigFrom(ca *CA, h *CertHolder, serverName string) (*tls.Config, error) {
	if serverName == "" {
		return nil, errors.New("pki: serverName is required")
	}
	return &tls.Config{
		GetClientCertificate:  h.GetClientCertificate,
		RootCAs:               caPool(ca),
		ServerName:            serverName,
		MinVersion:            tls.VersionTLS13,
		VerifyPeerCertificate: requireNodeID,
	}, nil
}
