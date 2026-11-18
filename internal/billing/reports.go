package billing

import (
	"context"
	"sort"
	"strings"
	"time"

	"ispbill/internal/store"
)

// CountsAsRevenue: money that really arrived (vouchers and admin grants are free time).
func CountsAsRevenue(p store.Payment) bool {
	return p.Status == store.PayApplied && p.Method != "voucher" && p.Method != "admin"
}

// PaymentFilter selects ledger rows. Dates are EAT calendar days, inclusive.
type PaymentFilter struct {
	From, To   time.Time // zero = open ended
	Site       func(siteID string) bool
	SiteID     string
	AccountID  string
	CustomerID string
	Method     string
	Status     string
	Q          string // key, phone, payer name, bill ref, account code or flat
}

// PaymentTotals summarises the filtered rows.
type PaymentTotals struct {
	Count     int
	Revenue   int            // real money only
	ByMethod  map[string]int // revenue by method
	MethodSeq []string       // methods sorted by revenue, biggest first
}

// FilterPayments returns every matching payment, newest first, with totals.
func (s *Service) FilterPayments(f PaymentFilter) (rows []store.Payment, tot PaymentTotals) {
	tot.ByMethod = map[string]int{}
	q := strings.ToLower(strings.TrimSpace(f.Q))
	var to time.Time
	if !f.To.IsZero() {
		to = f.To.Add(24 * time.Hour)
	}
	s.st.View(func(d *store.Data) {
		for _, p := range d.PaymentList() {
			if f.Site != nil && !f.Site(p.SiteID) && !(p.SiteID == "" && p.Status == store.PayUnmatched) {
				continue
			}
			if (f.SiteID != "" && p.SiteID != f.SiteID) || (f.AccountID != "" && p.AccountID != f.AccountID) ||
				(f.CustomerID != "" && p.CustomerID != f.CustomerID) || (f.Method != "" && p.Method != f.Method) ||
				(f.Status != "" && p.Status != f.Status) {
				continue
			}
			if (!f.From.IsZero() && p.PaidAt.Before(f.From)) || (!to.IsZero() && !p.PaidAt.Before(to)) {
				continue
			}
			if q != "" {
				a, _ := d.AnyAccount(p.AccountID)
				hay := strings.ToLower(p.Key + "|" + p.Phone + "|" + p.Name + "|" + p.BillRef + "|" + a.Code + "|" + a.Flat)
				if !strings.Contains(hay, q) {
					continue
				}
			}
			rows = append(rows, p)
			tot.Count++
			if CountsAsRevenue(p) {
				tot.Revenue += p.AmountKES
				tot.ByMethod[p.Method] += p.AmountKES
			}
		}
	})
	for m := range tot.ByMethod {
		tot.MethodSeq = append(tot.MethodSeq, m)
	}
	sort.Slice(tot.MethodSeq, func(i, j int) bool {
		a, b := tot.MethodSeq[i], tot.MethodSeq[j]
		if tot.ByMethod[a] != tot.ByMethod[b] {
			return tot.ByMethod[a] > tot.ByMethod[b]
		}
		return a < b
	})
	return rows, tot
}

// MonthRevenue is one row of a building's revenue table.
type MonthRevenue struct {
	Month    string // 2026-11
	Payments int
	KES      int
}

// BuildingReport is the numbers on a building's profile page.
type BuildingReport struct {
	Site                     store.Site
	Contacts                 []store.Contact
	Router, OLT              []store.Equipment
	ByStatus                 map[string]int
	ThisMonth, LastMonth     int
	Months                   []MonthRevenue // last 12, newest first
	Flats, Active, Expired   int
	New, Suspended, Occupied int
	Vacant                   int
}

