package billing

import (
	"errors"
	"testing"

	"ispbill/internal/store"
)

func TestFrozenStubsAndGlobalMACLookup(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	a := e.flat(t, "Flat 1", mac1)
	if ps := e.svc.ProvisionStatus(e.site); !ps.InSync || ps.ForeignConflicts == nil {
		t.Fatalf("%+v", ps)
	}
	if st := e.svc.AccountRouterState(a.ID); !st.InSync || st.LeaseStatus != "none" {
		t.Fatalf("%+v", st)
	}
	// A device that is another building's customer is not an unregistered device.
	e.svc.NoteDevice(b, mac1, "10.5.50.9", "portal")
	e.st.View(func(d *store.Data) {
		if len(d.Devices) != 0 {
			t.Fatalf("%v", d.Devices)
		}
	})
	// DuplicateMACError matches both sentinels.
	var err error = &DuplicateMACError{AccountID: "x"}
	if !errors.Is(err, ErrDuplicateMAC) || !errors.Is(err, store.ErrDuplicateMAC) {
		t.Fatal("DuplicateMACError must match ErrDuplicateMAC")
	}
}
