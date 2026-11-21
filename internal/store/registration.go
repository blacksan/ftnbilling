package store

import "time"

// Registration statuses.
const (
	RegPending = "pending"
	// RegApproving is the claim an approval holds while it makes the account; DecidedAt is when it was taken.
	// A claim older than a few minutes (a crash in between) goes back to pending.
	RegApproving = "approving"
	RegApproved  = "approved"
	RegRejected  = "rejected"
)

// Registration is a visitor's request, made on the captive page, to become a subscriber. It is pending until the
// owner approves it (which creates the account) or rejects it. Until then the visitor's device stays walled.
//
// The server cannot read the visitor's MAC from the captive page (the router's address is all it sees), so MAC
// is empty unless an operator filled it in; GuessMAC is the one online unregistered device the router had when
// the form arrived, when there was exactly one. It is a hint for the owner, never applied by itself.
type Registration struct {
	ID          string `json:"id"`
	SiteID      string `json:"site_id"`
	ApartmentID string `json:"apartment_id"`
	Unit        string `json:"unit"`
	Name        string `json:"name"`
	Phone       string `json:"phone"` // 2547XXXXXXXX
	Email       string `json:"email,omitempty"`
	MAC         string `json:"mac,omitempty"`
	GuessMAC    string `json:"guess_mac,omitempty"`
	// DeviceName is what the visitor says their phone or laptop is called (the name in its Wi-Fi settings). The
	// owner matches it against the DHCP host names of the candidate devices. Free text, never trusted.
	DeviceName string `json:"device_name,omitempty"`
	// Building is the apartment or building name the visitor typed when the server could not tell which router
	// they are on (two routers behind one public address). Such a registration has no SiteID until the owner
	// assigns it; Building is cleared then.
	Building string `json:"building,omitempty"`
	Status   string `json:"status"`
	// ConsentAt is when the visitor ticked the privacy notice (the form cannot be sent without it).
	ConsentAt time.Time `json:"consent_at,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	DecidedAt time.Time `json:"decided_at,omitempty"`
	DecidedBy string    `json:"decided_by,omitempty"`
	AccountID string    `json:"account_id,omitempty"` // the account made on approval
}

func (x Registration) norm() Registration {
	x.ConsentAt, x.CreatedAt, x.DecidedAt = nt(x.ConsentAt), nt(x.CreatedAt), nt(x.DecidedAt)
	return x
}

// Norm returns the row as it will be stored.
func (x Registration) Norm() Registration { return x.norm() }

// PendingRegistrations lists the pending registrations, oldest first, optionally of one router.
func (d *Data) PendingRegistrations(siteID string) []Registration {
	var out []Registration
	for _, r := range d.Registrations {
		if r.Status == RegPending && (siteID == "" || r.SiteID == siteID) {
			out = append(out, r)
		}
	}
	for i := 1; i < len(out); i++ { // insertion sort: the list is short
		for j := i; j > 0 && (out[j].CreatedAt.Before(out[j-1].CreatedAt) || (out[j].CreatedAt.Equal(out[j-1].CreatedAt) && out[j].ID < out[j-1].ID)); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
