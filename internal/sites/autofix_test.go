package sites

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func disableDHCP(m *routeros.Mock) {
	for _, r := range m.Table("/ip/dhcp-server") {
		m.SetRow(context.Background(), "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "true"})
	}
}

func TestAutoMaintainAppliesSafeFixes(t *testing.T) {
	f, s, m := checkRig(t)
	ctx := context.Background()
	disableDHCP(m)
	it := checkItem(t, f, s.ID, CheckDHCP)
	if it.Status != StatusFail || it.Fix.Kind != FixAuto || !it.Fix.Automatic || it.Fix.TryNow || it.Auto == nil || it.Auto.State != "pending" {
		t.Fatalf("before: %+v", it)
	}
	out := f.m.AutoMaintain(ctx, s.ID)
	if len(out) != 1 || out[0].ID != CheckDHCP || out[0].Err != "" {
		t.Fatalf("%+v", out)
	}
	it = checkItem(t, f, s.ID, CheckDHCP)
	if it.Status != StatusOK || it.Auto == nil || it.Auto.State != "fixed" || !strings.HasPrefix(it.Auto.Text, "Fixed by the server at ") {
		t.Fatalf("after: %+v", it)
	}
	if !hasEvent(f, s.ID, "setup fix "+CheckDHCP+" by system") {
		t.Fatal("not recorded with the actor system")
	}
	// settled: a second sweep does nothing
	m.ResetWrites()
	if out := f.m.AutoMaintain(ctx, s.ID); len(out) != 0 || m.Writes() != 0 {
		t.Fatalf("%+v %d", out, m.Writes())
	}
}

func TestAutoMaintainCoversTrafficReporting(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.NetflowAddr = "0.0.0.0:2055" })
	f.m.autoFixDelay = time.Hour
	s := f.joined("Block A")
	m := f.startedRouter(s.ID)
	seedHealthy(t, f, s, m)
	m.SeedRow("/ip/address", R{"address": "10.5.50.1/24", "interface": "br-customers"})
	m.SeedOne("/ip/traffic-flow", R{"enabled": "false", "interfaces": "all"})
	if it := checkItem(t, f, s.ID, CheckTrafficFlow); it.Status != StatusWarn || it.Fix.Kind != FixAuto {
		t.Fatalf("%+v", it)
	}
	out := f.m.AutoMaintain(context.Background(), s.ID)
	found := false
	for _, o := range out {
		if o.ID == CheckTrafficFlow && o.Err == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("%+v", out)
	}
	if it := checkItem(t, f, s.ID, CheckTrafficFlow); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	tgt := m.Table("/ip/traffic-flow/target")
	if len(tgt) != 1 || tgt[0]["dst-address"] != ServerTunnelIP(f.m.pfx) {
		t.Fatalf("%v", tgt)
	}
}

func TestAutoMaintainNeverRunsAConfirmFix(t *testing.T) {
	f, s, m := checkRig(t)
	ctx := context.Background()
	m.SeedRow("/interface/vlan", R{"name": "vlan100-customers", "vlan-id": "100", "interface": "sfp-sfpplus1"})
	m.SeedRow("/interface/bridge/port", R{"bridge": "br-customers", "interface": "sfp-sfpplus1"})
	m.SeedRow("/interface/bridge/port", R{"bridge": "br-customers", "interface": "vlan100-customers"})
	m.SeedRow("/interface/bridge/host", R{"on-interface": "sfp-sfpplus1", "mac-address": "AA:BB:CC:00:00:09", "local": "false"})
	f.st.Update(func(d *store.Data) error { x := d.Sites[s.ID]; x.VLANID = 100; d.Sites[s.ID] = x; return nil })
	it := checkItem(t, f, s.ID, CheckOLT)
	if it.Fix.Kind != FixConfirm || it.Fix.Automatic || it.Auto != nil {
		t.Fatalf("%+v", it)
	}
	m.ResetWrites()
	for _, o := range f.m.AutoMaintain(ctx, s.ID) {
		if o.ID == CheckOLT {
			t.Fatalf("a confirm fix was run: %+v", o)
		}
	}
	if portRow(m, "sfp-sfpplus1") == nil {
		t.Fatal("the OLT port was taken out of the bridge without confirmation")
	}
	// and the system actor is refused outright
	if _, err := f.m.applyFix(ctx, s.ID, CheckOLT, false, ActorSystem, false); err == nil {
		t.Fatal("the system ran a confirm fix")
	}
}

