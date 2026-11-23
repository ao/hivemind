package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// A server that joins a cluster of several gets the cluster CA, its key
// included, in the join reply, and writes it as init would (docs/ha.md).

// KeyPEM is the CA's private key in the form ca.key holds.
func (ca *CA) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(ca.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// ParseCAPair reads a CA certificate and its key in the forms ca.crt and
// ca.key hold, and checks that they belong together, as LoadCA does.
func ParseCAPair(certPEM, keyPEM []byte) (*CA, error) {
	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "EC PRIVATE KEY" {
		return nil, errors.New("pki: not a PEM EC private key")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CA key: %w", err)
	}
	if key.Curve != elliptic.P256() {
		return nil, errors.New("pki: CA key is not ECDSA P-256")
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, errors.New("pki: not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
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

// WriteCAFiles writes ca.key (mode 0600) and ca.crt into dir, each with
// O_EXCL, key first, and syncs them, as CreateCA does with a CA it makes.
// The pair must belong together. On failure nothing is left.
func WriteCAFiles(dir string, certPEM, keyPEM []byte) (*CA, error) {
	return writeCAPair(dir, CACertFile, CAKeyFile, certPEM, keyPEM)
}

// WriteNextCAFiles writes ca-next.key and ca-next.crt as WriteCAFiles
// writes ca.key and ca.crt: a server of a cluster of several writes its own
// copy of the CA a rotation moves to from the rotation's record.
func WriteNextCAFiles(dir string, certPEM, keyPEM []byte) (*CA, error) {
	return writeCAPair(dir, NextCACertFile, NextCAKeyFile, certPEM, keyPEM)
}

func writeCAPair(dir, certName, keyName string, certPEM, keyPEM []byte) (*CA, error) {
	ca, err := ParseCAPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, keyName)
	certPath := filepath.Join(dir, certName)
	if err := writeExclusive(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := writeExclusive(certPath, certPEM, 0o644); err != nil {
		_ = os.Remove(keyPath)
		return nil, err
	}
	for _, p := range []string{keyPath, certPath, dir} {
		if err := syncPath(p); err != nil {
			_ = os.Remove(keyPath)
			_ = os.Remove(certPath)
			return nil, err
		}
	}
	return ca, nil
}
