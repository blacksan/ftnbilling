package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrDuplicateIP means a change would give one static IP to more than one
// open account in the same building.
var ErrDuplicateIP = errors.New("that IP address is already assigned to another account in this building")

// checkAccountInvariants runs inside Update, after the change set is known and
// before it is committed. For every open, non-conflict account the change
// touches it requires that no other open, non-conflict account has the same
// MAC (any building) or the same (building, IP). Duplicates that already
// existed and are not touched by the change are not its business: migrations
// flag those (Account.MACConflict) and db-check lists them.
func checkAccountInvariants(d *Data, cs *ChangeSet) error {
	for _, a := range cs.Accounts.Upsert {
		if a.MAC != "" && !a.MACConflict && !macShape.MatchString(a.MAC) {
			return fmt.Errorf("%w: %q on flat %s", ErrBadMAC, a.MAC, a.Flat)
		}
	}
	if cs.Accounts.Empty() {
		return nil
	}
	macs, ips := map[string]bool{}, map[string]bool{}
	for _, a := range cs.Accounts.Upsert {
		if !a.ClosedAt.IsZero() || a.MACConflict {
			continue
		}
		if a.MAC != "" {
			macs[strings.ToUpper(a.MAC)] = true
		}
		if a.IP != "" {
			ips[a.SiteID+"|"+a.IP] = true
		}
	}
	if len(macs) == 0 && len(ips) == 0 {
		return nil
	}
	byMAC, byIP := map[string][]string{}, map[string][]string{}
	for id, a := range d.Accounts {
		if a.MACConflict {
			continue
		}
		if k := strings.ToUpper(a.MAC); macs[k] {
			byMAC[k] = append(byMAC[k], id)
		}
		if k := a.SiteID + "|" + a.IP; a.IP != "" && ips[k] {
			byIP[k] = append(byIP[k], id)
		}
	}
	for _, m := range []map[string][]string{byMAC, byIP} {
		for _, ids := range m {
			sort.Strings(ids)
		}
	}
	keys := func(m map[string][]string) []string {
		var out []string
		for k, ids := range m {
			if len(ids) > 1 {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	}
	if k := keys(byMAC); len(k) > 0 {
		return fmt.Errorf("%w (%s is on accounts %s)", ErrDuplicateMAC, k[0], strings.Join(byMAC[k[0]], " and "))
	}
	if k := keys(byIP); len(k) > 0 {
		return fmt.Errorf("%w (%s is on accounts %s)", ErrDuplicateIP, strings.SplitN(k[0], "|", 2)[1], strings.Join(byIP[k[0]], " and "))
	}
	return nil
}

// checkOtherInvariants covers the rules the Postgres schema enforces outside
// the accounts table, so that the JSON backend refuses the same changes:
// equipment MACs, user names, token hashes and building access modes.
func checkOtherInvariants(d *Data, cs *ChangeSet) error {
	for _, st := range cs.Sites.Upsert {
		switch st.AccessMode {
		case AccessIPoE:
		default:
			return fmt.Errorf("%w: %q", ErrBadAccessMode, st.AccessMode)
		}
	}
	if len(cs.Equipment.Upsert) > 0 {
		macs := map[string]bool{}
		for _, e := range cs.Equipment.Upsert {
			if e.MAC != "" && e.Status != EqRetired {
				macs[strings.ToUpper(e.MAC)] = true
			}
		}
		seen := map[string]string{}
		ids := make([]string, 0, len(d.Equipment))
		for id := range d.Equipment {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			e := d.Equipment[id]
			k := strings.ToUpper(e.MAC)
			if e.MAC == "" || e.Status == EqRetired || !macs[k] {
				continue
			}
			if o, dup := seen[k]; dup {
				return fmt.Errorf("%w (%s is on items %s and %s)", ErrDuplicateEquipmentMAC, e.MAC, o, id)
			}
			seen[k] = id
		}
	}
	if len(cs.AdminUsers.Upsert) > 0 {
		names := map[string]string{}
		ids := make([]string, 0, len(d.AdminUsers))
		for id := range d.AdminUsers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			k := strings.ToLower(d.AdminUsers[id].Username)
			if o, dup := names[k]; dup {
				return fmt.Errorf("%w (%q is on users %s and %s)", ErrDuplicateUsername, d.AdminUsers[id].Username, o, id)
			}
			names[k] = id
		}
	}
	if len(cs.APIDevices.Upsert) > 0 {
		acc, ref := map[string]string{}, map[string]string{}
		for id, x := range d.APIDevices {
			for _, t := range []struct {
				m map[string]string
				h string
			}{{acc, x.AccessHash}, {ref, x.RefreshHash}} {
				if t.h == "" {
					continue
				}
				if o, dup := t.m[t.h]; dup && o != id {
					return fmt.Errorf("%w (devices %s and %s)", ErrDuplicateToken, o, id)
				}
				t.m[t.h] = id
			}
		}
	}
	return nil
}
