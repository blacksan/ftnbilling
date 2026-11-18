package authn

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// RecoveryCodeCount is how many single-use codes enrolment hands out.
const RecoveryCodeCount = 10

// A recovery code is 10 characters from a 32-letter alphabet (50 bits), shown
// as xxxxx-xxxxx. They are random and long, so a plain SHA-256 is enough to
// store them (there is nothing to brute-force); comparison is constant time.
const recAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

func randomFrom(alpha string, n int) (string, error) {
	// Rejection sampling: no modulo bias.
	max := 256 - 256%len(alpha)
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) < max && len(out) < n {
				out = append(out, alpha[int(b)%len(alpha)])
			}
		}
	}
	return string(out), nil
}

// NewRecoveryCodes returns n fresh codes in display form and their hashes.
func NewRecoveryCodes(n int) (codes, hashes []string, err error) {
	for i := 0; i < n; i++ {
		raw, err := randomFrom(recAlphabet, 10)
		if err != nil {
			return nil, nil, err
		}
		codes = append(codes, raw[:5]+"-"+raw[5:])
		hashes = append(hashes, HashRecoveryCode(raw))
	}
	return codes, hashes, nil
}

func normRecovery(s string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "", "\t", "").Replace(strings.TrimSpace(s)))
}

// HashRecoveryCode hashes a code as typed (case, dashes and spaces ignored).
func HashRecoveryCode(code string) string {
	h := sha256.Sum256([]byte("ftn-recovery\x00" + normRecovery(code)))
	return hex.EncodeToString(h[:])
}

// matchRecovery returns the index of the hash that code matches, or -1. It
// compares against every stored hash, so timing does not tell which matched.
func matchRecovery(hashes []string, code string) int {
	h := []byte(HashRecoveryCode(code))
	idx := -1
	for i, s := range hashes {
		if subtle.ConstantTimeCompare(h, []byte(s)) == 1 {
			idx = i
		}
	}
	return idx
}