// Building builds the profile report for a building.
func (s *Service) Building(siteID string) (BuildingReport, bool) {
	var r BuildingReport
	now := s.now()
	ne := now.In(EAT)
	cur := time.Date(ne.Year(), ne.Month(), 1, 0, 0, 0, 0, EAT)
	months := make([]MonthRevenue, 12)
	idx := map[string]int{}
	for i := range months {
		m := cur.AddDate(0, -i, 0)
		months[i].Month = m.Format("2006-01")
		idx[months[i].Month] = i
	}
	r.ByStatus = map[string]int{}
	ok := false
	s.st.View(func(d *store.Data) {
		r.Site, ok = d.Sites[siteID]
		if !ok {
			return
		}
		for _, c := range d.Contacts {
			if c.SiteID == siteID {
				r.Contacts = append(r.Contacts, c)
			}
		}
		for _, e := range d.Equipment {
			if e.SiteID != siteID {
				continue
			}
			r.ByStatus[e.Status]++
			switch {
			case e.Kind == store.KindRouter && e.AccountID == "":
				r.Router = append(r.Router, e)
			case e.Kind == store.KindOLT && e.AccountID == "":
				r.OLT = append(r.OLT, e)
			}
		}
		for _, a := range d.Accounts {
			if a.SiteID != siteID {
				continue
			}
			r.Flats++
			switch AccountState(a, now) {
			case StateActive:
				r.Active++
			case StateExpired:
				r.Expired++
			case StateSuspended:
				r.Suspended++
			default:
				r.New++
			}
			if a.CustomerID != "" {
				r.Occupied++
			} else {
				r.Vacant++
			}
		}
		for _, p := range d.Payments {
			if p.SiteID != siteID || !CountsAsRevenue(p) {
				continue
			}
			if i, in := idx[p.PaidAt.In(EAT).Format("2006-01")]; in {
				months[i].Payments++
				months[i].KES += p.AmountKES
			}
		}
	})
	sort.Slice(r.Contacts, func(i, j int) bool { return r.Contacts[i].CreatedAt.Before(r.Contacts[j].CreatedAt) })
	r.Months = months
	r.ThisMonth, r.LastMonth = months[0].KES, months[1].KES
	return r, ok
}

// TimelineItem is one line of a customer's history.
type TimelineItem struct {
	Time time.Time
	Kind string // note, event, audit, payment, equipment
	Text string
}

// CustomerProfile is the data of a customer's page.
type CustomerProfile struct {
	Customer  store.Customer
	Tenancies []store.Tenancy // newest first
	Accounts  map[string]store.Account
	Payments  []store.Payment
	Total     int
	Equipment []store.Equipment
	Timeline  []TimelineItem // newest first, at most 200
}

