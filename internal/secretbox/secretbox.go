// Package secretbox encrypts small secrets with AES-256-GCM. Sub-keys are
// derived from DATA_KEY by label, so the national-ID cipher, its search index
// and the admin settings never share key bytes.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

// Box seals and opens values and makes a keyed hash for exact search.
type Box struct {
	aead   cipher.AEAD
	idxKey []byte
}

func derive(key []byte, label string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// New returns the box used for the national-ID field (labels ispbill/enc and
// ispbill/idx). It needs a 32-byte key.
func New(key []byte) (*Box, error) { return NewLabelled(key, "ispbill/enc", "ispbill/idx") }

// NewSettings returns the box used for admin settings. Its cipher key is
// derived with its own label, so a ciphertext of one box never opens in the other.
func NewSettings(key []byte) (*Box, error) {
	return NewLabelled(key, "ispbill/settings", "ispbill/settings-idx")
}

// NewLabelled derives the cipher key and the index key from key and labels.
func NewLabelled(key []byte, encLabel, idxLabel string) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("DATA_KEY must be 32 bytes")
	}
	blk, err := aes.NewCipher(derive(key, encLabel))
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Box{aead: g, idxKey: derive(key, idxLabel)}, nil
}

// Seal returns base64(nonce || ciphertext).
func (b *Box) Seal(plain string) (string, error) { return b.SealAAD(plain, nil) }

// Open reverses Seal; a wrong key or a damaged value is an error.
func (b *Box) Open(sealed string) (string, error) { return b.OpenAAD(sealed, nil) }

// SealAAD binds the ciphertext to aad (for settings, the setting key), so a
// sealed value copied into another row no longer opens.
func (b *Box) SealAAD(plain string, aad []byte) (string, error) {
	n := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b.aead.Seal(n, n, []byte(plain), aad)), nil
}

// OpenAAD reverses SealAAD with the same aad.
func (b *Box) OpenAAD(sealed string, aad []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("stored value is damaged")
	}
	n, ct := raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():]
	p, err := b.aead.Open(nil, n, ct, aad)
	if err != nil {
		return "", errors.New("cannot decrypt: DATA_KEY is not the key this value was written with")
	}
	return string(p), nil
}

// Index is the blind index: the same input always gives the same hex string.
func (b *Box) Index(plain string) string {
	m := hmac.New(sha256.New, b.idxKey)
	m.Write([]byte(plain))
	return hex.EncodeToString(m.Sum(nil))
}
