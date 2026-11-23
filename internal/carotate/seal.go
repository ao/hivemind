package carotate

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/ao/hivemind/internal/pki"
)

// The new CA's key in the rotation's record (docs/ha.md, where the new CA
// is kept). It is sealed with AES-256-GCM under a key derived with HKDF-SHA256
// from the current CA's private key, which every server has in ca.key and
// nothing else in the cluster does. The salt is the new CA's fingerprint,
// and the associated data names both CAs, so a sealed key opens only for
// the rotation it was made for. Once finish has replaced every server's
// ca.key, what is left of it in older log entries and snapshots cannot be
// opened.

// sealInfo is HKDF's info, and starts the associated data.
const sealInfo = "hivemind ca-next key v1"

// errSealed is a sealed key that does not open.
var errSealed = errors.New("the new CA's key in the CA rotation's record does not open with this server's ca.key")

// sealNextKey seals the new CA's key, in the form ca-next.key holds, under
// the current CA's key.
func sealNextKey(cur *pki.CA, nextFP [32]byte, keyPEM []byte) ([]byte, error) {
	aead, err := sealAEAD(cur, nextFP)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, keyPEM, sealAD(cur.Fingerprint(), nextFP)), nil
}

// openNextKey opens what sealNextKey sealed, with the current CA's key.
func openNextKey(cur *pki.CA, nextFP [32]byte, sealed []byte) ([]byte, error) {
	aead, err := sealAEAD(cur, nextFP)
	if err != nil {
		return nil, err
	}
	n := aead.NonceSize()
	if len(sealed) < n+aead.Overhead() {
		return nil, fmt.Errorf("%w: it is %d bytes", errSealed, len(sealed))
	}
	out, err := aead.Open(nil, sealed[:n], sealed[n:], sealAD(cur.Fingerprint(), nextFP))
	if err != nil {
		return nil, errSealed
	}
	return out, nil
}

func sealAEAD(cur *pki.CA, nextFP [32]byte) (cipher.AEAD, error) {
	if cur == nil || cur.Key == nil {
		return nil, errors.New("carotate: no current CA key to seal with")
	}
	secret, err := x509.MarshalPKCS8PrivateKey(cur.Key)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, secret, nextFP[:], sealInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealAD(curFP, nextFP [32]byte) []byte {
	ad := append([]byte(sealInfo), curFP[:]...)
	return append(ad, nextFP[:]...)
}
