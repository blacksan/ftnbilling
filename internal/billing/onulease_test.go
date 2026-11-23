package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func TestONUMgmtLease(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.st.Update(func(d *store.Data) error {
		s := d.Sites[e.site]
		s.ONUMgmtN = 3
		d.Sites[e.site] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.rt.SeedRow(pathLease, routeros.Row{"address": "10.6.3.20", "mac-address": "aa:bb:cc:00:00:01", "status": "bound"})
	e.rt.SeedRow(pathLease, routeros.Row{"address": "10.6.3.21", "mac-address": "aa:bb:cc:00:00:02", "status": "waiting"})
	e.rt.SeedRow(pathLease, routeros.Row{"address": "192.168.5.9", "mac-address": "aa:bb:cc:00:00:03", "status": "bound"})

	if mac, ok := e.svc.ONUMgmtLease(ctx, e.site, "10.6.3.20"); !ok || mac != "AA:BB:CC:00:00:01" {
		t.Fatalf("%q %v", mac, ok)
	}
	if mac, ok := e.svc.ONUMgmtLease(ctx, e.site, "10.6.3.21"); !ok || mac != "" {
		t.Fatalf("a waiting lease holds nothing: %q %v", mac, ok)
	}
	if mac, ok := e.svc.ONUMgmtLease(ctx, e.site, "192.168.5.9"); !ok || mac != "" {
		t.Fatalf("a customer address is not a management lease: %q %v", mac, ok)
	}
	// a new lease within the same minute is not read for again; a minute later it is
	e.rt.SeedRow(pathLease, routeros.Row{"address": "10.6.3.30", "mac-address": "aa:bb:cc:00:00:04", "status": "bound"})
	e.now = e.now.Add(5 * time.Second)
	if mac, ok := e.svc.ONUMgmtLease(ctx, e.site, "10.6.3.30"); !ok || mac != "" {
		t.Fatalf("answered from the snapshot: %q %v", mac, ok)
	}
	e.now = e.now.Add(11 * time.Minute)
	if mac, ok := e.svc.ONUMgmtLease(ctx, e.site, "10.6.3.30"); !ok || mac != "AA:BB:CC:00:00:04" {
		t.Fatalf("after the snapshot aged: %q %v", mac, ok)
	}
	// an unreachable router, an unknown building or a building without a management subnet: no answer
	e.now = e.now.Add(11 * time.Minute)
	e.rt.SetFail(errors.New("down"))
	if _, ok := e.svc.ONUMgmtLease(ctx, e.site, "10.6.3.20"); ok {
		t.Fatal("answered for a dead router")
	}
	e.rt.SetFail(nil)
	if _, ok := e.svc.ONUMgmtLease(ctx, "nosuch", "10.6.3.20"); ok {
		t.Fatal("unknown building")
	}
	if _, ok := e.svc.ONUMgmtLease(ctx, e.site, "not-an-ip"); ok {
		t.Fatal("bad address")
	}
}
