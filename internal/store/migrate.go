package store

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"time"
)

var timeNow = time.Now

// migrateRows is the pure part of the v2 -> v3 migration. If the data holds
// any accounts or payments, one env-mode site called "Main building" is
// created and stamped on every row; vouchers keep SiteID "" (valid anywhere).
func (d *Data) migrateRows(now time.Time) {
	d.Format = 3
	if len(d.Accounts) == 0 && len(d.Payments) == 0 {
		// No building is created, so there is nothing to attach devices to:
		// they are portal sightings of unregistered ONTs, with no value
		// without accounts. Keeping them with SiteID "" would fail Validate
		// (and make a Postgres import refuse at start-up).
		d.Devices = map[string]Device{}
		return
	}
	site := Site{ID: NewSiteID(), Name: "Main building", Mode: SiteModeEnv, CreatedAt: now}
	d.Sites[site.ID] = site
	for k, a := range d.Accounts {
		a.SiteID = site.ID
		d.Accounts[k] = a
	}
	for k, o := range d.Orders {
		o.SiteID = site.ID
		d.Orders[k] = o
	}
	for k, p := range d.Payments {
		p.SiteID = site.ID
		d.Payments[k] = p
	}
	for k, t := range d.Tasks {
		t.SiteID = site.ID
		d.Tasks[k] = t
	}
	for i := range d.Events {
		d.Events[i].SiteID = site.ID
	}
	devs := make(map[string]Device, len(d.Devices))
	for mac, dev := range d.Devices {
		if dev.MAC == "" {
			dev.MAC = mac
		}
		dev.SiteID = site.ID
		devs[DeviceKey(site.ID, dev.MAC)] = dev
	}
	d.Devices = devs
}

// migrateV3toV4 upgrades format 3 data in memory: event sequence numbers, then
// customers, tenancies, ONT equipment and stubs for deleted accounts.
func (d *Data) migrateV3toV4(now time.Time) {
	for i := range d.Events {
		if d.Events[i].Seq == 0 {
			d.EventSeq++
			d.Events[i].Seq = d.EventSeq
		} else if d.Events[i].Seq > d.EventSeq {
			d.EventSeq = d.Events[i].Seq
		}
	}
	d.deriveCRM(now)
	d.Format = 4
}

// detID makes a stable 12-hex id so that deriving twice gives the same rows.
func detID(kind, key string) string {
	h := sha256.Sum256([]byte(kind + "|" + key))
	return hex.EncodeToString(h[:6])
}

// phoneKey reduces a phone number to digits in 2547XXXXXXXX form where it can.
func phoneKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	p := b.String()
	switch {
	case len(p) == 10 && p[0] == '0':
		p = "254" + p[1:]
	case len(p) == 9 && (p[0] == '7' || p[0] == '1'):
		p = "254" + p
	}
	return p
}

var deletedRef = regexp.MustCompile(`^([A-Z0-9]+) \(deleted: (.*)\)$`)

