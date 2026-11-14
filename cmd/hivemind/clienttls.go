package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
)

// maxCAFile caps a --ca-cert file. A bundle of a few hundred certificates
// fits.
const maxCAFile = 1 << 20

// apiTLSConfig is the TLS config the CLI uses for an https server.
//
// The minimum version is TLS 1.2, and Go's client offers 1.3 first, so a Go
// server such as hivemind serve gets 1.3. The 1.2 floor lets the CLI reach an
// API behind a proxy that ends TLS at 1.2; Go's default 1.2 suites are all
// ECDHE with AEAD ciphers, so allowing 1.2 does not allow a weak cipher. The
// server's name is checked against the URL's host, as net/http always does,
// and there is no option to skip the check.
//
// With caPath set, only the certificates in that file are trusted, not the
// system's roots. Without it, the system's roots are. caPath on an http URL is
// an error, because it would protect nothing.
func apiTLSConfig(u *url.URL, caPath string) (*tls.Config, error) {
	if u.Scheme != "https" {
		if caPath != "" {
			return nil, fmt.Errorf("--ca-cert %s applies to an https server, but the server is %s", caPath, u.Redacted())
		}
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}
	if caPath != "" {
		pool, err := loadCAPool(caPath)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// loadCAPool reads every CERTIFICATE block in a PEM file. A file with none is
// an error. A certificate in the pool is trusted as a root, so the file can
// hold a CA, or a self-signed server certificate itself.
func loadCAPool(path string) (*x509.CertPool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--ca-cert: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCAFile+1))
	if err != nil {
		return nil, fmt.Errorf("--ca-cert %s: %w", path, err)
	}
	if len(b) > maxCAFile {
		return nil, fmt.Errorf("--ca-cert %s is larger than %d bytes", path, maxCAFile)
	}
	pool := x509.NewCertPool()
	n := 0
	for rest := b; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("--ca-cert %s: certificate %d: %w", path, n+1, err)
		}
		pool.AddCert(cert)
		n++
	}
	if n == 0 {
		return nil, fmt.Errorf("--ca-cert %s holds no PEM certificate", path)
	}
	return pool, nil
}

// certNameNote returns a warning when the first certificate in certPath does
// not name the host of serverURL, which a client would then refuse. It
// returns "" when the name matches or the file cannot be read; serve reports
// a bad file itself.
func certNameNote(certPath, serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(certPath)
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	if err := cert.VerifyHostname(u.Hostname()); err != nil {
		return fmt.Sprintf("Note: %s does not name %s, so the CLI will refuse it at %s. Use --server with a name the certificate carries.",
			certPath, u.Hostname(), serverURL)
	}
	return ""
}

// tlsReason explains a failed certificate check, or returns "" when err is
// not one. caPath is the --ca-cert file, if one was given.
func tlsReason(err error, caPath string) string {
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	switch {
	case errors.As(err, &ua):
		if caPath != "" {
			return fmt.Sprintf("the server's certificate is not signed by a CA in %s", caPath)
		}
		return "the server's certificate is signed by an unknown authority; pass --ca-cert (or set " + envCACert +
			") with the CA certificate that signed it"
	case errors.As(err, &he):
		return fmt.Sprintf("the server's certificate does not name %s: %v", he.Host, he)
	case errors.As(err, &ci):
		return fmt.Sprintf("the server's certificate is not valid: %v", ci)
	}
	return ""
}
