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
