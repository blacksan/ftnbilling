package store

import (
	"fmt"
	"sort"
	"strings"
)

// Validate lists, in plain words, everything in d that a database would
// reject (duplicate codes, duplicate MACs inside a building, dangling
// references, values outside a closed set). An empty list means the data can
// be imported. It never changes d.
func Validate(d *Data) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }
	site := func(id string) string {
		if s, ok := d.Sites[id]; ok {
			return s.Name
		}
		return "unknown building " + id
	}
	// Duplicate account codes (open and closed accounts share the code space).
	codes := map[string][]Account{}
	all := make([]Account, 0, len(d.Accounts)+len(d.ClosedAccounts))
	for _, a := range d.Accounts {
		all = append(all, a)
	}
	for _, a := range d.ClosedAccounts {
		all = append(all, a)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	for _, a := range all {
		if a.Code != "" {
			codes[a.Code] = append(codes[a.Code], a)
		}
		if _, ok := d.Sites[a.SiteID]; !ok {
			add("Flat %q (code %s) belongs to a building that does not exist (%q).", a.Flat, a.Code, a.SiteID)
		}
		if a.PackageID != "" {
			if _, ok := d.Packages[a.PackageID]; !ok {
				add("Flat %q in %s has a package that does not exist (%q).", a.Flat, site(a.SiteID), a.PackageID)
			}
		}
		if a.CustomerID != "" {
			if _, ok := d.Customers[a.CustomerID]; !ok {
				add("Flat %q in %s points at a customer that does not exist.", a.Flat, site(a.SiteID))
			}
		}
	}
	var dup []string
	for c, as := range codes {
		if len(as) > 1 {
			var where []string
			for _, a := range as {
				where = append(where, fmt.Sprintf("%s in %s", a.Flat, site(a.SiteID)))
			}
			dup = append(dup, fmt.Sprintf("The account code %s is used by more than one flat (%s). Codes must be unique; change one of them.", c, strings.Join(where, " and ")))
		}
	}
	sort.Strings(dup)
	out = append(out, dup...)
	// Duplicate MAC among open accounts, across all buildings. Accounts
	// flagged MACConflict are excluded (see Conflicts).
	macs := map[string][]Account{}
	ips := map[string][]Account{}
	for _, a := range d.Accounts {
		if a.MACConflict {
			continue
		}
		if a.MAC != "" {
			if !macShape.MatchString(strings.ToUpper(a.MAC)) {
				add("The ONT MAC %q of flat %q in %s is not written like AA:BB:CC:DD:EE:FF.", a.MAC, a.Flat, site(a.SiteID))
			}
			k := strings.ToUpper(a.MAC)
			macs[k] = append(macs[k], a)
		}
		if a.IP != "" {
			k := a.SiteID + "|" + a.IP
			ips[k] = append(ips[k], a)
		}
	}
	var dm []string
	for _, as := range macs {
		if len(as) > 1 {
			var flats []string
			for _, a := range as {
				flats = append(flats, a.Flat+" in "+site(a.SiteID))
			}
			sort.Strings(flats)
			dm = append(dm, fmt.Sprintf("The ONT MAC %s is on more than one open flat (%s). A MAC can belong to only one open flat in all buildings.", as[0].MAC, strings.Join(flats, " and ")))
		}
	}
	for _, as := range ips {
		if len(as) > 1 {
			var flats []string
			for _, a := range as {
				flats = append(flats, a.Flat)
			}
			sort.Strings(flats)
			dm = append(dm, fmt.Sprintf("The IP address %s is on more than one open flat in %s (%s).", as[0].IP, site(as[0].SiteID), strings.Join(flats, " and ")))
		}
	}
	sort.Strings(dm)
	out = append(out, dm...)
	// Buildings.
	tun := map[string]string{}
	for _, s := range d.Sites {
		if s.ID == "" {
			add("A building has no id.")
		}
		if s.Mode != SiteModeWG && s.Mode != SiteModeEnv {
			add("Building %q has mode %q; it must be %q or %q.", s.Name, s.Mode, SiteModeWG, SiteModeEnv)
		}
		if !s.Removed && s.TunnelIP != "" {
			if o, ok := tun[s.TunnelIP]; ok {
				add("Buildings %q and %q have the same tunnel address %s.", o, s.Name, s.TunnelIP)
			}
			tun[s.TunnelIP] = s.Name
		}
		if s.Lat < -90 || s.Lat > 90 || s.Lng < -180 || s.Lng > 180 {
			add("Building %q has a GPS position outside the valid range.", s.Name)
		}
	}
	// Ledger.
	keys := make([]string, 0, len(d.Payments))
	for k := range d.Payments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := d.Payments[k]
		if p.Key == "" || p.Key != strings.ToUpper(p.Key) {
			add("Payment %q: the transaction code must be non-empty capital letters and digits.", p.Key)
		}
		if k != p.Key {
			add("Payment %q is filed under a different key %q.", p.Key, k)
		}
		switch p.Status {
		case PayApplied, PayUnmatched, PayRefunded, PayIgnored:
		default:
			add("Payment %s has an unknown status %q.", p.Key, p.Status)
		}
		if p.SiteID != "" {
			if _, ok := d.Sites[p.SiteID]; !ok {
				add("Payment %s belongs to a building that does not exist.", p.Key)
			}
		}
		if p.AccountID != "" {
			if _, ok := lookupAccount(d, p.AccountID); !ok {
				add("Payment %s points at an account that does not exist.", p.Key)
			}
		}
		if p.PackageID != "" {
			if _, ok := d.Packages[p.PackageID]; !ok {
				add("Payment %s points at a package that does not exist.", p.Key)
			}
		}
	}
	oids := make([]string, 0, len(d.Orders))
	for k := range d.Orders {
		oids = append(oids, k)
	}
	sort.Strings(oids)
	for _, k := range oids {
		o := d.Orders[k]
		switch o.Status {
		case OrderPending, OrderPaid, OrderFailed:
		default:
			add("Order %s has an unknown status %q.", o.ID, o.Status)
		}
		if o.SiteID != "" {
			if _, ok := d.Sites[o.SiteID]; !ok {
				add("Order %s belongs to a building that does not exist.", o.ID)
			}
		}
		if o.PackageID != "" {
			if _, ok := d.Packages[o.PackageID]; !ok {
				add("Order %s points at a package that does not exist.", o.ID)
			}
		}
		if o.AccountID != "" {
			if _, ok := lookupAccount(d, o.AccountID); !ok {
				add("Order %s points at an account that does not exist.", o.ID)
			}
		}
	}
	for _, v := range d.Vouchers {
		if _, ok := d.Packages[v.PackageID]; !ok {
			add("Voucher %s is for a package that does not exist.", v.Code)
		}
		if v.SiteID != "" {
			if _, ok := d.Sites[v.SiteID]; !ok {
				add("Voucher %s is for a building that does not exist.", v.Code)
			}
		}
	}
	for _, dv := range d.Devices {
		if _, ok := d.Sites[dv.SiteID]; !ok {
			add("An unregistered device (%s) belongs to a building that does not exist.", dv.MAC)
		}
	}
	serials := map[string]string{}
	for _, e := range d.Equipment {
		switch e.Kind {
		case KindONT, KindRouter, KindOLT, KindSplitter, KindSwitch, KindOther:
		default:
			add("Equipment %s has an unknown kind %q.", e.ID, e.Kind)
		}
		switch e.Status {
		case EqInStock, EqInstalled, EqFaulty, EqReturned, EqRetired:
		default:
			add("Equipment %s has an unknown status %q.", e.ID, e.Status)
		}
		if e.AccountID != "" && e.Status != EqInstalled {
			add("Equipment %s is assigned to a flat but its status is %q, not installed.", e.ID, e.Status)
		}
		if e.Serial != "" {
			k := e.Kind + "|" + strings.ToUpper(e.Serial)
			if o, ok := serials[k]; ok {
				add("Two %s items have the serial number %s (%s and %s).", e.Kind, e.Serial, o, e.ID)
			}
			serials[k] = e.ID
		}
	}
	for _, c := range d.Contacts {
		switch c.Role {
		case RoleLandlord, RoleCaretaker, RoleAgent, RoleSecurity, RoleOther:
		default:
			add("Contact %q has an unknown role %q.", c.Name, c.Role)
		}
	}
	users := map[string]string{}
	for _, u := range d.AdminUsers {
		switch u.Role {
		case RoleOwner, RoleOperator, RoleViewer:
		default:
			add("User %q has an unknown role %q.", u.Username, u.Role)
		}
		if o, ok := users[strings.ToLower(u.Username)]; ok {
			add("Two users are called %q (%s and %s).", u.Username, o, u.ID)
		}
		users[strings.ToLower(u.Username)] = u.ID
	}
	for _, x := range d.AdminSessions {
		if _, ok := d.AdminUsers[x.UserID]; !ok {
			add("A sign-in session points at a user that does not exist.")
		}
	}
	for _, x := range d.APIDevices {
		if _, ok := d.AdminUsers[x.UserID]; !ok {
			add("An app device (%s) points at a user that does not exist.", x.Name)
		}
	}
	for _, s := range d.Sites {
		switch s.AccessMode {
		case "", AccessHotspot, AccessIPoEObserve, AccessIPoE:
		default:
			add("Building %q has access mode %q.", s.Name, s.AccessMode)
		}
	}
	for _, t := range d.Tenancies {
		if _, ok := d.Customers[t.CustomerID]; !ok {
			add("A tenancy points at a customer that does not exist.")
		}
		if _, ok := lookupAccount(d, t.AccountID); !ok {
			add("A tenancy points at an account that does not exist.")
		}
	}
	return out
}

// Conflicts lists, in plain words, the open accounts flagged MACConflict: a
// MAC used by another open account (in any building) or malformed. They stay
// in the data, are not provisioned and do not block an import; db-check prints
// this list until they are fixed.
func Conflicts(d *Data) []string {
	var out []string
	for _, a := range d.MACConflicts() {
		who := ""
		if o, ok := d.AccountByMAC(a.MAC); ok && o.ID != a.ID {
			who = fmt.Sprintf(", used by flat %s (%s) in %s", o.Flat, o.Code, d.Sites[o.SiteID].Name)
		}
		out = append(out, fmt.Sprintf("Flat %s (%s) in %s has MAC %q%s. Not provisioned until the MAC is fixed.",
			a.Flat, a.Code, d.Sites[a.SiteID].Name, a.MAC, who))
	}
	return out
}
