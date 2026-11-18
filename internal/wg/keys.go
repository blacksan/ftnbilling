// Package wg holds the small WireGuard helpers the billing server needs:
// X25519 key generation, rendering the peers file the root sync script
// applies, and reading the status dump that script's timer writes.
package wg

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

// GenerateKeypair makes a new WireGuard keypair (base64, 44 characters each).
// Go's X25519 implementation clamps the scalar exactly like `wg genkey`.
func GenerateKeypair() (priv, pub string, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k.Bytes()), base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// PublicKey derives the public key from a base64 private key.
func PublicKey(priv string) (string, error) {
	if !ValidKey(priv) {
		return "", errors.New("wg: not a valid key")
	}
	raw, _ := base64.StdEncoding.DecodeString(priv)
	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// ValidKey reports whether s is a 44-character base64 string that decodes to
// 32 bytes (the shape of every WireGuard key).
func ValidKey(s string) bool {
	if len(s) != 44 {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}
