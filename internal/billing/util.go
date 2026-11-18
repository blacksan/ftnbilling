// Package billing holds the business rules: orders, subscribers, vouchers,
// router synchronisation and expiry.
package billing

import (
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"ispbill/internal/store"
)

// EAT is East Africa Time (UTC+3, no DST).
var EAT = time.FixedZone("EAT", 3*3600)

// codeAlphabet has 32 symbols and no look-alikes (no 0/O, 1/I/L).
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is unrecoverable
	}
	return b
}

// RandomCode returns n random characters from the unambiguous alphabet.
func RandomCode(n int) string {
	out := make([]byte, n)
	for i := range out {
		// Rejection sampling keeps the distribution uniform.
		for {
			b := randBytes(1)[0] & 31
			if int(b) < len(codeAlphabet) {
				out[i] = codeAlphabet[b]
				break
			}
		}
	}
	return string(out)
}

// RandomSecret returns 2*n hex characters from crypto/rand (internal passwords).
func RandomSecret(n int) string { return fmt.Sprintf("%x", randBytes(n)) }

// RandomID returns a 12-char hex id (fits Daraja's 12-char AccountReference).
func RandomID() string { return fmt.Sprintf("%x", randBytes(6)) }

var phoneStrip = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "")

// ErrBadPhone is returned for numbers that are not Kenyan mobile numbers.
var ErrBadPhone = errors.New("enter a valid Safaricom/Airtel number like 0712345678")

var phoneRe = regexp.MustCompile(`^254[17]\d{8}$`)

// NormalizePhone converts 07XXXXXXXX, 01XXXXXXXX, 7XXXXXXXX, +2547XXXXXXXX and
// 2547XXXXXXXX to 2547XXXXXXXX (or 2541XXXXXXXX).
func NormalizePhone(s string) (string, error) {
	s = phoneStrip.Replace(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "+")
	switch {
	case strings.HasPrefix(s, "0") && len(s) == 10:
		s = "254" + s[1:]
	case len(s) == 9 && (s[0] == '7' || s[0] == '1'):
		s = "254" + s
	}
	if !phoneRe.MatchString(s) {
		return "", ErrBadPhone
	}
	return s, nil
}

// NormalizeCode upper-cases and trims user-typed codes and voucher codes.
func NormalizeCode(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify makes a router-safe name fragment.
func Slugify(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		s = "pkg"
	}
	return s
}

// ErrBadMAC is returned for text that is not a unicast MAC address.
var ErrBadMAC = store.ErrBadMAC

// NormalizeMAC accepts AA:BB:CC:DD:EE:FF, AA-BB-..., aabb.ccdd.eeff or
// aabbccddeeff and returns the upper-case colon form. Multicast and all-zero
// addresses are refused. The work is done by store.NormalizeMAC.
func NormalizeMAC(s string) (string, error) { return store.NormalizeMAC(s) }

var mpesaCodeRe = regexp.MustCompile(`^[A-Z0-9]{10}$`)

// ErrBadTransCode is returned for M-Pesa transaction codes of the wrong shape.
var ErrBadTransCode = errors.New("an M-Pesa transaction code is 10 capital letters and digits, e.g. SGL7K2M9XA")

// NormalizeTransCode upper-cases and validates an M-Pesa transaction code.
func NormalizeTransCode(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !mpesaCodeRe.MatchString(s) {
		return "", ErrBadTransCode
	}
	return s, nil
}
