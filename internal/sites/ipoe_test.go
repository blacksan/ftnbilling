package sites

import (
	"context"
	"errors"
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
	tok, _, err := f.m.MintJoin(s.ID)
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
	tok, _, err := f.m.MintJoin(siteID)
	if err != nil {
		f.t.Fatal(err)
	}
	b, _, err := f.m.ConsumeJoinStage(tok, mt, "9.9.9.9", false, stage)
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
	for _, bad := range []string{"/ip/hotspot/profile", "/ip/dhcp-server add", "/ip/pool", "walled-garden", "/tool/fetch", "/interface/bridge"} {
		if strings.Contains(out, bad) {
			t.Errorf("join-only script touches %s", bad)
		}
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
	tok, _, _ := f.m.MintJoin(s.ID)
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
