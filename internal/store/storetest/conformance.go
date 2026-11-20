package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/store"
)

// Fixture fills a store with one row of everything, every field set, through
// one Update. It returns the time used.
func Fixture(t testing.TB, s *store.Store) time.Time {
	t.Helper()
	at := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
	err := s.Update(func(d *store.Data) error {
		d.Sites["s111111"] = store.Site{ID: "s111111", Name: "Block A", Mode: store.SiteModeWG, Notes: "n", SupportPhone: "254700000001",
			Disabled: true, TunnelIP: "10.99.0.2", WGPublicKey: "pub", RESTUser: "ftn-api", RESTPass: "pw", RESTPort: 8081,
			Join:         store.JoinToken{Hash: "h", ExpiresAt: at, CreatedAt: at, UsedAt: at, UsedFrom: "1.2.3.4"},
			JoinedAt:     at,
			WANInterface: "ether1", WANList: "WAN", OLTInterface: "sfp1", VLANID: 20, CustomerSubnet: "10.20.0.0/24",
			WANCapMbps: 100.5, OLTCapMbps: 1000, FUPTB: 2.5, CustomerPorts: "ether5,ether4", DisabledPorts: "ether6",
			PkgOverrides: map[string]store.PkgOverride{"p1": {Hidden: true, PriceKES: 70}},
			Address:      "Kilimani Rd", Lat: -1.2921, Lng: 36.8219, GoLive: at, AccessMode: store.AccessIPoE, CreatedAt: at}
		d.Sites["s222222"] = store.Site{ID: "s222222", Name: "Env", Mode: store.SiteModeEnv, CreatedAt: at}
		d.Packages["p1"] = store.Package{ID: "p1", Name: "1 Day", Slug: "1-day", DurationMin: 1440, SpeedMbps: 10, PriceKES: 50, Sort: 1}
		d.Packages["p2"] = store.Package{ID: "p2", Name: "Old", Slug: "old", DurationMin: 60, SpeedMbps: 5, PriceKES: 10, Disabled: true, Deleted: true, Sort: 2}
		d.Customers["c1"] = store.Customer{ID: "c1", Name: "Alice", Phone: "254711000001", Phone2: "254711000002", WhatsApp: "254711000003", WAOptOut: true,
			Email: "a@example.com", Notes: "vip", NationalIDEnc: "enc", NationalIDHMAC: "hmac", NationalIDLast4: "1234", AnonymizedAt: at, CreatedAt: at, UpdatedAt: at}
		d.Equipment["e1"] = store.Equipment{ID: "e1", Kind: store.KindONT, Serial: "SN1", MAC: "AA:BB:CC:DD:EE:01", Model: "F601", Firmware: "1.0",
			Status: store.EqInstalled, SiteID: "s111111", AccountID: "a1", Notes: "n", InstalledAt: at, CreatedAt: at, UpdatedAt: at}
		d.Equipment["e2"] = store.Equipment{ID: "e2", Kind: store.KindSplitter, Status: store.EqInStock, CreatedAt: at}
		d.Equipment["e3"] = store.Equipment{ID: "e3", Kind: store.KindRouter, Serial: "sn1", Status: store.EqInstalled, SiteID: "s111111", CreatedAt: at}
		d.Accounts["a1"] = store.Account{ID: "a1", SiteID: "s111111", Code: "ABC234", Flat: "Flat 1", Floor: "2", MAC: "AA:BB:CC:DD:EE:01", Tenant: "Alice",
			Phone: "254711000001", Notes: "n", WhatsApp: "254711000003", WAOptOut: true, PackageID: "p1", ExpiresAt: at, Suspended: true,
			RemindedFor: at, ExpiredFor: at, CustomerID: "c1", ODB: "ODB-1", SplitterID: "e2", SplitterPort: 4, IP: "10.20.0.60", CreatedAt: at}
		// Flagged by a migration: same MAC as a1, kept, not provisioned.
		d.Accounts["a3"] = store.Account{ID: "a3", SiteID: "s222222", Code: "DUP234", Flat: "Flat 3", MAC: "AA:BB:CC:DD:EE:01", MACConflict: true, CreatedAt: at}
		d.ClosedAccounts["a2"] = store.Account{ID: "a2", SiteID: "s111111", Code: "XYZ234", Flat: "Flat 2", CreatedAt: at, ClosedAt: at}
		d.Tenancies["t1"] = store.Tenancy{ID: "t1", CustomerID: "c1", AccountID: "a1", Note: "n", MoveIn: at}
		d.Tenancies["t2"] = store.Tenancy{ID: "t2", CustomerID: "c1", AccountID: "a2", MoveIn: at, MoveOut: at.Add(time.Hour)}
		d.Contacts["k1"] = store.Contact{ID: "k1", SiteID: "s111111", Role: store.RoleCaretaker, Name: "Joe", Phone: "254722000000", Email: "j@example.com", Notes: "n", CreatedAt: at}
		d.Orders["o1"] = store.Order{ID: "o1", SiteID: "s111111", AccountID: "a1", PackageID: "p1", Phone: "254711000001", Method: "mpesa", AmountKES: 50,
			Status: store.OrderPaid, Reason: "r", CheckoutRequestID: "ws_CO_1", Receipt: "ABCDEFGHIJ", VoucherCode: "V1", CreatedAt: at, PaidAt: at}
		d.Vouchers["V1"] = store.Voucher{Code: "V1", SiteID: "s111111", PackageID: "p1", Batch: "b", Used: true, UsedAt: at, UsedBy: "o1", CreatedAt: at}
		d.Vouchers["V2"] = store.Voucher{Code: "V2", PackageID: "p1", Batch: "b", CreatedAt: at}
		d.Payments["ABCDEFGHIJ"] = store.Payment{Key: "ABCDEFGHIJ", SiteID: "s111111", Method: "mpesa-c2b", Status: store.PayApplied, AmountKES: 50, Phone: "254711000001",
			Name: "ALICE", BillRef: "ABC234", AccountID: "a1", CustomerID: "c1", PackageID: "p1", OrderID: "o1", Note: "n", PaidAt: at, CreatedAt: at, ResolvedAt: at}
		d.Payments["UNMATCHED1"] = store.Payment{Key: "UNMATCHED1", Method: "mpesa-c2b", Status: store.PayUnmatched, AmountKES: 75, PaidAt: at, CreatedAt: at}
		d.Devices[store.DeviceKey("s111111", "AA:BB:CC:DD:EE:09")] = store.Device{SiteID: "s111111", MAC: "AA:BB:CC:DD:EE:09", IP: "10.20.0.9", Source: "portal",
			FirstSeen: at, LastSeen: at, Hits: 3, Vendor: "Huawei", HostName: "HUAWEI-ONT", Online: true}
		d.Tasks["task1"] = store.Task{ID: "task1", SiteID: "s111111", AccountID: "a1", Code: "ABC234", Rev: 2, Reason: "r", Attempts: 1, NextTry: at,
			LastErr: "e", CreatedAt: at}
		d.EquipmentEvents["ee1"] = store.EquipmentEvent{ID: "ee1", EquipmentID: "e1", Kind: store.EqEvAssigned, FromStatus: "in_stock", ToStatus: "installed",
			SiteID: "s111111", AccountID: "a1", CustomerID: "c1", Note: "n", Time: at}
		d.Notes["n1"] = store.Note{ID: "n1", CustomerID: "c1", AccountID: "a1", SiteID: "s111111", EquipmentID: "e1", Text: "hello", Time: at}
		d.Settings["wa.token"] = store.Setting{Key: "wa.token", Value: "c2VhbGVk", Enc: true, UpdatedAt: at}
		d.Settings["wa.lang"] = store.Setting{Key: "wa.lang", Value: "sw", UpdatedAt: at}
		d.AddEvent(store.Event{Time: at, SiteID: "s111111", Kind: "info", AccountID: "a1", CustomerID: "c1", EquipmentID: "e1", Msg: "m"})
		d.AddEvent(store.Event{Time: at, Kind: "audit", AccountID: "a1", Msg: "changed", Actor: "brian", IP: "203.0.113.9"})
		d.AdminUsers["u1"] = store.AdminUser{ID: "u1", Username: "Brian", Role: store.RoleOwner, PassHash: "pbkdf2$x", TOTPSecretEnc: "sealed",
			TOTPConfirmedAt: at, TOTPLastStep: 12345, TOTPFails: 3, TOTPFailAt: at, RecoveryCodes: []string{"h1", "h2"}, PassChangedAt: at, CreatedAt: at, UpdatedAt: at}
		d.AdminUsers["u2"] = store.AdminUser{ID: "u2", Username: "viewer", Role: store.RoleViewer, Disabled: true, CreatedAt: at}
		d.AdminSessions["sess1"] = store.AdminSession{ID: "sess1", UserID: "u1", CSRF: "csrf", MFA: true, IP: "203.0.113.9", UserAgent: "UA",
			CreatedAt: at, LastSeenAt: at, ExpiresAt: at.Add(time.Hour)}
		d.APIDevices["dev1"] = store.APIDevice{ID: "dev1", UserID: "u1", InstallID: "inst", Name: "Pixel", Model: "Pixel 9", Platform: "android",
			AppVersion: "1.0", AccessHash: "ah", AccessExpiresAt: at, RefreshHash: "rh", RefreshPrevHash: "rp", RefreshOlder: []string{"o1", "o2"}, RefreshRotatedAt: at,
			RefreshExpiresAt: at, AbsoluteExpiresAt: at, LastIP: "203.0.113.9", CreatedAt: at, LastSeenAt: at, RevokedAt: at, RevokedReason: "reuse"}
		return nil
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return at
}

// Run is the conformance suite: every backend must pass it unchanged.
func Run(t *testing.T) {
	store.Strict = true
	t.Cleanup(func() { store.Strict = false })

	t.Run("PersistenceAcrossReopen", func(t *testing.T) {
		h := New(t)
		Fixture(t, h.Store)
		before := h.Store.Snapshot()
		after := h.Reopen().Snapshot()
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("data changed over a reopen\nbefore events %+v\nafter  events %+v\nbefore site %+v\nafter  site %+v",
				before.Events, after.Events, before.Sites["s111111"], after.Sites["s111111"])
		}
		if a := after.Accounts["a1"]; a.CustomerID != "c1" || a.SplitterID != "e2" || a.SplitterPort != 4 {
			t.Fatalf("account %+v", a)
		}
		if _, ok := after.ClosedAccounts["a2"]; !ok {
			t.Fatal("closed account not restored into ClosedAccounts")
		}
	})

	t.Run("OneMACPerOpenAccountAcrossBuildings", func(t *testing.T) {
		h := New(t)
		at := Fixture(t, h.Store)
		before := h.Store.Snapshot()
		const mac = "AA:BB:CC:DD:EE:01" // owned by a1 in s111111
		try := func(what string, fn func(d *store.Data)) error {
			t.Helper()
			err := h.Store.Update(func(d *store.Data) error { fn(d); return nil })
			if h.Store.Healthy() != nil {
				t.Fatalf("%s: a duplicate must not degrade the store", what)
			}
			return err
		}
		// A new account in another building, in another spelling.
		err := try("create", func(d *store.Data) {
			d.Accounts["n1"] = store.Account{ID: "n1", SiteID: "s222222", Code: "NEW234", Flat: "Flat 9", MAC: "aa-bb-cc-dd-ee-01", CreatedAt: at}
		})
		if !errors.Is(err, store.ErrDuplicateMAC) {
			t.Fatalf("create: %v", err)
		}
		// Changing an existing account onto it.
		err = try("change", func(d *store.Data) {
			a := d.Accounts["a3"]
			a.MACConflict = false // un-flagging it would make a second owner
			d.Accounts["a3"] = a
		})
		if !errors.Is(err, store.ErrDuplicateMAC) {
			t.Fatalf("un-flag: %v", err)
		}
		// Re-opening a closed account whose MAC is taken.
		err = try("reopen", func(d *store.Data) {
			a := d.ClosedAccounts["a2"]
			a.MAC = mac
			a.ClosedAt = time.Time{}
			delete(d.ClosedAccounts, "a2")
			d.Accounts["a2"] = a
		})
		if !errors.Is(err, store.ErrDuplicateMAC) {
			t.Fatalf("reopen: %v", err)
		}
		if got := h.Store.Snapshot(); !reflect.DeepEqual(got, before) {
			t.Fatal("a refused update changed memory")
		}
		if got := h.Reopen().Snapshot(); !reflect.DeepEqual(got, before) {
			t.Fatal("a refused update changed the stored data")
		}
		// Two accounts may swap MACs in one update; a closed account frees its MAC.
		if err := try("swap", func(d *store.Data) {
			a1, a4 := d.Accounts["a1"], store.Account{ID: "a4", SiteID: "s222222", Code: "SWP234", Flat: "Flat 4", MAC: "AA:BB:CC:DD:EE:04", CreatedAt: at}
			a1.MAC, a4.MAC = a4.MAC, a1.MAC
			d.Accounts["a1"], d.Accounts["a4"] = a1, a4
		}); err != nil {
			t.Fatalf("swap: %v", err)
		}
		if err := try("close and reuse", func(d *store.Data) {
			a4 := d.Accounts["a4"] // now holds ...EE:01
			delete(d.Accounts, "a4")
			a4.ClosedAt = at
			d.ClosedAccounts["a4"] = a4
			d.Accounts["n2"] = store.Account{ID: "n2", SiteID: "s222222", Code: "NEW235", Flat: "Flat 10", MAC: mac, CreatedAt: at}
		}); err != nil {
			t.Fatalf("close and reuse: %v", err)
		}
		// Two accounts of one building cannot share an IP.
		err = try("ip", func(d *store.Data) {
			a := d.Accounts["n2"]
			a.SiteID, a.IP = "s111111", "10.20.0.60"
			d.Accounts["n2"] = a
		})
		if !errors.Is(err, store.ErrDuplicateIP) {
			t.Fatalf("duplicate IP: %v", err)
		}
	})

	t.Run("IntegrityRefusalsNeverDegradeTheStore", func(t *testing.T) {
		h := New(t)
		at := Fixture(t, h.Store)
		before := h.Store.Snapshot()
		cases := []struct {
			name string
			fn   func(d *store.Data)
			want error
		}{
			{"equipment MAC", func(d *store.Data) {
				d.Equipment["n1"] = store.Equipment{ID: "n1", Kind: store.KindONT, MAC: "aa-bb-cc-dd-ee-01", Status: store.EqInStock, CreatedAt: at}
			}, store.ErrDuplicateEquipmentMAC},
			{"user name", func(d *store.Data) {
				d.AdminUsers["n1"] = store.AdminUser{ID: "n1", Username: "BRIAN", Role: store.RoleViewer, CreatedAt: at}
			}, store.ErrDuplicateUsername},
			{"access token", func(d *store.Data) {
				d.APIDevices["n1"] = store.APIDevice{ID: "n1", UserID: "u1", AccessHash: "ah", CreatedAt: at}
			}, store.ErrDuplicateToken},
			{"refresh token", func(d *store.Data) {
				d.APIDevices["n1"] = store.APIDevice{ID: "n1", UserID: "u1", RefreshHash: "rh", CreatedAt: at}
			}, store.ErrDuplicateToken},
			{"malformed MAC", func(d *store.Data) {
				d.Accounts["n1"] = store.Account{ID: "n1", SiteID: "s111111", Code: "MAL234", Flat: "Flat 20", MAC: "aa:bb", CreatedAt: at}
			}, store.ErrBadMAC},
			{"access mode", func(d *store.Data) {
				st := d.Sites["s222222"]
				st.AccessMode = "bogus"
				d.Sites["s222222"] = st
			}, store.ErrBadAccessMode},
			{"un-retiring a duplicate", func(d *store.Data) {
				d.Equipment["r1"] = store.Equipment{ID: "r1", Kind: store.KindONT, MAC: "AA:BB:CC:DD:EE:01", Status: store.EqRetired, CreatedAt: at}
			}, nil}, // allowed: retired items may share a MAC (checked below)
		}
		for _, c := range cases {
			for i := 0; i < 5; i++ { // more than the failure count that degrades a store
				err := h.Store.Update(func(d *store.Data) error { c.fn(d); return nil })
				if c.want == nil {
					if err != nil {
						t.Fatalf("%s: %v", c.name, err)
					}
					break
				}
				if !errors.Is(err, c.want) {
					t.Fatalf("%s: got %v, want %v", c.name, err, c.want)
				}
			}
			if err := h.Store.Healthy(); err != nil {
				t.Fatalf("%s: store degraded: %v", c.name, err)
			}
		}
		after := h.Store.Snapshot()
		if len(after.Equipment) != len(before.Equipment)+1 || after.Equipment["r1"].Status != store.EqRetired {
			t.Fatal("only the retired item should have been added")
		}
		delete(after.Equipment, "r1")
		after.EventSeq, before.EventSeq = 0, 0
		if !reflect.DeepEqual(after.Equipment, before.Equipment) || !reflect.DeepEqual(after.Accounts, before.Accounts) ||
			!reflect.DeepEqual(after.AdminUsers, before.AdminUsers) || !reflect.DeepEqual(after.APIDevices, before.APIDevices) {
			t.Fatal("a refused update changed the data")
		}
		// A real write still works afterwards.
		if err := h.Store.Update(func(d *store.Data) error { d.Notes["after"] = store.Note{ID: "after", Text: "t", Time: at}; return nil }); err != nil {
			t.Fatal(err)
		}
		// Un-retiring the duplicate is refused.
		err := h.Store.Update(func(d *store.Data) error {
			e := d.Equipment["r1"]
			e.Status = store.EqInStock
			d.Equipment["r1"] = e
			return nil
		})
		if !errors.Is(err, store.ErrDuplicateEquipmentMAC) {
			t.Fatalf("un-retire: %v", err)
		}
	})

	t.Run("NoChangeWritesNothing", func(t *testing.T) {
		h := New(t)
		Fixture(t, h.Store)
		n := h.Fault.Commits()
		if err := h.Store.Update(func(*store.Data) error { return store.ErrNoChange }); err != nil {
			t.Fatal(err)
		}
		if err := h.Store.Update(func(d *store.Data) error { d.Packages["p1"] = d.Packages["p1"]; return nil }); err != nil {
			t.Fatal(err)
		}
		if h.Fault.Commits() != n {
			t.Fatalf("a no-op update reached the backend (%d -> %d commits)", n, h.Fault.Commits())
		}
	})

	t.Run("RollbackOnErrorPanicAndCommitFailure", func(t *testing.T) {
		h := New(t)
		Fixture(t, h.Store)
		want := h.Store.Snapshot()
		mutate := func(d *store.Data) {
			p := d.Packages["p1"]
			p.PriceKES = 999
			d.Packages["p1"] = p
			c1 := d.Customers["c1"]
			c1.Name = "Changed"
			d.Customers["c1"] = c1
			d.Customers["c9"] = store.Customer{ID: "c9", Name: "New", CreatedAt: time.Now()}
			d.AddEvent(store.Event{Kind: "info", Msg: "x"})
		}
		same := func(what string) {
			t.Helper()
			if got := h.Store.Snapshot(); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: memory differs from before", what)
			}
			if got := h.Reopen().Snapshot(); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: stored data differs from before", what)
			}
		}
		boom := errors.New("boom")
		if err := h.Store.Update(func(d *store.Data) error { mutate(d); return boom }); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
		same("fn error")
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("panic swallowed")
				}
			}()
			h.Store.Update(func(d *store.Data) error { mutate(d); panic("kaboom") })
		}()
		same("panic")
		h.Fault.FailNextCommit(ErrInjected)
		err := h.Store.Update(func(d *store.Data) error { mutate(d); return nil })
		if !errors.Is(err, store.ErrPersist) || !errors.Is(err, ErrInjected) {
			t.Fatalf("want ErrPersist wrapping the cause, got %v", err)
		}
		same("commit failure")
		// And it still works afterwards.
		if err := h.Store.Update(func(d *store.Data) error { mutate(d); return nil }); err != nil {
			t.Fatal(err)
		}
		if h.Reopen().Snapshot().Packages["p1"].PriceKES != 999 {
			t.Fatal("later update lost")
		}
	})

	t.Run("ConcurrentUpdates", func(t *testing.T) {
		h := New(t)
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := h.Store.Update(func(d *store.Data) error {
					id := fmt.Sprintf("p%02d", i)
					d.Packages[id] = store.Package{ID: id, Name: id, Slug: id}
					d.AddEvent(store.Event{Kind: "info", Msg: id})
					return nil
				}); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		s := h.Reopen().Snapshot()
		if len(s.Packages) != 20 || s.EventSeq != 20 || len(s.Events) != 20 {
			t.Fatalf("packages %d seq %d events %d", len(s.Packages), s.EventSeq, len(s.Events))
		}
		seen := map[int64]bool{}
		for _, e := range s.Events {
			if seen[e.Seq] || e.Seq < 1 || e.Seq > 20 {
				t.Fatalf("bad sequence %d", e.Seq)
			}
			seen[e.Seq] = true
		}
	})

	t.Run("EventSequenceAndCap", func(t *testing.T) {
		h := New(t)
		for round := 0; round < 2; round++ {
			if err := h.Store.Update(func(d *store.Data) error {
				for i := 0; i < 175; i++ {
					d.AddEvent(store.Event{Kind: "info", Msg: "e", SiteID: "sx"})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		check := func(s *store.Store) {
			t.Helper()
			snap := s.Snapshot()
			if len(snap.Events) != 300 || snap.EventSeq != 350 || snap.Events[0].Seq != 51 || snap.Events[299].Seq != 350 {
				t.Fatalf("memory: %d events, seq %d, first %d", len(snap.Events), snap.EventSeq, snap.Events[0].Seq)
			}
		}
		check(h.Store)
		s := h.Reopen()
		check(s)
		evs, err := s.Events(context.Background(), store.EventQuery{SiteID: "sx", Limit: 1000})
		if err != nil || len(evs) < 300 || evs[0].Seq != 350 || evs[1].Seq != 349 {
			t.Fatalf("Events(): %d rows, err %v", len(evs), err)
		}
		if h.Kind == "postgres" && len(evs) != 350 {
			t.Fatalf("postgres keeps every event, got %d", len(evs))
		}
		page, _ := s.Events(context.Background(), store.EventQuery{BeforeSeq: 100, Limit: 3})
		if h.Kind == "postgres" && (len(page) != 3 || page[0].Seq != 99) {
			t.Fatalf("paging: %+v", page)
		}
		// The sequence continues after a restart.
		s.Update(func(d *store.Data) error { d.AddEvent(store.Event{Kind: "info", Msg: "next"}); return nil })
		if got := h.Reopen().Snapshot().EventSeq; got != 351 {
			t.Fatalf("seq after restart: %d", got)
		}
	})

	t.Run("SettingsDiffAndRollback", func(t *testing.T) {
		h := New(t)
		at := time.Date(2026, 6, 7, 8, 9, 10, 987654321, time.UTC)
		if err := h.Store.Update(func(d *store.Data) error {
			d.Settings["a.one"] = store.Setting{Key: "a.one", Value: "1", UpdatedAt: at}
			d.Settings["a.two"] = store.Setting{Key: "a.two", Value: "sealed", Enc: true, UpdatedAt: at}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		got := h.Reopen().Snapshot().Settings
		if len(got) != 2 || got["a.one"].Value != "1" || got["a.one"].Enc || !got["a.two"].Enc ||
			!got["a.one"].UpdatedAt.Equal(at.Truncate(time.Microsecond)) {
			t.Fatalf("settings not stored: %+v", got)
		}
		want := h.Store.Snapshot()
		boom := errors.New("boom")
		err := h.Store.Update(func(d *store.Data) error {
			d.Settings["a.one"] = store.Setting{Key: "a.one", Value: "changed", UpdatedAt: at}
			delete(d.Settings, "a.two")
			d.Settings["a.three"] = store.Setting{Key: "a.three", Value: "x", UpdatedAt: at}
			return boom
		})
		if !errors.Is(err, boom) || !reflect.DeepEqual(h.Store.Snapshot(), want) {
			t.Fatalf("rollback after error failed: %v", err)
		}
		h.Fault.FailNextCommit(ErrInjected)
		err = h.Store.Update(func(d *store.Data) error { delete(d.Settings, "a.one"); return nil })
		if !errors.Is(err, store.ErrPersist) || !reflect.DeepEqual(h.Store.Snapshot(), want) {
			t.Fatalf("rollback after commit failure failed: %v", err)
		}
		// Update one row and delete another in one commit.
		if err := h.Store.Update(func(d *store.Data) error {
			d.Settings["a.one"] = store.Setting{Key: "a.one", Value: "2", UpdatedAt: at}
			delete(d.Settings, "a.two")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		got = h.Reopen().Snapshot().Settings
		if len(got) != 1 || got["a.one"].Value != "2" {
			t.Fatalf("update+delete not stored: %+v", got)
		}
	})

	t.Run("AccountCloseAndReopen", func(t *testing.T) {
		h := New(t)
		now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
		h.Store.Update(func(d *store.Data) error {
			d.Sites["s1"] = store.Site{ID: "s1", Name: "A", Mode: store.SiteModeEnv, CreatedAt: now}
			d.Accounts["a1"] = store.Account{ID: "a1", SiteID: "s1", Code: "AAA111", Flat: "1", MAC: "AA:BB:CC:DD:EE:01", CreatedAt: now}
			return nil
		})
		if err := h.Store.Update(func(d *store.Data) error {
			a := d.Accounts["a1"]
			delete(d.Accounts, "a1")
			a.ClosedAt = now.Add(time.Hour)
			d.ClosedAccounts["a1"] = a
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		s := h.Reopen().Snapshot()
		if _, open := s.Accounts["a1"]; open || !s.ClosedAccounts["a1"].ClosedAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("close not persisted: %+v", s.ClosedAccounts)
		}
		// A new flat may take the closed one's MAC; the closed one keeps its code.
		if err := h.Store.Update(func(d *store.Data) error {
			d.Accounts["a2"] = store.Account{ID: "a2", SiteID: "s1", Code: "BBB222", Flat: "1", MAC: "AA:BB:CC:DD:EE:01", CreatedAt: now}
			return nil
		}); err != nil {
			t.Fatalf("MAC of a closed account must be reusable: %v", err)
		}
		// Reopen the closed account is refused while the MAC is taken (postgres) or allowed (json has no index);
		// release the MAC first, then reopen.
		h.Store.Update(func(d *store.Data) error { delete(d.Accounts, "a2"); return nil })
		if err := h.Store.Update(func(d *store.Data) error {
			a := d.ClosedAccounts["a1"]
			delete(d.ClosedAccounts, "a1")
			a.ClosedAt = time.Time{}
			d.Accounts["a1"] = a
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		s = h.Reopen().Snapshot()
		if _, open := s.Accounts["a1"]; !open || len(s.ClosedAccounts) != 0 {
			t.Fatal("reopen not persisted")
		}
	})

	t.Run("TimesAndZeroTime", func(t *testing.T) {
		h := New(t)
		loc := time.FixedZone("EAT", 3*3600)
		in := time.Date(2026, 1, 2, 3, 4, 5, 987654321, loc)
		h.Store.Update(func(d *store.Data) error {
			d.Sites["s1"] = store.Site{ID: "s1", Name: "A", Mode: store.SiteModeEnv, CreatedAt: in}
			d.Accounts["a1"] = store.Account{ID: "a1", SiteID: "s1", Flat: "1", CreatedAt: in, ExpiresAt: in} // other times zero
			return nil
		})
		for _, s := range []*store.Store{h.Store, h.Reopen()} {
			s.View(func(d *store.Data) {
				a := d.Accounts["a1"]
				want := in.UTC().Truncate(time.Microsecond)
				if !a.CreatedAt.Equal(want) || a.CreatedAt.Nanosecond() != 987654000 || a.CreatedAt.Location() != time.UTC {
					t.Fatalf("time %v want %v", a.CreatedAt, want)
				}
				if !a.RemindedFor.IsZero() || !a.ClosedAt.IsZero() || !d.Sites["s1"].GoLive.IsZero() || !d.Sites["s1"].Join.UsedAt.IsZero() {
					t.Fatal("zero time did not stay zero")
				}
			})
		}
	})

	t.Run("StrictPanicsOnNoChangeAfterMutation", func(t *testing.T) {
		h := New(t)
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "ErrNoChange") {
				t.Fatalf("want strict panic, got %v", r)
			}
			if len(h.Store.Snapshot().Packages) != 0 {
				t.Fatal("mutation survived the strict panic")
			}
		}()
		h.Store.Update(func(d *store.Data) error { d.Packages["p"] = store.Package{ID: "p"}; return store.ErrNoChange })
	})

	t.Run("DeletesPersist", func(t *testing.T) {
		h := New(t)
		Fixture(t, h.Store)
		if err := h.Store.Update(func(d *store.Data) error {
			delete(d.Devices, store.DeviceKey("s111111", "AA:BB:CC:DD:EE:09"))
			delete(d.Tasks, "task1")
			delete(d.Vouchers, "V2")
			delete(d.Contacts, "k1")
			delete(d.Notes, "n1")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		s := h.Reopen().Snapshot()
		if len(s.Devices) != 0 || len(s.Tasks) != 0 || len(s.Vouchers) != 1 || len(s.Contacts) != 0 || len(s.Notes) != 0 {
			t.Fatalf("deletes not persisted: %d %d %d %d %d", len(s.Devices), len(s.Tasks), len(s.Vouchers), len(s.Contacts), len(s.Notes))
		}
	})

	t.Run("AmbiguousCommitReloads", func(t *testing.T) {
		for _, committed := range []bool{false, true} {
			h := New(t)
			h.Store.Update(func(d *store.Data) error { d.Packages["p1"] = store.Package{ID: "p1", Name: "old"}; return nil })
			h.Fault.AmbiguousNextCommit(committed)
			err := h.Store.Update(func(d *store.Data) error { d.Packages["p1"] = store.Package{ID: "p1", Name: "new"}; return nil })
			if !errors.Is(err, store.ErrPersist) {
				t.Fatalf("committed=%v: got %v", committed, err)
			}
			want := "old"
			if committed {
				want = "new" // the reload found out the commit had really happened
			}
			if got := h.Store.Snapshot().Packages["p1"].Name; got != want {
				t.Fatalf("committed=%v: memory has %q, want %q (it must match the database)", committed, got, want)
			}
			if got := h.Reopen().Snapshot().Packages["p1"].Name; got != want {
				t.Fatalf("committed=%v: stored %q", committed, got)
			}
			if h.Store.Healthy() != nil {
				t.Fatal("should be healthy")
			}
		}
	})

	t.Run("DegradedAndRecovery", func(t *testing.T) {
		restore := store.SetReloadInterval(10 * time.Millisecond)
		defer restore()
		h := New(t)
		h.Store.Update(func(d *store.Data) error { d.Packages["p1"] = store.Package{ID: "p1", Name: "kept"}; return nil })
		h.Fault.FailLoads(errors.New("db down"))
		h.Fault.AmbiguousNextCommit(false)
		h.Store.Update(func(d *store.Data) error { d.Packages["p1"] = store.Package{ID: "p1", Name: "lost"}; return nil })
		if h.Store.Healthy() == nil {
			t.Fatal("expected degraded")
		}
		if err := h.Store.Update(func(d *store.Data) error { return nil }); !errors.Is(err, store.ErrUnavailable) {
			t.Fatalf("want ErrUnavailable, got %v", err)
		}
		h.Store.View(func(d *store.Data) {
			if d.Packages["p1"].Name != "kept" {
				t.Fatal("reads must keep working while degraded")
			}
		})
		h.Fault.FailLoads(nil)
		deadline := time.Now().Add(5 * time.Second)
		for h.Store.Healthy() != nil && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if err := h.Store.Healthy(); err != nil {
			t.Fatalf("did not recover: %v", err)
		}
		if err := h.Store.Update(func(d *store.Data) error { d.Packages["p2"] = store.Package{ID: "p2"}; return nil }); err != nil {
			t.Fatal(err)
		}
	})
}
