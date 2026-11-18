package store

import (
	"os"
	"path/filepath"
	"testing"
)

const v3Fixture = `{
 "format": 3,
 "sites": {"s111111": {"id":"s111111","name":"Block A","mode":"env","created_at":"2026-01-01T00:00:00Z"}},
 "packages": {"p1": {"id":"p1","name":"1 Day","slug":"1-day","duration_min":1440,"speed_mbps":10,"price_kes":50,"sort":10}},
 "accounts": {
  "a1": {"id":"a1","site_id":"s111111","code":"AAA222","flat":"Flat 1","mac":"AA:BB:CC:DD:EE:01","tenant":"Alice","phone":"254711000001","whatsapp":"254711000001","hs_pass":"x","created_at":"2026-01-02T00:00:00Z"},
  "a2": {"id":"a2","site_id":"s111111","code":"BBB333","flat":"Flat 2","mac":"AA:BB:CC:DD:EE:02","tenant":"","phone":"0711000001","wa_opt_out":true,"hs_pass":"x","created_at":"2026-01-03T00:00:00Z"},
  "a3": {"id":"a3","site_id":"s111111","code":"CCC444","flat":"Flat 3","mac":"AA:BB:CC:DD:EE:03","tenant":"Bob","hs_pass":"x","created_at":"2026-01-04T00:00:00Z"},
  "a4": {"id":"a4","site_id":"s111111","code":"DDD555","flat":"Flat 4","mac":"AA:BB:CC:DD:EE:04","hs_pass":"x","created_at":"2026-01-05T00:00:00Z"}
 },
 "payments": {
  "K1": {"key":"K1","site_id":"s111111","method":"mpesa-c2b","status":"applied","amount_kes":50,"account_id":"a1","paid_at":"2026-01-06T00:00:00Z","created_at":"2026-01-06T00:00:00Z"},
  "K2": {"key":"K2","site_id":"s111111","method":"mpesa-c2b","status":"applied","amount_kes":50,"account_id":"gone","bill_ref":"ZZ9QQQ (deleted: Flat 9)","paid_at":"2026-01-07T00:00:00Z","created_at":"2026-01-07T00:00:00Z"}
 },
 "orders": {"o1": {"id":"o1","site_id":"s111111","account_id":"gone2","package_id":"p1","method":"test","amount_kes":50,"status":"failed","created_at":"2026-01-08T00:00:00Z"}},
 "events": [{"time":"2026-01-03T00:00:00Z","kind":"info","msg":"one"},{"time":"2026-01-04T00:00:00Z","kind":"info","msg":"two"}]
}`

func TestMigrateV3ToV4DerivesCRM(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ispbill.json")
	os.WriteFile(p, []byte(v3Fixture), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.View(func(d *Data) {
		if d.Format != FormatVersion {
			t.Fatalf("format %d", d.Format)
		}
		// a1 and a2 share a phone (07.. and 2547.. forms): one customer, two tenancies.
		if len(d.Customers) != 2 {
			t.Fatalf("customers: %d %+v", len(d.Customers), d.Customers)
		}
		a1, a2, a3, a4 := d.Accounts["a1"], d.Accounts["a2"], d.Accounts["a3"], d.Accounts["a4"]
		if a1.CustomerID == "" || a1.CustomerID != a2.CustomerID || a3.CustomerID == "" || a3.CustomerID == a1.CustomerID || a4.CustomerID != "" {
			t.Fatalf("links: %q %q %q %q", a1.CustomerID, a2.CustomerID, a3.CustomerID, a4.CustomerID)
		}
		c := d.Customers[a1.CustomerID]
		if c.Name != "Alice" || !c.WAOptOut || c.WhatsApp != "254711000001" {
			t.Fatalf("customer %+v", c)
		}
		if len(d.Tenancies) != 3 {
			t.Fatalf("tenancies %d", len(d.Tenancies))
		}
		if ten, ok := d.OpenTenancy("a2"); !ok || ten.CustomerID != a1.CustomerID || !ten.MoveIn.Equal(a2.CreatedAt) {
			t.Fatalf("tenancy %+v", ten)
		}
		// One ONT per account with a MAC, each with a received event.
		if len(d.Equipment) != 4 || len(d.EquipmentEvents) != 4 {
			t.Fatalf("equipment %d events %d", len(d.Equipment), len(d.EquipmentEvents))
		}
		for _, e := range d.Equipment {
			if e.Kind != KindONT || e.Status != EqInstalled || e.AccountID == "" || e.MAC == "" {
				t.Fatalf("ont %+v", e)
			}
		}
		// Stubs for accounts that no longer exist.
		g := d.ClosedAccounts["gone"]
		if g.Code != "ZZ9QQQ" || g.Flat != "Flat 9" || g.ClosedAt.IsZero() || g.SiteID != "s111111" {
			t.Fatalf("stub %+v", g)
		}
		if g2 := d.ClosedAccounts["gone2"]; g2.Code != "" || g2.Flat == "" {
			t.Fatalf("stub2 %+v", g2)
		}
		if d.Payments["K1"].CustomerID != a1.CustomerID || d.Payments["K2"].CustomerID != "" {
			t.Fatal("payment customer back-fill")
		}
		if d.EventSeq != 2 || d.Events[0].Seq != 1 || d.Events[1].Seq != 2 {
			t.Fatalf("event seq %d %+v", d.EventSeq, d.Events)
		}
	})
	if _, err := os.Stat(p + ".v3.bak"); err != nil {
		t.Fatal("no v3 backup")
	}
	if b, _ := os.ReadFile(p + ".v3.bak"); string(b) != v3Fixture {
		t.Fatal("backup differs")
	}
	// Deriving again changes nothing (stable ids, no duplicates).
	before := s.Snapshot()
	s.Update(func(d *Data) error { d.deriveCRM(timeNow()); return nil })
	after := s.Snapshot()
	if len(after.Customers) != len(before.Customers) || len(after.Equipment) != len(before.Equipment) ||
		len(after.Tenancies) != len(before.Tenancies) || len(after.ClosedAccounts) != len(before.ClosedAccounts) {
		t.Fatal("deriveCRM is not idempotent")
	}
	// ReadJSON migrates in memory and never writes.
	dir2 := t.TempDir()
	p2 := filepath.Join(dir2, "x.json")
	os.WriteFile(p2, []byte(v3Fixture), 0o600)
	d, from, err := ReadJSON(p2)
	if err != nil || from != 3 || len(d.Customers) != 2 {
		t.Fatalf("ReadJSON: %v %d", err, from)
	}
	if b, _ := os.ReadFile(p2); string(b) != v3Fixture {
		t.Fatal("ReadJSON modified the file")
	}
	if entries, _ := os.ReadDir(dir2); len(entries) != 1 {
		t.Fatal("ReadJSON left files behind")
	}
}
