package sites

import (
	"context"
	"errors"
	"ispbill/internal/billing"
	"strings"
	"testing"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// joined makes a joined building with two flats (one paid, one not).
func (f *fixture) joined(name string) store.Site {
	f.t.Helper()
	s := f.create(name)
	tok, _, err := f.m.MintJoinStage(s.ID, StageJoin)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err := f.m.ConsumeJoinStage(tok, mt, "1.2.3.4", false, StageJoin); err != nil {
		f.t.Fatal(err)
	}
	err = f.st.Update(func(d *store.Data) error {
		p := store.Package{ID: "p1", Name: "Monthly 20", Slug: "m20", SpeedMbps: 20, DurationMin: 43200, PriceKES: 2000}
		d.Packages[p.ID] = p
		d.Accounts["a1"] = store.Account{ID: "a1", SiteID: s.ID, Code: "K7Q2MX", Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01", PackageID: "p1", ExpiresAt: f.now.Add(24 * time.Hour), CreatedAt: f.now}
		d.Accounts["a2"] = store.Account{ID: "a2", SiteID: s.ID, Code: "P4R8TN", Flat: "Flat 10A", MAC: "AA:BB:CC:DD:EE:02", CreatedAt: f.now}
		d.Accounts["a3"] = store.Account{ID: "a3", SiteID: s.ID, Code: "ZZZ999", Flat: "Flat 11", MAC: "AA:BB:CC:DD:EE:03", MACConflict: true, CreatedAt: f.now}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.site(s.ID)
}

func (f *fixture) consume(siteID, stage string) (string, error) {
	f.t.Helper()
	tok, _, err := f.m.MintJoinStage(siteID, stage)
	if err != nil {
		f.t.Fatal(err)
	}
	b, _, err := f.m.ConsumeJoinForce(tok, mt, "9.9.9.9", false, stage, true)
	return string(b), err
}

func TestJoinStageIsJoinOnly(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	tok, _, _ := f.m.MintJoin(s.ID)
	b, _, err := f.m.ConsumeJoinStage(tok, mt, "1.2.3.4", false, StageJoin)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	got := f.site(s.ID)
	if !strings.Contains(out, "private-key=") || !strings.Contains(out, got.RESTPass) || !strings.Contains(out, "stage: join") {
		t.Fatal("join stage lacks its keys")
	}
	for _, bad := range []string{"/ip/hotspot/profile", "/ip/dhcp-server add", "/ip/pool", "walled-garden", "/interface/bridge", "traffic-flow"} {
		if strings.Contains(out, bad) {
			t.Errorf("join-only script touches %s", bad)
		}
	}
	// The only fetch is the report to the server.
	if strings.Count(out, "/tool/fetch") != 2 || !strings.Contains(out, "/report\"") {
		t.Error("join-only script may fetch only to POST its report (success path and error path)")
	}
	// The stage's command is its own file.
	if !strings.Contains(out, `/file remove [find where name="ftn-join.rsc"]`) {
		t.Error("self-removal")
	}
}

func TestCutoverStageRendersFromTheDatabaseAndKeepsKeys(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	before := f.site(s.ID)
	f.m.SetAccessMode(s.ID, store.AccessIPoE)
	out, err := f.consume(s.ID, StageCutover)
	if err != nil {
		t.Fatal(err)
	}
	after := f.site(s.ID)
	if after.WGPublicKey != before.WGPublicKey || after.RESTPass != before.RESTPass || after.TunnelIP != before.TunnelIP {
		t.Fatal("the cutover must not touch the router's keys")
	}
	if strings.Contains(out, "private-key") || strings.Contains(out, before.RESTPass) {
		t.Fatal("the cutover script holds no secrets")
	}
	// Flats get fixed addresses in flat order; the flagged one is left out.
	var a1, a2, a3 store.Account
	f.st.View(func(d *store.Data) { a1, a2, a3 = d.Accounts["a1"], d.Accounts["a2"], d.Accounts["a3"] })
	if a1.IP != "10.5.50.60" || a2.IP != "10.5.50.61" || a3.IP == "" {
		t.Fatalf("%q %q %q", a1.IP, a2.IP, a3.IP)
	}
	for _, want := range []string{
		`address=10.5.50.60 mac-address=AA:BB:CC:DD:EE:01 server=ftn-dhcp comment="ftn:acct:K7Q2MX Flat 3B"`,
		`name=ftn-K7Q2MX target=10.5.50.60/32 max-limit=20M/20M`,
		`name=ftn-P4R8TN target=10.5.50.61/32 max-limit=2M/2M`, // no package yet: the walled rate
		`list=ftn-suspended address=10.5.50.61 disabled=yes`,   // ipoe mode: the unpaid flat is walled
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cutover lacks %q", want)
		}
	}
	if strings.Contains(out, "ZZZ999") {
		t.Error("a MAC-flagged flat must not be provisioned")
	}
	// The token was single use.
	tok := "x"
	if _, _, err := f.m.ConsumeJoinStage(tok, mt, "x", false, StageCutover); err != ErrJoinInvalid {
		t.Fatalf("%v", err)
	}
	// Observe mode: nobody is suspended by the script.
	f.m.SetAccessMode(s.ID, store.AccessIPoEObserve)
	out2, err := f.consume(s.ID, StageCutover)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "address-list add list=ftn-suspended") {
		t.Fatal("observe mode must not write suspended entries")
	}
	// Re-running gives the same addresses.
	if !strings.Contains(out2, `address=10.5.50.61 mac-address=AA:BB:CC:DD:EE:02`) {
		t.Fatal("addresses must be stable across runs")
	}
}

func TestCutoverNeedsAJoinedRouterAndKeepsTheTokenWhenItCannotRender(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A") // not joined
	tok, _, _ := f.m.MintJoinStage(s.ID, StageCutover)
	if _, _, err := f.m.ConsumeJoinStage(tok, mt, "x", false, StageCutover); !errors.Is(err, ErrCannotRender) {
		t.Fatalf("%v", err)
	}
	if got := f.site(s.ID); !got.Join.UsedAt.IsZero() {
		t.Fatal("a failed render must not use the token up")
	}
	// No PUBLIC_IP on the server: the walled flats could not reach the payment page.
	f2 := newFixture(t, func(c *config.Config) { c.PublicIP = "" })
	s2 := f2.joined("Block B")
	if _, err := f2.consume(s2.ID, StageCutover); !errors.Is(err, ErrCannotRender) {
		t.Fatalf("%v", err)
	}
	// Unknown stage looks like any bad token.
	if _, _, err := f.m.ConsumeJoinStage(tok, mt, "x", false, "wipe"); err != ErrJoinInvalid {
		t.Fatalf("%v", err)
	}
}

func TestSetAccessModeAndConfirmCutover(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	ctx := context.Background()
	if err := f.m.SetAccessMode(s.ID, "bogus"); !errors.Is(err, store.ErrBadAccessMode) {
		t.Fatalf("%v", err)
	}
	if err := f.m.SetAccessMode(s.ID, store.AccessIPoEObserve); err != nil {
		t.Fatal(err)
	}
	if got := f.site(s.ID).AccessMode; got != store.AccessIPoEObserve {
		t.Fatal(got)
	}
	if err := f.m.SetAccessMode(s.ID, store.AccessIPoEObserve); err != nil {
		t.Fatal(err) // no change is not an error
	}
	// Not cut over: confirming is refused and nothing is removed.
	f.start()
	f.m.Reload(s.ID)
	f.m.Router(s.ID)
	mock := f.mocks[s.ID]
	mock.SeedRow("/system/scheduler", routeros.Row{"name": RollbackSchedule, "interval": "1m"})
	if err := f.m.ConfirmCutover(ctx, s.ID); !errors.Is(err, ErrNotCutOver) {
		t.Fatalf("%v", err)
	}
	if p, ok := f.m.CutoverPending(ctx, s.ID); !ok || !p {
		t.Fatal("timer must still be there")
	}
	// Cut over: ftn-dhcp is enabled. Confirm removes the timer through REST.
	mock.SeedRow("/ip/dhcp-server", routeros.Row{"name": "ftn-dhcp", "disabled": "false"})
	mock.SeedRow("/system/script", routeros.Row{"name": RollbackScript})
	mock.SeedRow("/system/script", routeros.Row{"name": RollbackTick})
	if err := f.m.ConfirmCutover(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.m.CutoverPending(ctx, s.ID); p {
		t.Fatal("timer must be gone")
	}
	scripts := mock.Table("/system/script")
	if len(scripts) != 1 || scripts[0]["name"] != RollbackScript {
		t.Fatalf("the tick script goes, the rollback script stays for a manual rollback: %v", scripts)
	}
	// Confirming again is harmless.
	if err := f.m.ConfirmCutover(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	// ftn-dhcp disabled (rolled back): refused.
	for _, r := range mock.Table("/ip/dhcp-server") {
		mock.SetRow(ctx, "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "true"})
	}
	if err := f.m.ConfirmCutover(ctx, s.ID); !errors.Is(err, ErrNotCutOver) {
		t.Fatalf("%v", err)
	}
	// The audit trail names every step.
	var msgs []string
	f.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.Kind == "audit" {
				msgs = append(msgs, e.Msg)
			}
		}
	})
	if j := strings.Join(msgs, "|"); !strings.Contains(j, "access mode changed") || !strings.Contains(j, "cutover confirmed") {
		t.Fatal(j)
	}
}

func TestTokensAreBoundToTheirStage(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	f.m.SetAccessMode(s.ID, store.AccessIPoE)
	// A cutover token cannot run the join (which rotates the router's keys), and a join token cannot run the cutover.
	cut, _, _ := f.m.MintJoinStage(s.ID, StageCutover)
	if _, _, err := f.m.ConsumeJoinStage(cut, mt, "x", false, StageJoin); err != ErrJoinInvalid {
		t.Fatalf("cutover token as join: %v", err)
	}
	if _, _, err := f.m.ConsumeJoinStage(cut, mt, "x", false, ""); err != ErrJoinInvalid {
		t.Fatalf("cutover token as legacy join: %v", err)
	}
	if _, _, err := f.m.ConsumeJoinStage(cut, mt, "x", false, StageCleanup); err != ErrJoinInvalid {
		t.Fatalf("cutover token as cleanup: %v", err)
	}
	join, _, _ := f.m.MintJoinStage(s.ID, StageJoin)
	if _, _, err := f.m.ConsumeJoinForce(join, mt, "x", false, StageCutover, true); err != ErrJoinInvalid {
		t.Fatalf("join token as cutover: %v", err)
	}
	cut, _, _ = f.m.MintJoinStage(s.ID, StageCutover)
	if _, _, err := f.m.ConsumeJoinForce(cut, mt, "x", false, StageCutover, true); err != nil {
		t.Fatalf("the right stage works: %v", err)
	}
}

// A paying flat whose MAC the router has never shown blocks the cutover unless it is forced.
func TestCutoverRefusesWhenAPayingFlatWasNeverSeen(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	f.m.SetAccessMode(s.ID, store.AccessIPoE)
	f.start()
	f.m.Reload(s.ID)
	f.m.Router(s.ID)
	mock := f.mocks[s.ID]
	tok, _, _ := f.m.MintJoinStage(s.ID, StageCutover)
	_, _, err := f.m.ConsumeJoinStage(tok, mt, "x", false, StageCutover)
	if !errors.Is(err, ErrCannotRender) || !strings.Contains(err.Error(), "Flat 3B") || strings.Contains(err.Error(), "Flat 10A") {
		t.Fatalf("a refusal names the paying flat only: %v", err)
	}
	if got := f.site(s.ID); !got.Join.UsedAt.IsZero() {
		t.Fatal("a refusal keeps the token")
	}
	// The flat shows up in the lease table (any server): the cutover goes ahead.
	mock.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": "aa:bb:cc:dd:ee:01", "address": "10.5.50.23", "status": "bound"})
	if _, _, err := f.m.ConsumeJoinStage(tok, mt, "x", false, StageCutover); err != nil {
		t.Fatal(err)
	}
	// Forcing skips the check.
	f2 := newFixture(t, nil)
	s2 := f2.joined("Block B")
	f2.m.SetAccessMode(s2.ID, store.AccessIPoE)
	tok, _, _ = f2.m.MintJoinStage(s2.ID, StageCutover)
	if _, _, err := f2.m.ConsumeJoinForce(tok, mt, "x", false, StageCutover, true); err != nil {
		t.Fatal(err)
	}
}

func TestObserveCutoverKeepsTheUnregisteredRangeOff(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	f.m.SetAccessMode(s.ID, store.AccessIPoEObserve)
	out, err := f.consume(s.ID, StageCutover)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, `address-list enable [find where list="ftn-unreg"`) {
		t.Fatal("observe must not wall the unregistered range")
	}
	f.m.SetAccessMode(s.ID, store.AccessIPoE)
	out, err = f.consume(s.ID, StageCutover)
	if err != nil || !strings.Contains(out, `address-list enable [find where list="ftn-unreg"`) {
		t.Fatalf("ipoe walls it in the switch: %v", err)
	}
}

func TestSetAccessModeRefusesHotspotWhileTheRouterRunsFtnDHCP(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	f.start()
	f.m.SetAccessMode(s.ID, store.AccessIPoE)
	f.m.Reload(s.ID)
	f.m.Router(s.ID)
	mock := f.mocks[s.ID]
	mock.SeedRow("/ip/dhcp-server", routeros.Row{"name": "ftn-dhcp", "disabled": "false"})
	if err := f.m.SetAccessMode(s.ID, store.AccessHotspot); !errors.Is(err, ErrRouterIsIPoE) {
		t.Fatalf("%v", err)
	}
	if f.site(s.ID).AccessMode != store.AccessIPoE {
		t.Fatal("mode changed")
	}
	// Observe and ipoe are fine either way; hotspot is fine once ftn-dhcp is off.
	for _, r := range mock.Table("/ip/dhcp-server") {
		mock.SetRow(context.Background(), "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "true"})
	}
	if err := f.m.SetAccessMode(s.ID, store.AccessHotspot); err != nil {
		t.Fatal(err)
	}
}

func TestJoinCommandForce(t *testing.T) {
	got := JoinCommandForce("https://b.example", "T", true, StageCutover)
	if !strings.Contains(got, `/j/T?stage=cutover&force=1"`) {
		t.Fatal(got)
	}
}

func (f *fixture) startedRouter(siteID string) *routeros.Mock {
	f.t.Helper()
	f.start()
	f.m.Reload(siteID)
	f.m.Router(siteID)
	return f.mocks[siteID]
}

// After Confirm cutover the server applies changes; the cutover cannot be re-rendered without force.
func TestCutoverAfterConfirmIsRefusedUnlessForced(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	f.m.SetAccessMode(s.ID, store.AccessIPoE)
	mock := f.startedRouter(s.ID)
	mock.SeedRow("/ip/dhcp-server", routeros.Row{"name": "ftn-dhcp", "disabled": "false"})
	if err := f.m.ConfirmCutover(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	tok, _, _ := f.m.MintJoinStage(s.ID, StageCutover)
	_, _, err := f.m.ConsumeJoinForce(tok, mt, "x", false, StageCutover, false)
	if !errors.Is(err, ErrCannotRender) || !strings.Contains(err.Error(), "already cut over") {
		t.Fatalf("%v", err)
	}
	if got := f.site(s.ID); !got.Join.UsedAt.IsZero() {
		t.Fatal("a refusal keeps the token")
	}
	if _, _, err := f.m.ConsumeJoinForce(tok, mt, "x", true, StageCutover, true); err != nil {
		t.Fatalf("forced: %v", err)
	}
	// Going back to hotspot clears the mark.
	for _, r := range mock.Table("/ip/dhcp-server") {
		mock.SetRow(context.Background(), "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "true"})
	}
	if err := f.m.SetAccessMode(s.ID, store.AccessHotspot); err != nil {
		t.Fatal(err)
	}
	f.st.View(func(d *store.Data) {
		if billing.SiteMark(d, s.ID, billing.MarkCutoverConfirmed) {
			t.Fatal("the mark must be cleared")
		}
	})
}

// Devices the server does not know stop the cutover; forcing it renders the script that keeps them.
func TestCutoverRefusesUnmanagedDevices(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	f.m.SetAccessMode(s.ID, store.AccessIPoE)
	mock := f.startedRouter(s.ID)
	// Both paying flats' MACs are present; an OLT management address has an ARP entry and no lease; an AP has a static lease on the old server.
	mock.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": "AA:BB:CC:DD:EE:01", "address": "10.5.50.23", "status": "bound", "dynamic": "true"})
	mock.SeedRow("/ip/arp", routeros.Row{"mac-address": "00:19:3E:00:00:01", "address": "10.5.50.5", "interface": "br-hotspot"})
	mock.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": "00:19:3E:00:00:02", "address": "10.5.50.6", "dynamic": "false", "server": "dhcp-hotspot"})
	mock.SeedRow("/ip/arp", routeros.Row{"mac-address": "00:19:3E:00:00:03", "address": "192.168.99.7", "interface": "ether9"}) // other network: not ours
	got, err := f.m.UnmanagedDevices(context.Background(), s.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	tok, _, _ := f.m.MintJoinStage(s.ID, StageCutover)
	_, _, err = f.m.ConsumeJoinForce(tok, mt, "x", false, StageCutover, false)
	if !errors.Is(err, ErrCannotRender) || !strings.Contains(err.Error(), "00:19:3E:00:00:01") || !strings.Contains(err.Error(), "00:19:3E:00:00:02") || strings.Contains(err.Error(), "00:19:3E:00:00:03") {
		t.Fatalf("%v", err)
	}
	if _, _, err := f.m.ConsumeJoinForce(tok, mt, "x", false, StageCutover, true); err != nil {
		t.Fatalf("forced: %v", err)
	}
}

func TestSetAccessModeFailsClosedWhenTheRouterIsUnreachable(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	f.m.SetAccessMode(s.ID, store.AccessIPoE)
	mock := f.startedRouter(s.ID)
	mock.SetFail(errors.New("timeout"))
	if err := f.m.SetAccessMode(s.ID, store.AccessHotspot); !errors.Is(err, ErrRouterUnknown) {
		t.Fatalf("%v", err)
	}
	if f.site(s.ID).AccessMode != store.AccessIPoE {
		t.Fatal("must stay")
	}
	if err := f.m.SetAccessModeForce(s.ID, store.AccessHotspot, true); err != nil {
		t.Fatalf("override: %v", err)
	}
	// Observe and ipoe never need the router.
	mock.SetFail(errors.New("timeout"))
	if err := f.m.SetAccessMode(s.ID, store.AccessIPoEObserve); err != nil {
		t.Fatal(err)
	}
}

// The cutover's address allocation avoids the router's own static leases (hook set by billing.NewService).
func TestCutoverAvoidsForeignStaticLeaseAddresses(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	f.m.SetAccessMode(s.ID, store.AccessIPoE)
	f.m.SetForeignIPs(func(string) map[string]bool { return map[string]bool{"10.5.50.60": true} })
	if _, err := f.consume(s.ID, StageCutover); err != nil {
		t.Fatal(err)
	}
	f.st.View(func(d *store.Data) {
		if got := d.Accounts["a1"].IP; got != "10.5.50.61" {
			t.Fatalf("the router's own lease holds .60: got %s", got)
		}
	})
}
