package store

import "strings"

// SetWhatsApp sets the WhatsApp number and opt-out of an account and keeps the
// linked customer in step (the customer is what the admin edits; the account
// copy is what messaging reads). Every write to Account.WAOptOut must go
// through here or through SetWAOptOut. It reports whether anything changed.
func (d *Data) SetWhatsApp(accountID, number string, optOut bool) bool {
	a, ok := d.Accounts[accountID]
	if !ok {
		return false
	}
	changed := false
	if a.WhatsApp != number || a.WAOptOut != optOut {
		a.WhatsApp, a.WAOptOut = number, optOut
		d.Accounts[accountID] = a
		changed = true
	}
	if c, ok := d.Customers[a.CustomerID]; ok && (c.WhatsApp != number || c.WAOptOut != optOut) {
		c.WhatsApp, c.WAOptOut = number, optOut
		d.Customers[c.ID] = c
		changed = true
	}
	return changed
}

// SetWAOptOut changes only the opt-out of an account and its customer.
func (d *Data) SetWAOptOut(accountID string, optOut bool) bool {
	a, ok := d.Accounts[accountID]
	if !ok {
		return false
	}
	return d.SetWhatsApp(accountID, a.WhatsApp, optOut)
}

// SetWAOptOutByNumber applies an inbound STOP/START to every account (and
// customer) that uses the WhatsApp number.
func (d *Data) SetWAOptOutByNumber(number string, optOut bool) bool {
	changed := false
	for id, a := range d.Accounts {
		if a.WhatsApp == number && d.SetWAOptOut(id, optOut) {
			changed = true
		}
	}
	for id, c := range d.Customers {
		if c.WhatsApp == number && c.WAOptOut != optOut {
			c.WAOptOut = optOut
			d.Customers[id] = c
			changed = true
		}
	}
	return changed
}

// ContainsFold reports whether s contains sub, ignoring case.
func ContainsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
