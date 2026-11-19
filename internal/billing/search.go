package billing

import (
	"sort"
	"strings"

	"ispbill/internal/store"
)

// SearchLimit is the most results shown per group.
const SearchLimit = 20

// SearchResult groups what a query found.
type SearchResult struct {
	Query     string
	Customers []store.Customer
	Accounts  []store.Account
	Equipment []store.Equipment
	Payments  []store.Payment
	Buildings []store.Site
	// Apartments found by name, address, or caretaker/landlord name or phone.
	Apartments []store.Apartment
}

// Total is the number of hits over all groups.
func (r SearchResult) Total() int {
	return len(r.Customers) + len(r.Accounts) + len(r.Equipment) + len(r.Payments) + len(r.Buildings) + len(r.Apartments)
}

// Single returns what to open when the search found exactly one thing:
// kind is customer, account, equipment, payment, apartment or building.
func (r SearchResult) Single() (kind, id string, ok bool) {
	if r.Total() != 1 {
		return "", "", false
	}
	switch {
	case len(r.Customers) == 1:
		return "customer", r.Customers[0].ID, true
	case len(r.Accounts) == 1:
		return "account", r.Accounts[0].Code, true
	case len(r.Equipment) == 1:
		return "equipment", r.Equipment[0].ID, true
	case len(r.Payments) == 1:
		return "payment", r.Payments[0].Key, true
	}
	if len(r.Apartments) == 1 {
		return "apartment", r.Apartments[0].ID, true
	}
	return "building", r.Buildings[0].ID, true
}

// PhoneVariants turns what someone typed into the stored form 2547XXXXXXXX,
// or returns the digits for a suffix match (6 or more digits).
func phoneForms(q string) (full, digits string) {
	var b strings.Builder
	for _, r := range q {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits = b.String()
	switch {
	case len(digits) == 10 && digits[0] == '0':
		full = "254" + digits[1:]
	case len(digits) == 9 && (digits[0] == '7' || digits[0] == '1'):
		full = "254" + digits
	case len(digits) == 12 && strings.HasPrefix(digits, "254"):
		full = digits
	}
	return
}

func hexOnly(q string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(q) {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'F':
			b.WriteRune(r)
		case r == ':' || r == '-' || r == '.' || r == ' ':
		default:
			return ""
		}
	}
	return b.String()
}

func macDigits(m string) string {
	return strings.NewReplacer(":", "", "-", "", ".", "").Replace(strings.ToUpper(m))
}

// PhoneMatches reports whether a stored phone number (+254... or 254...)
// matches what someone typed in any common form.
func PhoneMatches(stored, q string) bool {
	full, digits := phoneForms(q)
	return phoneMatch(strings.TrimPrefix(stored, "+"), full, digits)
}

func phoneMatch(stored, full, digits string) bool {
	if stored == "" || digits == "" {
		return false
	}
	if full != "" {
		return stored == full
	}
	return len(digits) >= 6 && strings.HasSuffix(stored, digits)
}

