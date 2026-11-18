package billing

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// Secretbox encrypts small secrets (the national ID) with AES-256-GCM and
// makes a keyed hash for exact search. Two sub-keys are derived from DATA_KEY
// so the cipher key and the search key are never the same bytes.
type Secretbox struct {
	aead   cipher.AEAD
	idxKey []byte
}

// ErrNoDataKey is returned when the national-ID feature is used without DATA_KEY.
var ErrNoDataKey = errors.New("the national ID feature is off: set DATA_KEY on the server (openssl rand -base64 32)")

func derive(key []byte, label string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// NewSecretbox needs a 32-byte key.
func NewSecretbox(key []byte) (*Secretbox, error) {
	if len(key) != 32 {
		return nil, errors.New("DATA_KEY must be 32 bytes")
	}
	blk, err := aes.NewCipher(derive(key, "ispbill/enc"))
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Secretbox{aead: g, idxKey: derive(key, "ispbill/idx")}, nil
}

// NormalizeNationalID upper-cases and strips spaces and dashes. It accepts 5
// to 15 letters and digits (Kenyan IDs are 7-8 digits; passports vary).
func NormalizeNationalID(s string) (string, error) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "", ".", "").Replace(strings.TrimSpace(s)))
	if len(s) < 5 || len(s) > 15 {
		return "", errors.New("a national ID or passport number has 5 to 15 letters and digits")
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
			return "", errors.New("a national ID or passport number has only letters and digits")
		}
	}
	return s, nil
}

// Seal returns base64(nonce || ciphertext).
func (b *Secretbox) Seal(plain string) (string, error) {
	n := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b.aead.Seal(n, n, []byte(plain), nil)), nil
}

// Open reverses Seal; a wrong key or a damaged value is an error.
func (b *Secretbox) Open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("stored value is damaged")
	}
	n, ct := raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():]
	p, err := b.aead.Open(nil, n, ct, nil)
	if err != nil {
		return "", errors.New("cannot decrypt: DATA_KEY is not the key this value was written with")
	}
	return string(p), nil
}

// Index is the blind index: the same ID always gives the same hex string.
func (b *Secretbox) Index(plain string) string {
	m := hmac.New(sha256.New, b.idxKey)
	m.Write([]byte(plain))
	return hex.EncodeToString(m.Sum(nil))
}
