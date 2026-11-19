package store

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
)

// NewApartmentID returns "a" + 6 random hex characters.
func NewApartmentID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "a" + hex.EncodeToString(b)
}

// ApartmentList returns the apartments, natural-sorted by name.
func (d *Data) ApartmentList() []Apartment {
	out := make([]Apartment, 0, len(d.Apartments))
	for _, a := range d.Apartments {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return NaturalLess(out[i].Name, out[j].Name)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ApartmentByName finds an apartment by name, ignoring case and extra spaces.
func (d *Data) ApartmentByName(name string) (Apartment, bool) {
	k := apartmentNameKey(name)
	if k == "" {
		return Apartment{}, false
	}
	for _, a := range d.Apartments {
		if apartmentNameKey(a.Name) == k {
			return a, true
		}
	}
	return Apartment{}, false
}

func apartmentNameKey(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// ApartmentServedBy reports whether the router serves the apartment.
func (d *Data) ApartmentServedBy(apartmentID, siteID string) bool {
	_, ok := d.ApartmentSites[ApartmentSiteKey(apartmentID, siteID)]
	return ok
}

// ApartmentSiteIDs lists the routers that serve an apartment (removed routers
// excluded), by router name.
func (d *Data) ApartmentSiteIDs(apartmentID string) []string {
	var sites []Site
	for _, l := range d.ApartmentSites {
		if l.ApartmentID != apartmentID {
			continue
		}
		if s, ok := d.Sites[l.SiteID]; ok && !s.Removed {
			sites = append(sites, s)
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].Name != sites[j].Name {
			return NaturalLess(sites[i].Name, sites[j].Name)
		}
		return sites[i].ID < sites[j].ID
	})
	out := make([]string, len(sites))
	for i, s := range sites {
		out[i] = s.ID
	}
	return out
}

// SiteApartmentIDs lists the apartments a router serves, by apartment name.
func (d *Data) SiteApartmentIDs(siteID string) []string {
	var apts []Apartment
	for _, l := range d.ApartmentSites {
		if l.SiteID != siteID {
			continue
		}
		if a, ok := d.Apartments[l.ApartmentID]; ok {
			apts = append(apts, a)
		}
	}
	sort.Slice(apts, func(i, j int) bool {
		if apts[i].Name != apts[j].Name {
			return NaturalLess(apts[i].Name, apts[j].Name)
		}
		return apts[i].ID < apts[j].ID
	})
	out := make([]string, len(apts))
	for i, a := range apts {
		out[i] = a.ID
	}
	return out
}

// OpenAccountsOfApartment lists the open accounts of an apartment in unit order.
func (d *Data) OpenAccountsOfApartment(apartmentID string) []Account {
	var out []Account
	for _, a := range d.Accounts {
		if a.ApartmentID == apartmentID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Unit != out[j].Unit {
			return NaturalLess(out[i].Unit, out[j].Unit)
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// UnitKey is how units are compared: ignoring case and extra spaces.
func UnitKey(s string) string { return apartmentNameKey(s) }
