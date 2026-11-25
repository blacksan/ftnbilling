package web_test

import (
	"encoding/json"
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

func TestAPIONUTrustIsOwnerOnlyAndUnknownListWorks(t *testing.T) {
	forceACS = true
	t.Cleanup(func() { forceACS = false })
	r := newAPIRig(t)
	r.st.Update(func(d *store.Data) error {
		s := d.Sites[r.A.ID]
		s.ONUMgmtN = 4
		d.Sites[r.A.ID] = s
		d.Equipment["eqt"] = store.Equipment{ID: "eqt", Kind: store.KindONT, Serial: "SNT1", MAC: "AA:BB:CC:00:00:10", Status: store.EqInStock, CreatedAt: time.Now()}
		d.ONUs["ot1"] = store.ONU{ID: "ot1", Serial: "SNT1", Manufacturer: "V-SOL", Root: store.RootTR098, SiteID: r.A.ID, MgmtIP: "10.6.4.9", LastInform: time.Now(),
			Sec: store.ONUSec{MACCheck: store.MACWaiting, MACWaitLease: "AA:BB:CC:00:00:90"}}
		return nil
	})
	if v := r.call(r.oper.Access, "POST", "/onus/ot1/trust", nil, nil); v.Status != 403 {
		t.Fatalf("an operator trusted an ONU: %d", v.Status)
	}
	g := r.call(r.view.Access, "GET", "/onus", nil, nil)
	if !strings.Contains(string(g.Raw), `"mac_wait"`) || !strings.Contains(string(g.Raw), "Waiting: management MAC doesn't match the registered ONU yet") {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
	if v := r.call(r.owner.Access, "POST", "/onus/ot1/trust", map[string]any{"mac": "AA:BB:CC:00:00:91"}, nil); v.Status != 409 {
		t.Fatalf("trusted for a MAC the ONU no longer waits with: %d %s", v.Status, v.Raw)
	}
	if v := r.call(r.owner.Access, "POST", "/onus/ot1/trust", map[string]any{"mac": "AA:BB:CC:00:00:90"}, nil); v.Status != 200 {
		t.Fatalf("%d %s", v.Status, v.Raw)
	}
	if v := r.call(r.owner.Access, "POST", "/onus/nope/trust", nil, nil); v.Status != 404 {
		t.Fatalf("%d", v.Status)
	}
	if u := r.call(r.view.Access, "GET", "/onus/unknown", nil, nil); u.Status != 200 || !strings.Contains(string(u.Raw), `"items"`) {
		t.Fatalf("%d %s", u.Status, u.Raw)
	}
}

func TestAPILinkUnknownONUIsOwnerOnlyAndAudited(t *testing.T) {
	forceACS = true
	t.Cleanup(func() { forceACS = false })
	r := newAPIRig(t)
	now := time.Now()
	r.st.Update(func(d *store.Data) error {
		s := d.Sites[r.A.ID]
		s.ONUMgmtN = 4
		d.Sites[r.A.ID] = s
		d.Equipment["eqv"] = store.Equipment{ID: "eqv", Kind: store.KindONT, Serial: "V26051300697", GPONSerial: "VSOL00127E0F", MAC: "68:22:9F:12:7E:10", Model: "V2804N-Z", Status: store.EqInStock, CreatedAt: now}
		b, _ := json.Marshal([]map[string]any{{"serial": "1234568229F127E0F", "oui": "00E0DF", "model": "VSOL V414", "site_id": r.A.ID, "lease_mac": "68:22:9F:12:7E:11",
			"first_seen": now, "last_seen": now, "count": 2}})
		d.Settings["acs.unknown"] = store.Setting{Key: "acs.unknown", Value: string(b), UpdatedAt: now}
		return nil
	})
	g := r.call(r.view.Access, "GET", "/onus/unknown", nil, nil)
	if g.Status != 200 || !strings.Contains(string(g.Raw), `"equipment_id":"eqv"`) || !strings.Contains(string(g.Raw), "in the serial") || !strings.Contains(string(g.Raw), "management MAC 68:22:9F:12:7E:11 near WAN MAC 68:22:9F:12:7E:10") {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
	body := map[string]any{"serial": "1234568229F127E0F", "equipment_id": "eqv"}
	for name, tok := range map[string]string{"viewer": r.view.Access, "operator": r.oper.Access} {
		if v := r.call(tok, "POST", "/onus/unknown/link", body, nil); v.Status != 403 {
			t.Fatalf("%s linked an unknown ONU: %d", name, v.Status)
		}
		if v := r.call(tok, "PUT", "/equipment/eqv/tr069-serial", map[string]any{"tr069_serial": "X"}, nil); v.Status != 403 {
			t.Fatalf("%s set a TR-069 serial: %d", name, v.Status)
		}
	}
	// the owner's own password is needed (step-up, as on the web page)
	if v := r.call(r.owner.Access, "POST", "/onus/unknown/link", body, nil); v.Status != 400 {
		t.Fatalf("linked without the password: %d %s", v.Status, v.Raw)
	}
	if v := r.call(r.owner.Access, "PUT", "/equipment/eqv/tr069-serial", map[string]any{"tr069_serial": "X"}, nil); v.Status != 400 {
		t.Fatalf("set a TR-069 serial without the password: %d %s", v.Status, v.Raw)
	}
	if v := r.call(r.owner.Access, "POST", "/onus/unknown/link", map[string]any{"serial": "1234568229F127E0F", "equipment_id": "eqv", "password": "wrong-password-xx"}, nil); v.Status != 403 {
		t.Fatalf("linked with a wrong password: %d %s", v.Status, v.Raw)
	}
	body["password"] = r.ownerW.Pass
	if v := r.call(r.owner.Access, "POST", "/onus/unknown/link", map[string]any{"serial": "NOTLISTED", "equipment_id": "eqv", "password": r.ownerW.Pass}, nil); v.Status != 409 {
		t.Fatalf("%d %s", v.Status, v.Raw)
	}
	if v := r.call(r.owner.Access, "POST", "/onus/unknown/link", map[string]any{"serial": "1234568229F127E0F", "equipment_id": "nope", "password": r.ownerW.Pass}, nil); v.Status != 404 {
		t.Fatalf("%d %s", v.Status, v.Raw)
	}
	v := r.call(r.owner.Access, "POST", "/onus/unknown/link", body, nil)
	if v.Status != 200 || !strings.Contains(string(v.Raw), `"tr069_serial":"1234568229F127E0F"`) {
		t.Fatalf("%d %s", v.Status, v.Raw)
	}
	if strings.Contains(string(v.Raw), "mgmt_pass\":\"") && !strings.Contains(string(v.Raw), `"mgmt_pass":""`) {
		t.Fatalf("a secret in the answer: %s", v.Raw)
	}
	if u := r.call(r.view.Access, "GET", "/onus/unknown", nil, nil); !strings.Contains(string(u.Raw), `"items":[]`) {
		t.Fatalf("still unknown: %s", u.Raw)
	}
	audited := false
	r.st.View(func(d *store.Data) {
		for _, ev := range d.Events {
			if ev.Kind == "audit" && strings.Contains(ev.Msg, "1234568229F127E0F") {
				audited = true
			}
		}
	})
	if !audited {
		t.Fatal("no audit entry")
	}
	// the owner can clear it again
	if c := r.call(r.owner.Access, "PUT", "/equipment/eqv/tr069-serial", map[string]any{"tr069_serial": "", "password": r.ownerW.Pass}, nil); c.Status != 200 || strings.Contains(string(c.Raw), `"tr069_serial"`) {
		t.Fatalf("%d %s", c.Status, c.Raw)
	}
}
