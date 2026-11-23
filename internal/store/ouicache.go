package store

import (
	"strings"
	"time"
)

// OUIEntry is one answer of the online MAC-vendor lookup, kept so the same prefix is never asked twice: Found is a
// vendor name (Vendor, already trimmed and length-capped by the lookup), not Found is a "no such prefix" (404) that
// is asked again after 30 days. Key: Prefix, the first three bytes of a MAC as "AA:BB:CC". A MAC itself is never
// stored here, and the table holds nothing about customers.
type OUIEntry struct {
	Prefix    string    `json:"prefix"`
	Vendor    string    `json:"vendor,omitempty"`
	Found     bool      `json:"found"`
	CheckedAt time.Time `json:"checked_at"`
}

// MaxOUIVendor caps a stored vendor name: the answer of a web service is untrusted text.
const MaxOUIVendor = 80

func (x OUIEntry) norm() OUIEntry {
	x.Prefix = strings.ToUpper(strings.TrimSpace(x.Prefix))
	x.CheckedAt = nt(x.CheckedAt)
	if !x.Found {
		x.Vendor = ""
	}
	return x
}

// Norm is the form the store keeps (for tests).
func (x OUIEntry) Norm() OUIEntry { return x.norm() }
