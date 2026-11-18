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
	site := Site{ID: NewSiteID(), Name: "Main building", Mode: SiteModeEnv, HotspotServer: "hotspot1",
		LegacyContainer: true, CreatedAt: now}
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
