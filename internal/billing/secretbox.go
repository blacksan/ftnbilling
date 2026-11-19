package billing

import (
	"errors"
	"strings"

	"ispbill/internal/secretbox"
)

// Secretbox encrypts the national ID and makes a keyed hash for exact search.
// It lives in internal/secretbox so admin settings can share the key
// derivation; this alias keeps the billing API unchanged.
type Secretbox = secretbox.Box

// ErrNoDataKey is returned when the national-ID feature is used without DATA_KEY.
var ErrNoDataKey = errors.New("the national ID feature is off: set DATA_KEY on the server (openssl rand -base64 32)")

// NewSecretbox needs a 32-byte key.
func NewSecretbox(key []byte) (*Secretbox, error) { return secretbox.New(key) }

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
