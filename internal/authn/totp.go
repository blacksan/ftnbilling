// Package authn is the sign-in machinery of the admin pages and the app:
// TOTP, recovery and setup codes, browser sessions, and API tokens. It keeps
// all state in the store (tables admin_users, admin_sessions, api_devices).
// Nothing here logs or returns a secret except the one call that has to show
// it once (enrolment, recovery codes, setup code, new tokens).
package authn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters (RFC 6238 with the defaults every authenticator app uses).
const (
	totpDigits = 6
	totpPeriod = 30
	// totpSkew is how many 30-second steps either side of now are accepted.
	totpSkew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns 20 random bytes (the RFC 4226 recommended length).
func NewTOTPSecret() ([]byte, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// EncodeSecret is the base32 form authenticator apps take.
func EncodeSecret(secret []byte) string { return b32.EncodeToString(secret) }

// DecodeSecret reverses EncodeSecret (spaces, dashes and case are ignored).
func DecodeSecret(s string) ([]byte, error) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(s))
	return b32.DecodeString(s)
}

// TOTPStep is the time step of t.
func TOTPStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// hotp is RFC 4226 with SHA-1 and 6 digits.
func hotp(secret []byte, counter uint64) string {
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], counter)
	m := hmac.New(sha1.New, secret)
	m.Write(c[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1000000)
}

// TOTPCode is the code for a step (exported for tests and tools).
func TOTPCode(secret []byte, step int64) string { return hotp(secret, uint64(step)) }

// NormalizeCode strips spaces and dashes from what a person typed.
func NormalizeCode(s string) string {
	return strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(strings.TrimSpace(s))
}

// MatchTOTP checks code against the steps around now. It returns the step that
// matched, and only accepts a step greater than lastStep, so a code is good
// once: the caller stores the returned step (replay protection). All steps are
// compared in constant time, whether or not an earlier one matched.
func MatchTOTP(secret []byte, code string, now time.Time, lastStep int64) (step int64, ok bool) {
	code = NormalizeCode(code)
	if len(code) != totpDigits {
		return 0, false
	}
	cur := TOTPStep(now)
	found := int64(0)
	hit := 0
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		s := cur + d
		eq := subtle.ConstantTimeCompare([]byte(hotp(secret, uint64(s))), []byte(code))
		if eq == 1 && s > lastStep {
			// Take the latest matching step (only one can match unless the
			// secret is degenerate).
			found, hit = s, 1
		}
	}
	return found, hit == 1
}

// OTPAuthURI is the address an authenticator app imports (shown as a QR code
// and as text).
func OTPAuthURI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", EncodeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + label + "?" + q.Encode()
}
