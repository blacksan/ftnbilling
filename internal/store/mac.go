package store

import (
	"errors"
	"strings"
)

// ErrDuplicateMAC means a change would put one MAC address on more than one
// open account. A MAC is unique across all buildings among open accounts
// (closed accounts do not count; accounts flagged MACConflict are excluded
// until somebody fixes them). Update returns it before and instead of writing,
// and the Postgres backend maps its unique index to it.
var ErrDuplicateMAC = errors.New("that MAC address already belongs to another account")

// ErrBadMAC is returned for text that is not a unicast MAC address.
var ErrBadMAC = errors.New("enter the ONT WAN MAC address like AA:BB:CC:DD:EE:FF")

// CanonMAC returns the upper-case colon form AA:BB:CC:DD:EE:FF of any
// spelling of a 12-digit MAC (colons, dashes, dots, spaces or none). It does
// not judge the address: multicast and all-zero values pass. ok is false when
// s is not 12 hex digits; callers then keep the text as it was.
func CanonMAC(s string) (string, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.NewReplacer(":", "", "-", "", ".", "", " ", "").Replace(s)
	if len(s) != 12 {
		return "", false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'F') {
			return "", false
		}
	}
	var b strings.Builder
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(s[i : i+2])
	}
	return b.String(), true
}

// NormalizeMAC accepts AA:BB:CC:DD:EE:FF, AA-BB-..., aabb.ccdd.eeff or
// aabbccddeeff and returns the upper-case colon form. Multicast and all-zero
// addresses are refused.
func NormalizeMAC(s string) (string, error) {
	m, ok := CanonMAC(s)
	if !ok {
		return "", ErrBadMAC
	}
	switch m[1] {
	case '1', '3', '5', '7', '9', 'B', 'D', 'F':
		return "", ErrBadMAC // multicast
	}
	if m == "00:00:00:00:00:00" {
		return "", ErrBadMAC
	}
	return m, nil
}