// Customer builds a customer's profile. The timeline merges notes, events,
// payments and equipment events from the flats they occupied.
func (s *Service) Customer(ctx context.Context, id string) (CustomerProfile, bool) {
	p := CustomerProfile{Accounts: map[string]store.Account{}}
	ok := false
	var accIDs []string
	s.st.View(func(d *store.Data) {
		p.Customer, ok = d.Customers[id]
		if !ok {
			return
		}
		for _, t := range d.Tenancies {
			if t.CustomerID == id {
				p.Tenancies = append(p.Tenancies, t)
				a, _ := d.AnyAccount(t.AccountID)
				p.Accounts[t.AccountID] = a
				accIDs = append(accIDs, t.AccountID)
			}
		}
		for _, pay := range d.PaymentList() {
			if pay.CustomerID == id {
				p.Payments = append(p.Payments, pay)
				if CountsAsRevenue(pay) {
					p.Total += pay.AmountKES
				}
				p.Timeline = append(p.Timeline, TimelineItem{pay.PaidAt, "payment", "KES " + itoa(pay.AmountKES) + " " + pay.Method + " " + pay.Key + " (" + pay.Status + ")"})
			}
		}
		for _, n := range d.Notes {
			if n.CustomerID == id {
				p.Timeline = append(p.Timeline, TimelineItem{n.Time, "note", n.Text})
			}
		}
		for _, ee := range d.EquipmentEvents {
			if ee.CustomerID == id {
				e := d.Equipment[ee.EquipmentID]
				p.Timeline = append(p.Timeline, TimelineItem{ee.Time, "equipment", strings.TrimSpace(e.Kind + " " + nameOr(e.Serial, e.MAC) + ": " + ee.Kind + " " + ee.Note)})
			}
		}
		for _, e := range d.Equipment {
			if e.AccountID != "" && d.Accounts[e.AccountID].CustomerID == id {
				p.Equipment = append(p.Equipment, e)
			}
		}
	})
	if !ok {
		return p, false
	}
	sort.Slice(p.Tenancies, func(i, j int) bool { return p.Tenancies[i].MoveIn.After(p.Tenancies[j].MoveIn) })
	sort.Slice(p.Equipment, func(i, j int) bool { return p.Equipment[i].Kind < p.Equipment[j].Kind })
	// Events: those tagged with the customer, and those of their flats while they lived there.
	seen := map[int64]bool{}
	add := func(evs []store.Event, window *store.Tenancy) {
		for _, e := range evs {
			if seen[e.Seq] {
				continue
			}
			if window != nil && (e.Time.Before(window.MoveIn) || (!window.MoveOut.IsZero() && e.Time.After(window.MoveOut))) {
				continue
			}
			seen[e.Seq] = true
			kind := "event"
			if e.Kind == "audit" {
				kind = "audit"
			}
			p.Timeline = append(p.Timeline, TimelineItem{e.Time, kind, e.Msg})
		}
	}
	if evs, err := s.st.Events(ctx, store.EventQuery{CustomerID: id, Limit: 200}); err == nil {
		add(evs, nil)
	}
	for _, t := range p.Tenancies {
		t := t
		if evs, err := s.st.Events(ctx, store.EventQuery{AccountID: t.AccountID, Limit: 200}); err == nil {
			add(evs, &t)
		}
	}
	sort.SliceStable(p.Timeline, func(i, j int) bool { return p.Timeline[i].Time.After(p.Timeline[j].Time) })
	if len(p.Timeline) > 200 {
		p.Timeline = p.Timeline[:200]
	}
	return p, true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// CustomerRow is a line of the customer list.
type CustomerRow struct {
	C        store.Customer
	Accounts []store.Account // currently occupied
	Former   bool            // no current tenancy
}

// ListCustomers lists customers for the admin list. state: current, former or all.
func (s *Service) ListCustomers(q, state string, in func(siteID string) bool, siteID string) []CustomerRow {
	q = strings.TrimSpace(q)
	full, digits := phoneForms(q)
	var out []CustomerRow
	s.st.View(func(d *store.Data) {
		for _, c := range d.Customers {
			row := CustomerRow{C: c}
			for _, a := range d.Accounts {
				if a.CustomerID == c.ID {
					row.Accounts = append(row.Accounts, a)
				}
			}
			sort.Slice(row.Accounts, func(i, j int) bool { return store.NaturalLess(row.Accounts[i].Flat, row.Accounts[j].Flat) })
			row.Former = len(row.Accounts) == 0
			if state == "current" && row.Former || state == "former" && !row.Former {
				continue
			}
			if siteID != "" || !row.Former {
				ok := false
				for _, a := range row.Accounts {
					if (siteID == "" || a.SiteID == siteID) && in(a.SiteID) {
						ok = true
					}
				}
				if !ok && (siteID != "" || len(row.Accounts) > 0) {
					continue
				}
			}
			if q != "" && !(store.ContainsFold(c.Name, q) || phoneMatch(c.Phone, full, digits) || phoneMatch(c.Phone2, full, digits) || phoneMatch(c.WhatsApp, full, digits)) {
				continue
			}
			out = append(out, row)
		}
	})
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].C.Name) < strings.ToLower(out[j].C.Name) })
	return out
}