// Search looks in memory. in reports whether a building is visible to the
// admin's current scope. Phone numbers match in any common form, MACs without
// separators, codes/receipts/serials exactly then by prefix, names and flats
// by case-folded substring. A national ID matches exactly through its blind
// index.
func (s *Service) Search(q string, in func(siteID string) bool) SearchResult {
	q = strings.TrimSpace(q)
	res := SearchResult{Query: q}
	if q == "" {
		return res
	}
	up := strings.ToUpper(q)
	full, digits := phoneForms(q)
	hexq := hexOnly(q)
	macQ := ""
	if len(hexq) >= 4 {
		macQ = hexq
	}
	nidHits := map[string]bool{}
	for _, c := range s.FindByNationalID(q) {
		nidHits[c.ID] = true
	}
	exact := map[string]bool{} // exact hits sort first
	s.st.View(func(d *store.Data) {
		for _, c := range d.Customers {
			if nidHits[c.ID] || store.ContainsFold(c.Name, q) || phoneMatch(c.Phone, full, digits) || phoneMatch(c.Phone2, full, digits) ||
				phoneMatch(c.WhatsApp, full, digits) || (len(q) >= 3 && store.ContainsFold(c.Email, q)) {
				res.Customers = append(res.Customers, c)
				exact[c.ID] = nidHits[c.ID] || strings.EqualFold(c.Name, q) || (full != "" && c.Phone == full)
			}
		}
		for _, a := range append(openAccounts(d), d.ClosedAccountList()...) {
			if !in(a.SiteID) {
				continue
			}
			if a.Code == up || strings.HasPrefix(a.Code, up) && len(up) >= 3 || store.ContainsFold(a.Flat, q) || store.ContainsFold(a.Tenant, q) ||
				phoneMatch(a.Phone, full, digits) || (macQ != "" && strings.Contains(macDigits(a.MAC), macQ)) {
				res.Accounts = append(res.Accounts, a)
				exact[a.ID] = a.Code == up
			}
		}
		for _, e := range d.Equipment {
			if e.SiteID != "" && !in(e.SiteID) {
				continue
			}
			if (e.Serial != "" && (e.Serial == up || strings.HasPrefix(e.Serial, up) && len(up) >= 3)) || (macQ != "" && strings.Contains(macDigits(e.MAC), macQ)) {
				res.Equipment = append(res.Equipment, e)
				exact[e.ID] = e.Serial == up
			}
		}
		for _, p := range d.Payments {
			if p.SiteID != "" && !in(p.SiteID) {
				continue
			}
			if p.Key == up || (len(up) >= 4 && strings.HasPrefix(p.Key, up)) || phoneMatch(p.Phone, full, digits) {
				res.Payments = append(res.Payments, p)
				exact[p.Key] = p.Key == up
			}
		}
		for _, st := range d.SiteList() {
			if in(st.ID) && (store.ContainsFold(st.Name, q) || (len(q) >= 3 && store.ContainsFold(st.Address, q))) {
				res.Buildings = append(res.Buildings, st)
			}
		}
		for _, a := range d.ApartmentList() {
			visible := len(d.ApartmentSiteIDs(a.ID)) == 0
			for _, id := range d.ApartmentSiteIDs(a.ID) {
				visible = visible || in(id)
			}
			if !visible {
				continue
			}
			hit := store.ContainsFold(a.Name, q) || (len(q) >= 3 && store.ContainsFold(a.Address, q))
			for _, c := range []store.ApartmentContact{a.Caretaker, a.Landlord} {
				hit = hit || (len(q) >= 2 && store.ContainsFold(c.Name, q)) || phoneMatch(strings.TrimPrefix(c.Phone, "+"), full, digits) || phoneMatch(strings.TrimPrefix(c.Phone2, "+"), full, digits)
			}
			if hit {
				res.Apartments = append(res.Apartments, a)
			}
		}
	})
	sort.SliceStable(res.Customers, func(i, j int) bool {
		a, b := res.Customers[i], res.Customers[j]
		if exact[a.ID] != exact[b.ID] {
			return exact[a.ID]
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	sort.SliceStable(res.Accounts, func(i, j int) bool { return exact[res.Accounts[i].ID] && !exact[res.Accounts[j].ID] })
	sort.SliceStable(res.Equipment, func(i, j int) bool { return exact[res.Equipment[i].ID] && !exact[res.Equipment[j].ID] })
	sort.Slice(res.Payments, func(i, j int) bool {
		a, b := res.Payments[i], res.Payments[j]
		if exact[a.Key] != exact[b.Key] {
			return exact[a.Key]
		}
		return a.PaidAt.After(b.PaidAt)
	})
	cut := func(n int) int {
		if n > SearchLimit {
			return SearchLimit
		}
		return n
	}
	res.Customers, res.Accounts = res.Customers[:cut(len(res.Customers))], res.Accounts[:cut(len(res.Accounts))]
	res.Equipment, res.Payments = res.Equipment[:cut(len(res.Equipment))], res.Payments[:cut(len(res.Payments))]
	res.Buildings = res.Buildings[:cut(len(res.Buildings))]
	res.Apartments = res.Apartments[:cut(len(res.Apartments))]
	return res
}

func openAccounts(d *store.Data) []store.Account { return d.AccountList() }
