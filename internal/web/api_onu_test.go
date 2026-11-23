package web_test

import (
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func TestAPIONUWiFiRegisterAndRoles(t *testing.T) {
	forceACS = true
	t.Cleanup(func() { forceACS = false })
	r := newAPIRig(t)
	a := r.acct(t, r.A, "A1", "AA:BB:CC:00:00:01")
	// the register: add, list, duplicate
	add := r.call(r.oper.Access, "POST", "/equipment", map[string]any{"serial": "ZTEGC1", "gpon_serial": "ZTEG-1", "mac": "AA:BB:CC:00:00:C1", "model": "F670L"}, nil)
	if add.Status != 201 {
		t.Fatalf("%d %s", add.Status, add.Raw)
	}
	if dup := r.call(r.oper.Access, "POST", "/equipment", map[string]any{"serial": "ZTEGC1", "mac": "AA:BB:CC:00:00:C2"}, nil); dup.Status != 409 {
		t.Fatalf("duplicate serial: %d %s", dup.Status, dup.Raw)
	}
	if nomac := r.call(r.oper.Access, "POST", "/equipment", map[string]any{"serial": "NOMAC"}, nil); nomac.Status != 400 {
		t.Fatalf("an ONU without MAC: %d", nomac.Status)
	}
	if v := r.call(r.view.Access, "POST", "/equipment", map[string]any{"serial": "X", "mac": "AA:BB:CC:00:00:C3"}, nil); v.Status != 403 {
		t.Fatalf("viewer added: %d", v.Status)
	}
	imp := r.call(r.oper.Access, "POST", "/equipment/import", map[string]any{"lines": "ZTEGC2,AA:BB:CC:00:00:C4,F670L\nbad line,zz"}, nil)
	if imp.Status != 200 || imp.JSON(t)["added"].(float64) != 1 {
		t.Fatalf("%d %s", imp.Status, imp.Raw)
	}
	list := r.call(r.view.Access, "GET", "/equipment?kind=ont", nil, nil)
	if list.Status != 200 || !strings.Contains(string(list.Raw), "ZTEGC2") {
		t.Fatalf("%d %s", list.Status, list.Raw)
	}
	// an ONU reports itself, is linked by hand, and gets a Wi-Fi change
	r.st.Update(func(d *store.Data) error {
		s := d.Sites[r.A.ID]
		s.ONUMgmtN = 4
		d.Sites[r.A.ID] = s
		d.ONUs["o1"] = store.ONU{ID: "o1", Serial: "ZTEGC1", Manufacturer: "ZTE", ProductClass: "F670L", Root: store.RootTR098, SiteID: r.A.ID, MgmtIP: "10.6.4.9",
			MACs: []string{"AA:BB:CC:00:00:C1"}, LastInform: time.Now(), WiFi: []store.ONUWiFi{{Path: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.", AP: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.", Band: "2.4", SSID: "Old"}}}
		return nil
	})
	if l := r.call(r.view.Access, "POST", "/onus/o1/link", map[string]any{"account_id": a.ID}, nil); l.Status != 403 {
		t.Fatalf("viewer linked: %d", l.Status)
	}
	if l := r.call(r.oper.Access, "POST", "/onus/o1/link", map[string]any{"account_id": a.ID}, nil); l.Status != 200 {
		t.Fatalf("%d %s", l.Status, l.Raw)
	}
	bad := r.call(r.oper.Access, "PUT", "/accounts/"+a.ID+"/wifi", map[string]any{"ssid": "", "password": "x"}, nil)
	if bad.Status != 400 {
		t.Fatalf("invalid input: %d %s", bad.Status, bad.Raw)
	}
	ok := r.call(r.oper.Access, "PUT", "/accounts/"+a.ID+"/wifi", map[string]any{"ssid": "My Net", "password": "correct-horse-1"}, nil)
	if ok.Status != 202 || strings.Contains(string(ok.Raw), "correct-horse-1") {
		t.Fatalf("%d %s", ok.Status, ok.Raw)
	}
	r.acs.Wait()
	g := r.call(r.view.Access, "GET", "/accounts/"+a.ID+"/onu", nil, nil)
	if g.Status != 200 || strings.Contains(string(g.Raw), "correct-horse-1") || !strings.Contains(string(g.Raw), "queued") {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
	if w := r.call(r.view.Access, "PUT", "/accounts/"+a.ID+"/wifi", map[string]any{"ssid": "x", "password": "yyyyyyyy"}, nil); w.Status != 403 {
		t.Fatalf("viewer changed Wi-Fi: %d", w.Status)
	}
	if rb := r.call(r.oper.Access, "POST", "/accounts/"+a.ID+"/onu/reboot", nil, nil); rb.Status != 202 {
		t.Fatalf("%d %s", rb.Status, rb.Raw)
	}
	if lst := r.call(r.view.Access, "GET", "/sites/"+r.A.ID+"/onus", nil, nil); lst.Status != 200 || !strings.Contains(string(lst.Raw), "ZTEGC1") {
		t.Fatalf("%d %s", lst.Status, lst.Raw)
	}
	// the audit trail has the change and not the password
	for _, ev := range func() []store.Event {
		var e []store.Event
		r.st.View(func(d *store.Data) { e = append(e, d.Events...) })
		return e
	}() {
		if strings.Contains(ev.Msg, "correct-horse-1") {
			t.Fatal("the password is in the audit trail")
		}
	}
}

func TestAPIONUQuarantineConfirmAndTasks(t *testing.T) {
	forceACS = true
	t.Cleanup(func() { forceACS = false })
	r := newAPIRig(t)
	a := r.acct(t, r.A, "A1", "AA:BB:CC:00:00:01")
	r.st.Update(func(d *store.Data) error {
		s := d.Sites[r.A.ID]
		s.ONUMgmtN = 4
		d.Sites[r.A.ID] = s
		d.ONUs["o9"] = store.ONU{ID: "o9", Serial: "SNQ9", Manufacturer: "V-SOL", Root: store.RootTR098, SiteID: r.A.ID, MgmtIP: "10.6.4.9", AccountID: a.ID, Link: store.LinkManual,
			LastInform: time.Now(), Sec: store.ONUSec{ACSCred: store.CredDevice, MgmtMAC: "AA:BB:CC:00:00:09",
				Quarantine: store.ONUQuarantine{Active: true, Since: time.Now(), LastAt: time.Now(), Count: 1, Reason: "seen from another router", SiteID: r.A.ID, IP: "10.6.4.50", MAC: "AA:BB:CC:00:00:50"}}}
		d.ONUTasks["tq1"] = store.ONUTask{ID: "tq1", ONUID: "o9", SiteID: r.A.ID, AccountID: a.ID, Kind: store.TaskReboot, Status: store.TaskPending, CreatedAt: time.Now()}
		d.ONUTasks["tw1"] = store.ONUTask{ID: "tw1", ONUID: "o9", SiteID: r.A.ID, AccountID: a.ID, Kind: store.TaskWiFi, Status: store.TaskAccepted, CreatedAt: time.Now().Add(-time.Hour), PassLeaf: "KeyPassphrase", AfterReboot: true}
		return nil
	})
	list := r.call(r.view.Access, "GET", "/onus", nil, nil)
	if list.Status != 200 || !strings.Contains(string(list.Raw), `"quarantine"`) || !strings.Contains(string(list.Raw), "seen from another router") {
		t.Fatalf("%d %s", list.Status, list.Raw)
	}
	if v := r.call(r.view.Access, "POST", "/onus/o9/approve", nil, nil); v.Status != 403 {
		t.Fatalf("viewer approved: %d", v.Status)
	}
	// block first: the alert is closed and the place is remembered; approving then has nothing to approve
	if b := r.call(r.oper.Access, "POST", "/onus/o9/block", nil, nil); b.Status != 200 {
		t.Fatalf("%d %s", b.Status, b.Raw)
	}
	if again := r.call(r.oper.Access, "POST", "/onus/o9/approve", nil, nil); again.Status != 409 {
		t.Fatalf("approve without a quarantine: %d %s", again.Status, again.Raw)
	}
	if u := r.call(r.oper.Access, "POST", "/onus/o9/unblock", nil, nil); u.Status != 200 {
		t.Fatalf("%d %s", u.Status, u.Raw)
	}
	// the task view says where each stands
	g := r.call(r.view.Access, "GET", "/accounts/"+a.ID+"/onu", nil, nil)
	if g.Status != 200 || !strings.Contains(string(g.Raw), "applies after the ONU restarts") && !strings.Contains(string(g.Raw), "queued") {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
	// cancel the queued reboot
	if c := r.call(r.oper.Access, "POST", "/onu-tasks/tq1/cancel", nil, nil); c.Status != 200 {
		t.Fatalf("%d %s", c.Status, c.Raw)
	}
	if c := r.call(r.oper.Access, "POST", "/onu-tasks/tq1/cancel", nil, nil); c.Status != 409 {
		t.Fatalf("a task cancelled twice: %d", c.Status)
	}
	// "It works"
	if v := r.call(r.view.Access, "POST", "/accounts/"+a.ID+"/onu/confirm", nil, nil); v.Status != 403 {
		t.Fatalf("viewer confirmed: %d", v.Status)
	}
	cf := r.call(r.oper.Access, "POST", "/accounts/"+a.ID+"/onu/confirm", nil, nil)
	if cf.Status != 200 {
		t.Fatalf("%d %s", cf.Status, cf.Raw)
	}
	r.st.View(func(d *store.Data) {
		if d.ONUTasks["tw1"].Status != store.TaskConfirmed || d.ONUTasks["tw1"].ConfirmedBy == "" {
			t.Fatalf("%+v", d.ONUTasks["tw1"])
		}
	})
	if cf2 := r.call(r.oper.Access, "POST", "/accounts/"+a.ID+"/onu/confirm", nil, nil); cf2.Status != 409 {
		t.Fatalf("confirmed twice: %d", cf2.Status)
	}
	// a MAC the box reported in a session that was not verified is refused (the box has no management MAC now)
	r.st.Update(func(d *store.Data) error {
		o := d.ONUs["o9"]
		o.Sec.MgmtMAC, o.MACs = "", []string{"AA:BB:CC:00:00:C9"}
		d.ONUs["o9"] = o
		return nil
	})
	if m := r.call(r.oper.Access, "POST", "/accounts/"+a.ID+"/onu/use-reported-mac", map[string]any{"mac": "AA:BB:CC:00:00:C9"}, nil); m.Status != 400 {
		t.Fatalf("%d %s", m.Status, m.Raw)
	}
}
