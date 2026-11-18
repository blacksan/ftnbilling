package settings

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// KeyAdminHash is the settings row that holds the admin password hash.
const KeyAdminHash = "admin.pass_hash"

const (
	pbkdf2Iter = 600000
	hashPrefix = "pbkdf2-sha256"
	saltBytes  = 16
	hashBytes  = 32
	minPassLen = 12
	maxPassLen = 200
)

// HashPassword returns pbkdf2-sha256$600000$<b64 salt>$<b64 hash>.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return encodeHash(pw, salt, pbkdf2Iter)
}

func encodeHash(pw string, salt []byte, iter int) (string, error) {
	k, err := pbkdf2.Key(sha256.New, pw, salt, iter, hashBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashPrefix, iter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(k)), nil
}

// VerifyPassword checks pw against a stored hash in constant time. A damaged
// or unknown hash never verifies.
func VerifyPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != hashPrefix {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 || iter > 10_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) < 8 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) != hashBytes {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, hashBytes)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// CheckNewPassword applies the password rules: at least 12 characters, not
// "admin", not the user name.
func CheckNewPassword(pw, user string) error {
	switch {
	case len(pw) < minPassLen:
		return errors.New("the password must be at least 12 characters")
	case len(pw) > maxPassLen:
		return errors.New("the password is too long (200 characters at most)")
	case strings.EqualFold(pw, "admin") || (user != "" && strings.EqualFold(pw, user)):
		return errors.New("the password must not be \"admin\" or the user name")
	}
	for _, r := range pw {
		if r < 0x20 || r == 0x7f {
			return errors.New("the password must not contain control characters")
		}
	}
	return nil
}

// RandomPassword makes a 20-character password from an unambiguous alphabet
// (the reset command prints it once).
func RandomPassword() (string, error) {
	const alpha = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, len(b))
	for i, x := range b {
		out[i] = alpha[int(x)%len(alpha)] // 54 symbols: the bias is negligible for a one-off password
	}
	return string(out), nil
}