func TestAutoMaintainBacksOffAfterAFailure(t *testing.T) {
	f, s, m := checkRig(t)
	ctx := context.Background()
	disableDHCP(m)
	m.SetWriteFail("/ip/dhcp-server", &routeros.APIError{Status: 400, Message: "Bad Request", Detail: "failure: no way"})
	out := f.m.AutoMaintain(ctx, s.ID)
	if len(out) != 1 || !strings.Contains(out[0].Err, "no way") {
		t.Fatalf("%+v", out)
	}
	it := checkItem(t, f, s.ID, CheckDHCP)
	if it.Auto == nil || it.Auto.State != "failed" || !strings.Contains(it.Auto.Text, "no way") || !it.Fix.TryNow || it.Fix.Kind != FixAuto {
		t.Fatalf("%+v", it)
	}
	if out := f.m.AutoMaintain(ctx, s.ID); len(out) != 0 {
		t.Fatalf("tried again inside the backoff: %+v", out)
	}
	f.advance(autoBackoff(1) + time.Second)
	if out := f.m.AutoMaintain(ctx, s.ID); len(out) != 1 || out[0].Err == "" {
		t.Fatalf("second attempt: %+v", out)
	}
	if autoBackoff(2) != 2*autoBackoff(1) || autoBackoff(30) != autoBackoffMax {
		t.Fatal("the wait must double up to its limit")
	}
	m.SetWriteFail("/ip/dhcp-server", nil)
	f.advance(autoBackoff(2) + time.Second)
	if out := f.m.AutoMaintain(ctx, s.ID); len(out) != 1 || out[0].Err != "" {
		t.Fatalf("recovered: %+v", out)
	}
	if it := checkItem(t, f, s.ID, CheckDHCP); it.Status != StatusOK || it.Fix.TryNow || it.Auto.State != "fixed" {
		t.Fatalf("%+v", it)
	}
}

func TestAutoMaintainLeavesANonV5RouterAlone(t *testing.T) {
	f := newFixture(t, nil)
	f.m.autoFixDelay = time.Hour
	s := f.joined("Block A")
	m := f.startedRouter(s.ID)
	m.SeedOne("/system/resource", R{"version": "7.25.1 (stable)"})
	m.SeedRow("/interface/bridge", R{"name": "bridge"})
	m.SeedRow("/ip/dhcp-server", R{"name": "other", "interface": "bridge", "disabled": "true"})
	m.ResetWrites()
	if out := f.m.AutoMaintain(context.Background(), s.ID); len(out) != 0 || m.Writes() != 0 {
		t.Fatalf("%+v writes %d", out, m.Writes())
	}
	f.m.SweepSite(context.Background(), s.ID)
	if m.Writes() != 0 {
		t.Fatalf("%d writes", m.Writes())
	}
	c, _ := f.m.Checks(context.Background(), s.ID, true)
	for _, it := range c.Items {
		if it.Fix.Automatic || it.Auto != nil {
			t.Errorf("%s offers automatic fixing on a router that must be rebuilt: %+v", it.ID, it)
		}
	}
}

func TestChecklistDropsBlankEvidence(t *testing.T) {
	f, s, _ := checkRig(t)
	c := Checks{Layout: LayoutV5, Items: []CheckItem{
		{ID: "a", Evidence: []string{"", "   ", "real line", "\t"}},
		{ID: "b", Evidence: nil},
		{ID: "c", Evidence: []string{" "}},
	}}
	got := f.m.decorate(s.ID, c)
	if len(got.Items[0].Evidence) != 1 || got.Items[0].Evidence[0] != "real line" {
		t.Fatalf("%q", got.Items[0].Evidence)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), `"evidence":null`) || strings.Contains(string(b), `""`+`,`) && strings.Contains(string(b), `"evidence":[""`) {
		t.Fatalf("%s", b)
	}
	for _, it := range got.Items {
		for _, l := range it.Evidence {
			if strings.TrimSpace(l) == "" {
				t.Fatalf("blank evidence kept in %s", it.ID)
			}
		}
	}
	// the cached copy is not changed
	if len(c.Items[0].Evidence) != 4 {
		t.Fatal("the input was modified")
	}
	// and a real pass never holds one
	all, _ := f.m.Checks(context.Background(), s.ID, true)
	for _, it := range all.Items {
		for _, l := range it.Evidence {
			if strings.TrimSpace(l) == "" {
				t.Fatalf("blank evidence in %s", it.ID)
			}
		}
	}
}
