package web

import (
	"strings"

	"ispbill/internal/store"
)

// houseNo is the house number as the team reads it: a leading "Flat " is
// dropped, so a house stored as "Flat 1A" shows as "1A". The stored text is
// never changed.
func houseNo(unit string) string {
	u := strings.TrimSpace(unit)
	if len(u) > 5 && strings.EqualFold(u[:5], "flat ") {
		u = strings.TrimSpace(u[5:])
	}
	return u
}

// houseLabel is "House 1A" ("" when the account has no house number).
func houseLabel(unit string) string {
	if n := houseNo(unit); n != "" {
		return "House " + n
	}
	return ""
}

// kindName is the equipment kind in words: the key "ont" is shown as ONU.
func kindName(k string) string {
	switch k {
	case store.KindONT:
		return "ONU"
	case store.KindOLT:
		return "OLT"
	case "":
		return ""
	}
	return strings.ToUpper(k[:1]) + strings.ReplaceAll(k[1:], "_", " ")
}

// aptHouse writes "Apartment · House N" for an account.
func aptHouse(apt string, a store.Account) string {
	parts := []string{}
	if apt != "" {
		parts = append(parts, apt)
	}
	if h := houseLabel(a.Unit); h != "" {
		parts = append(parts, h)
	} else if h := houseLabel(a.Flat); h != "" {
		parts = append(parts, h)
	}
	return strings.Join(parts, " · ")
}
