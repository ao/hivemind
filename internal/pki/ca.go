// Package pki holds the cluster certificate authority, node certificates,
// join tokens and the TLS configurations for node-to-node traffic. It uses
// only the standard library and does no network I/O.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	// CACertFile and CAKeyFile are the names of the CA files under the data dir.
	CACertFile = "ca.crt"
	CAKeyFile  = "ca.key"

	caLifetime = 10 * 365 * 24 * time.Hour

	// clockSkew backdates NotBefore so that a node whose clock runs slightly
	// behind can still use a certificate issued a moment ago.
	clockSkew = 5 * time.Minute
)

var (
	// ErrCAExists is returned by CreateCA when either CA file is already present.
	ErrCAExists = errors.New("pki: cluster CA already exists")
	// ErrKeyPermissions is returned by LoadCA when the key is readable by group or others.
	ErrKeyPermissions = errors.New("pki: CA key is accessible to group or others")
)

// CA is the cluster certificate authority.
type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	Key     *ecdsa.PrivateKey
}

// CreateCA makes a new self-signed ECDSA P-256 CA valid for ten years and
// writes ca.key (mode 0600) and ca.crt into dir. Both files are created with
// O_EXCL, so an existing CA is never overwritten. The key is written first, so
// a failure can't leave a certificate without its key.
func CreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "hivemind cluster CA", Organization: []string{"hivemind"}},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("pki: self-sign CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyPath := filepath.Join(dir, CAKeyFile)
	certPath := filepath.Join(dir, CACertFile)
	if err := writeExclusive(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := writeExclusive(certPath, certPEM, 0o644); err != nil {
		// The key was created by this call. Remove it so the directory
		// does not hold a key with no certificate.
		_ = os.Remove(keyPath)
		return nil, err
	}
	return &CA{Cert: cert, CertPEM: certPEM, Key: key}, nil
}

// LoadCA reads the CA from dir. It refuses a key that is a symlink or
// not a regular file, or that group or others can read.
func LoadCA(dir string) (*CA, error) {
	keyPath := filepath.Join(dir, CAKeyFile)
	info, err := os.Lstat(keyPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("pki: %s is not a regular file", keyPath)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s has mode %04o, want 0600", ErrKeyPermissions, keyPath, info.Mode().Perm())
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("pki: %s is not a PEM EC private key", keyPath)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CA key: %w", err)
	}
	if key.Curve != elliptic.P256() {
		return nil, errors.New("pki: CA key is not ECDSA P-256")
	}

	certPEM, err := os.ReadFile(filepath.Join(dir, CACertFile))
	if err != nil {
		return nil, err
	}
	cblock, _ := pem.Decode(certPEM)
	if cblock == nil || cblock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("pki: %s is not a PEM certificate", CACertFile)
	}
	cert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("pki: CA certificate is not a CA")
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("pki: CA certificate does not match CA key")
	}
	return &CA{Cert: cert, CertPEM: certPEM, Key: key}, nil
}

// Fingerprint is the SHA-256 of the certificate's SubjectPublicKeyInfo. Join
// tokens carry it so a joining node can pin the CA.
func Fingerprint(cert *x509.Certificate) [32]byte {
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo)
}

// Fingerprint returns the fingerprint of the CA's public key.
func (ca *CA) Fingerprint() [32]byte {
	return Fingerprint(ca.Cert)
}

// writeExclusive creates path with O_EXCL and writes data. It returns an error
// wrapping ErrCAExists if the file is already there.
func writeExclusive(path string, data []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrCAExists, path)
		}
		return err
	}
	// The umask can narrow the mode at create time. Set it explicitly.
	werr := f.Chmod(mode)
	if werr == nil {
		_, werr = f.Write(data)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return werr
	}
	return nil
}

// randomSerial returns a positive 128-bit serial number.
func randomSerial() (*big.Int, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, fmt.Errorf("pki: serial number: %w", err)
	}
	b[0] |= 0x80
	return new(big.Int).SetBytes(b), nil
}
