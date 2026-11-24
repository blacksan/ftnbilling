package billing

import (
	"errors"
	"testing"
	"time"

	"ispbill/internal/store"
)

const macPossible = "AA:BB:CC:00:11:22"

func (e *env) possibleOLT(t *testing.T, mac string) {
	t.Helper()
	now := time.Now()
	if err := e.svc.st.Update(func(d *store.Data) error {
		d.Devices[store.DeviceKey(e.site, mac)] = store.Device{SiteID: e.site, MAC: mac, Source: store.DeviceSourceOLTMgmt, Port: "ether5", Online: true, FirstSeen: now, LastSeen: now, Hits: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPossibleOLTIsClassifiedCountedAndNeverAttached(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	apt := e.apt(t, "MG One", e.site)
	e.possibleOLT(t, macPossible)
	got := unregByMAC(e)
	c := got[macPossible].Class
	if c.Class != ClassPossibleOLT || c.Label == "" || c.Creatable {
		t.Fatalf("class %+v", c)
	}
	if n := e.svc.UnregisteredOnline(nil); n != 1 {
		t.Fatalf("a possible OLT is a new device to look at: count %d", n)
	}
	_, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt.ID, SiteID: e.site, Unit: "P1", MAC: macPossible})
	if !errors.Is(err, ErrOLTMAC) {
		t.Fatalf("a possible OLT's MAC was accepted for a customer: %v", err)
	}
}

func TestPossibleOLTStaysOnlineWhenTheBridgePassDoesNotSeeIt(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.possibleOLT(t, macPossible)
	e.reconcile(t)
	if !unregByMAC(e)[macPossible].Online {
		t.Fatal("the management-port device was flipped offline by a bridge pass")
	}
}
