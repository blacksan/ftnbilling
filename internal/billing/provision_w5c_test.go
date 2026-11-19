package billing

import (
	"context"
	"errors"
	"testing"

	"ispbill/internal/store"
)

func TestCreateCustomer(t *testing.T) {
	ctx := context.Background()
	t.Run("ok, with a package and its time", func(t *testing.T) {
		e := newEnv(t)
		e.ipoe(t, store.AccessIPoE)
		pk := e.pkg("1 Week")
		a, rs, err := e.svc.CreateCustomer(ctx, AddCustomerInput{SiteID: e.site, Flat: "Flat 4", Tenant: "Amina", MAC: macA, PackageID: pk.ID, Activate: true, Note: "cash"})
		if err != nil {
			t.Fatal(err)
		}
		if a.PackageID != pk.ID || a.IP == "" || !a.ExpiresAt.After(e.now) || rs.PendingTask {
			t.Fatalf("%+v %+v", a, rs)
		}
		if len(rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "mac-address", macA)) != 1 {
			t.Fatal("the lease must be on the router after the one synchronous attempt")
		}
	})
	t.Run("activate needs a package", func(t *testing.T) {
		e := newEnv(t)
		if _, _, err := e.svc.CreateCustomer(ctx, AddCustomerInput{SiteID: e.site, Flat: "Flat 4", MAC: macA, Activate: true}); err == nil {
			t.Fatal("expected an error")
		}
		if _, ok := e.svc.AccountByMAC(macA); ok {
			t.Fatal("nothing may be left behind")
		}
	})
	t.Run("mac in use names the owner", func(t *testing.T) {
		e := newEnv(t)
		e.ipoe(t, store.AccessIPoE)
		first := e.flat(t, "Flat 1", macA)
		_, _, err := e.svc.CreateCustomer(ctx, AddCustomerInput{SiteID: e.site, Flat: "Flat 2", MAC: macA})
		var dup *DuplicateMACError
		if !errors.As(err, &dup) || dup.Code != first.Code || dup.Flat != "Flat 1" || dup.SiteName != "Block A" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("queued when the router is offline", func(t *testing.T) {
		e := newEnv(t)
		e.ipoe(t, store.AccessIPoE)
		e.rt.SetFail(errors.New("tunnel down"))
		a, rs, err := e.svc.CreateCustomer(ctx, AddCustomerInput{SiteID: e.site, Flat: "Flat 5", MAC: macB2})
		if err != nil {
			t.Fatal(err)
		}
		if a.ID == "" || !rs.PendingTask || rs.InSync {
			t.Fatalf("the account exists and its router work is queued: %+v", rs)
		}
	})
}
