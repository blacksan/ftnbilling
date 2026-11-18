package authn

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Token prefixes: they make a leaked token recognisable (secret scanners) and
// let the server tell an access token from a refresh token without a lookup.
const (
	accessPrefix  = "ftna_"
	refreshPrefix = "ftnr_"
)

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("authn: no random source: " + err.Error())
	}
	return b
}

// RandomID is a public identifier (a device or user id): 16 hex digits.
func RandomID() string { return hex.EncodeToString(randomBytes(8)) }

// HashToken is the hex SHA-256 of a secret. Tokens and session ids are 256-bit
// random values, so a fast hash is the right tool.
func HashToken(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// hashEq compares two hex hashes in constant time.
func hashEq(a, b string) bool {
	return len(a) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// newToken makes "<prefix><deviceID>.<43 chars>". The device id is public (it
// only selects the row); the secret part is what is hashed and compared.
func newToken(prefix, deviceID string) string {
	return prefix + deviceID + "." + base64.RawURLEncoding.EncodeToString(randomBytes(32))
}

// splitToken separates the device id of a token of the given kind.
func splitToken(prefix, tok string) (deviceID string, ok bool) {
	rest, found := strings.CutPrefix(tok, prefix)
	if !found || len(tok) > 200 {
		return "", false
	}
	id, secret, found := strings.Cut(rest, ".")
	if !found || len(id) == 0 || len(id) > 40 || len(secret) < 40 {
		return "", false
	}
	return id, true
}

// NewSessionValue makes the browser cookie value (256 random bits). Only its
// hash is stored.
func NewSessionValue() string { return base64.RawURLEncoding.EncodeToString(randomBytes(32)) }

// NewCSRFToken makes a per-session CSRF token.
func NewCSRFToken() string { return hex.EncodeToString(randomBytes(24)) }