// deriveCRM builds the customer side of the data from accounts:
//   - one customer per distinct normalised account phone; otherwise one per
//     account that has a tenant name; vacant accounts get none;
//   - an open tenancy per customer account, starting when the account was made;
//   - one installed ONT per account that has a MAC;
//   - a closed stub account for every payment or order whose account no longer
//     exists, so references stay valid;
//   - Payment.CustomerID back-filled from the account.
//
// It is deterministic and safe to run twice.
func (d *Data) deriveCRM(now time.Time) {
	accts := d.AccountList()
	sort.SliceStable(accts, func(i, j int) bool {
		if !accts[i].CreatedAt.Equal(accts[j].CreatedAt) {
			return accts[i].CreatedAt.Before(accts[j].CreatedAt)
		}
		return accts[i].ID < accts[j].ID
	})
	created := func(t time.Time) time.Time {
		if t.IsZero() {
			return now
		}
		return t
	}
	for _, a := range accts {
		if a.CustomerID == "" {
			key, tenant := phoneKey(a.Phone), strings.TrimSpace(a.Tenant)
			cid := ""
			switch {
			case key != "":
				cid = detID("cust", key)
			case tenant != "":
				cid = detID("cust-acct", a.ID)
			}
			if cid != "" {
				c, ok := d.Customers[cid]
				name := tenant
				if name == "" {
					name = "Tenant of " + a.Flat
				}
				if !ok {
					c = Customer{ID: cid, Name: name, Phone: a.Phone, WhatsApp: a.WhatsApp, WAOptOut: a.WAOptOut,
						CreatedAt: created(a.CreatedAt), UpdatedAt: created(a.CreatedAt)}
				} else {
					if tenant != "" && strings.HasPrefix(c.Name, "Tenant of ") {
						c.Name = tenant
					}
					if c.WhatsApp == "" {
						c.WhatsApp = a.WhatsApp
					}
					c.WAOptOut = c.WAOptOut || a.WAOptOut
					if !a.CreatedAt.IsZero() && a.CreatedAt.Before(c.CreatedAt) {
						c.CreatedAt = a.CreatedAt
					}
				}
				d.Customers[cid] = c
				a.CustomerID = cid
				d.Accounts[a.ID] = a
				tid := detID("ten", a.ID)
				if _, ok := d.Tenancies[tid]; !ok {
					d.Tenancies[tid] = Tenancy{ID: tid, CustomerID: cid, AccountID: a.ID, MoveIn: created(a.CreatedAt)}
				}
			}
		}
		if a.MAC != "" {
			eid := detID("ont", a.ID)
			if _, ok := d.Equipment[eid]; !ok {
				at := created(a.CreatedAt)
				d.Equipment[eid] = Equipment{ID: eid, Kind: KindONT, MAC: a.MAC, Status: EqInstalled, SiteID: a.SiteID,
					AccountID: a.ID, InstalledAt: at, CreatedAt: at, UpdatedAt: at}
				d.EquipmentEvents[detID("eqe", a.ID)] = EquipmentEvent{ID: detID("eqe", a.ID), EquipmentID: eid, Kind: EqEvReceived,
					ToStatus: EqInstalled, SiteID: a.SiteID, AccountID: a.ID, CustomerID: a.CustomerID, Note: "created at import", Time: at}
			}
		}
	}

	// Stubs for deleted accounts that payments and orders still point at.
	type ref struct {
		site, billRef string
		first         time.Time
	}
	missing := map[string]*ref{}
	note := func(id, site, billRef string, at time.Time) {
		if id == "" {
			return
		}
		if _, ok := d.AnyAccount(id); ok {
			return
		}
		r := missing[id]
		if r == nil {
			r = &ref{first: at}
			missing[id] = r
		}
		if r.site == "" || d.Sites[r.site].ID == "" {
			r.site = site
		}
		if r.billRef == "" {
			r.billRef = billRef
		}
		if !at.IsZero() && (r.first.IsZero() || at.Before(r.first)) {
			r.first = at
		}
	}
	for _, p := range d.Payments {
		note(p.AccountID, p.SiteID, p.BillRef, p.CreatedAt)
	}
	for _, o := range d.Orders {
		note(o.AccountID, o.SiteID, "", o.CreatedAt)
	}
	if len(missing) > 0 {
		ids := make([]string, 0, len(missing))
		for id := range missing {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		codes := map[string]bool{}
		for _, a := range d.Accounts {
			codes[a.Code] = true
		}
		for _, a := range d.ClosedAccounts {
			codes[a.Code] = true
		}
		fallback := ""
		for _, s := range d.SiteList() {
			if fallback == "" || s.CreatedAt.Before(d.Sites[fallback].CreatedAt) {
				fallback = s.ID
			}
		}
		for _, id := range ids {
			r := missing[id]
			site := r.site
			if _, ok := d.Sites[site]; !ok || site == "" {
				if fallback == "" {
					fallback = "s000000"
					d.Sites[fallback] = Site{ID: fallback, Name: "Unknown building", Mode: SiteModeEnv, Removed: true, CreatedAt: created(time.Time{})}
				}
				site = fallback
			}
			code, flat := "", "(deleted account)"
			if m := deletedRef.FindStringSubmatch(r.billRef); m != nil {
				code, flat = m[1], m[2]
				if codes[code] {
					code = ""
				}
			}
			if code != "" {
				codes[code] = true
			}
			at := created(r.first)
			d.ClosedAccounts[id] = Account{ID: id, SiteID: site, Code: code, Flat: flat, CreatedAt: at, ClosedAt: at}
		}
	}
	for k, p := range d.Payments {
		if p.CustomerID == "" && p.AccountID != "" {
			if a, ok := d.AnyAccount(p.AccountID); ok && a.CustomerID != "" {
				p.CustomerID = a.CustomerID
				d.Payments[k] = p
			}
		}
	}
}

// migrateV5toV6 upgrades format 5 data in memory: MACs are written in the
// canonical form, and every open account whose MAC is malformed, or already
// used by an open account elsewhere (any building), is flagged MACConflict
// with an audit event. Nothing is dropped or blanked. Among accounts sharing
// a MAC the keeper is the one with a live paid plan, then the oldest, then the
// lowest id; the rest are flagged. (Mirrors migration 0004 in pgstore.)
func (d *Data) migrateV5toV6(now time.Time) {
	for k, a := range d.Accounts {
		if m, ok := CanonMAC(a.MAC); ok {
			a.MAC = m
			d.Accounts[k] = a
		}
	}
	for k, a := range d.ClosedAccounts {
		if m, ok := CanonMAC(a.MAC); ok {
			a.MAC = m
		}
		if a.MAC != "" && !macShape.MatchString(a.MAC) {
			a.MACConflict = true
		}
		d.ClosedAccounts[k] = a
	}
	live := func(a Account) bool { return !a.ExpiresAt.IsZero() && a.ExpiresAt.After(now) && !a.Suspended }
	groups := map[string][]Account{}
	flag := map[string]bool{}
	for _, a := range d.Accounts {
		switch {
		case a.MAC == "":
		case !macShape.MatchString(a.MAC):
			flag[a.ID] = true
		default:
			groups[a.MAC] = append(groups[a.MAC], a)
		}
	}
	for _, as := range groups {
		sort.Slice(as, func(i, j int) bool {
			if li, lj := live(as[i]), live(as[j]); li != lj {
				return li
			}
			if !as[i].CreatedAt.Equal(as[j].CreatedAt) {
				return as[i].CreatedAt.Before(as[j].CreatedAt)
			}
			return as[i].ID < as[j].ID
		})
		for _, a := range as[1:] {
			flag[a.ID] = true
		}
	}
	ids := make([]string, 0, len(flag))
	for id := range flag {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := d.Accounts[id]
		a.MACConflict = true
		d.Accounts[id] = a
		d.AddEvent(Event{Time: now, SiteID: a.SiteID, Kind: "audit", AccountID: a.ID,
			Msg: "MAC conflict found by migration 0004: flat " + a.Flat + " (" + a.Code + ") has MAC " + a.MAC +
				", already used or malformed. It will not be provisioned on IPoE until fixed."})
	}
	d.migrateEquipmentMACs(now)
	d.Format = 6
}

// migrateEquipmentMACs writes equipment MACs in the canonical form and makes
// them unique among live (not retired) items so the unique index can always
// exist. Of the items sharing a MAC the keeper is an installed one, then the
// oldest, then the lowest id. The others are retired and detached from their
// flat, their MAC stays on the row and a note says what they were; each gets
// an audit event. Nothing is deleted. (Mirrors migration 0004 in pgstore.)
func (d *Data) migrateEquipmentMACs(now time.Time) {
	groups := map[string][]Equipment{}
	for k, e := range d.Equipment {
		if m, ok := CanonMAC(e.MAC); ok {
			e.MAC = m
			d.Equipment[k] = e
		}
		if e.MAC != "" && e.Status != EqRetired {
			groups[e.MAC] = append(groups[e.MAC], e)
		}
	}
	var losers []Equipment
	for _, es := range groups {
		sort.Slice(es, func(i, j int) bool {
			if ii, jj := es[i].Status == EqInstalled, es[j].Status == EqInstalled; ii != jj {
				return ii
			}
			if !es[i].CreatedAt.Equal(es[j].CreatedAt) {
				return es[i].CreatedAt.Before(es[j].CreatedAt)
			}
			return es[i].ID < es[j].ID
		})
		losers = append(losers, es[1:]...)
	}
	sort.Slice(losers, func(i, j int) bool { return losers[i].ID < losers[j].ID })
	for _, e := range losers {
		note := "migration 0004: MAC " + e.MAC + " is on another live item; retired (was " + e.Status + ")"
		if e.AccountID != "" {
			note += ", detached from account " + e.AccountID
		}
		if e.Notes != "" {
			note = e.Notes + " | " + note
		}
		acct := e.AccountID
		e.Status, e.AccountID, e.Notes = EqRetired, "", note
		d.Equipment[e.ID] = e
		d.AddEvent(Event{Time: now, SiteID: e.SiteID, Kind: "audit", AccountID: acct, EquipmentID: e.ID,
			Msg: "Equipment MAC conflict found by migration 0004: item " + e.ID + " shares MAC " + e.MAC +
				" with another live item. Retired and detached; the MAC is kept on the row."})
	}
}

var macShape = regexp.MustCompile(`^([0-9A-F]{2}:){5}[0-9A-F]{2}$`)

// obsoleteSiteSettings are the per-router settings of the old provisioning job
// and the hotspot-to-IPoE cutover. They are deleted when data moves to format 7
// (migration 0006 does the same in Postgres).
var obsoleteSiteSettings = []string{"prov_job", "prov_lease", "serving_customers", "cutover_seen_at", "cutover_confirmed_at", "hotspot_bypass_at"}

// migrateV6toV7 upgrades format 6 data in memory: every router becomes IPoE
// and the settings of the old provisioning job are purged. The new Site fields
// stay empty (they mean the defaults).
func (d *Data) migrateV6toV7() {
	for id, s := range d.Sites {
		s.AccessMode = AccessIPoE
		d.Sites[id] = s
	}
	for k := range d.Settings {
		if IsObsoleteSiteSetting(k) {
			delete(d.Settings, k)
		}
	}
}

// IsObsoleteSiteSetting reports whether a settings key is "site.<id>.<name>"
// for one of the settings the bootstrap flow no longer uses.
func IsObsoleteSiteSetting(key string) bool {
	rest, ok := strings.CutPrefix(key, "site.")
	if !ok {
		return false
	}
	_, name, ok := strings.Cut(rest, ".")
	if !ok {
		return false
	}
	for _, o := range obsoleteSiteSettings {
		if name == o {
			return true
		}
	}
	return false
}
