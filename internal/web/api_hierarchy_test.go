package web_test

import (
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func TestAPIOLTTreeClassAndConfirm(t *testing.T) {
	r := newAPIRig(t)
	if err := r.svc.SetDataKey([]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	var aptA string
	r.st.View(func(d *store.Data) { aptA = d.SiteApartmentIDs(r.A.ID)[0] })
	// create: validation, roles
	if v := r.call(r.view.Access, "POST", "/olts", map[string]any{"name": "x", "mac": tOLTMAC, "site_id": r.A.ID}, nil); v.Status != 403 {
		t.Fatalf("viewer created an OLT: %d", v.Status)
	}
	if bad := r.call(r.oper.Access, "POST", "/olts", map[string]any{"mac": tOLTMAC}, nil); bad.Status != 400 {
		t.Fatalf("no name: %d %s", bad.Status, bad.Raw)
	}
	body := map[string]any{"name": "Alpha OLT", "model": "V1600GS-R", "serial": "VS1", "mac": tOLTMAC, "site_id": r.A.ID, "router_port": "ether3", "pon_ports": 4,
		"pon_apartments": map[string]any{"1": []string{aptA}}, "mgmt_ip": "192.168.1.10", "mgmt_user": "admin", "mgmt_pass": "Olt-Secret-99"}
	c := r.call(r.oper.Access, "POST", "/olts", body, nil)
	if c.Status != 201 {
		t.Fatalf("%d %s", c.Status, c.Raw)
	}
	if strings.Contains(string(c.Raw), "Olt-Secret-99") || strings.Contains(string(c.Raw), `"mgmt_pass"`) || !strings.Contains(string(c.Raw), `"has_mgmt_pass":true`) {
		t.Fatalf("the response carries the password: %s", c.Raw)
	}
	id := c.JSON(t)["id"].(string)
	// the sealed value is in no API answer: get, list, equipment list
	for _, p := range []string{"/olts", "/olts/" + id, "/equipment", "/equipment?kind=olt", "/equipment/tree"} {
		g := r.call(r.view.Access, "GET", p, nil, nil)
		if g.Status != 200 || strings.Contains(string(g.Raw), "Olt-Secret-99") || strings.Contains(string(g.Raw), `"mgmt_pass"`) {
			t.Fatalf("GET %s: %d %s", p, g.Status, g.Raw)
		}
	}
	// validation: MAC reuse (409), a PON mapped to a router that does not serve the apartment, bad IP
	if d := r.call(r.oper.Access, "POST", "/olts", map[string]any{"name": "Other", "mac": tOLTMAC, "site_id": r.A.ID}, nil); d.Status != 409 {
		t.Fatalf("duplicate MAC: %d %s", d.Status, d.Raw)
	}
	var aptB string
	r.st.View(func(d *store.Data) { aptB = d.SiteApartmentIDs(r.B.ID)[0] })
	if d := r.call(r.oper.Access, "POST", "/olts", map[string]any{"name": "Other", "mac": "48:21:B8:10:D7:99", "site_id": r.A.ID, "pon_ports": 4, "pon_apartments": map[string]any{"1": []string{aptB}}}, nil); d.Status != 400 {
		t.Fatalf("apartment of another router: %d %s", d.Status, d.Raw)
	}
	if d := r.call(r.oper.Access, "POST", "/olts", map[string]any{"name": "Other", "mac": "48:21:B8:10:D7:98", "site_id": r.A.ID, "mgmt_ip": "nope"}, nil); d.Status != 400 {
		t.Fatalf("bad IP: %d", d.Status)
	}
	// an account MAC is refused for an OLT
	r.acct(t, r.A, "A9", "AA:BB:CC:00:00:09")
	if d := r.call(r.oper.Access, "POST", "/olts", map[string]any{"name": "Other", "mac": "AA:BB:CC:00:00:09", "site_id": r.A.ID}, nil); d.Status != 409 {
		t.Fatalf("a customer's MAC: %d %s", d.Status, d.Raw)
	}
	// an edit keeps the password
	if e := r.call(r.oper.Access, "PUT", "/olts/"+id, map[string]any{"name": "Alpha OLT 2", "model": "V1600GS-R", "mac": tOLTMAC, "site_id": r.A.ID, "router_port": "ether3", "pon_ports": 4}, nil); e.Status != 200 || !strings.Contains(string(e.Raw), `"has_mgmt_pass":true`) {
		t.Fatalf("%d %s", e.Status, e.Raw)
	}
	// ONU placement and the tree
	on := r.call(r.oper.Access, "POST", "/equipment", map[string]any{"serial": "ZTEGC1", "mac": tONUMAC, "model": "F601", "olt_id": id, "pon_port": 1}, nil)
	if on.Status != 201 {
		t.Fatalf("%d %s", on.Status, on.Raw)
	}
	onuID := on.JSON(t)["id"].(string)
	if pl := r.call(r.oper.Access, "PUT", "/equipment/"+onuID+"/placement", map[string]any{"olt_id": id, "pon_port": 9}, nil); pl.Status != 400 {
		t.Fatalf("PON out of range: %d", pl.Status)
	}
	if pl := r.call(r.oper.Access, "PUT", "/equipment/"+onuID+"/placement", map[string]any{"olt_id": id, "pon_port": 1}, nil); pl.Status != 200 || !strings.Contains(string(pl.Raw), `"apartment_ids":["`+aptA+`"]`) {
		t.Fatalf("%d %s", pl.Status, pl.Raw)
	}
	tr := r.call(r.view.Access, "GET", "/equipment/tree", nil, nil).JSON(t)
	routers := tr["routers"].([]any)
	var alpha map[string]any
	for _, x := range routers {
		if m := x.(map[string]any); m["id"] == r.A.ID {
			alpha = m
		}
	}
	olts := alpha["olts"].([]any)
	if len(olts) != 1 || alpha["onu_count"].(float64) != 1 {
		t.Fatalf("tree %v", alpha)
	}
	pons := olts[0].(map[string]any)["pons"].([]any)
	p1 := pons[0].(map[string]any)
	if p1["port"].(float64) != 1 || len(p1["onus"].([]any)) != 1 || len(p1["apartments"].([]any)) != 1 {
		t.Fatalf("pon 1 %v", p1)
	}

	// New devices: the OLT's MAC is classed, the unknown device that only the bridge saw needs confirmation
	now := time.Now()
	r.st.Update(func(d *store.Data) error {
		for mac, port := range map[string]string{tOLTMAC: "ether3", tONUMAC: "ether5", "6C:1F:F7:BB:46:94": "ether5"} {
			d.Devices[store.DeviceKey(r.A.ID, mac)] = store.Device{SiteID: r.A.ID, MAC: mac, Source: "bridge", Port: port, FirstSeen: now, LastSeen: now, Online: true, Hits: 1}
		}
		return nil
	})
	un := r.call(r.view.Access, "GET", "/unregistered?site="+r.A.ID, nil, nil)
	if un.Status != 200 {
		t.Fatalf("%d %s", un.Status, un.Raw)
	}
	kinds := map[string]map[string]any{}
	for _, it := range un.JSON(t)["items"].([]any) {
		m := it.(map[string]any)
		kinds[m["mac"].(string)] = m["class"].(map[string]any)
	}
	if k := kinds[tOLTMAC]; k["kind"] != "olt" || k["attachable"] != false || k["creatable"] != false || k["label"] != "OLT V1600GS-R Alpha OLT 2" {
		t.Fatalf("olt class %v", k)
	}
	if k := kinds[tONUMAC]; k["kind"] != "onu" || k["label"] != "ONU F601 · ZTEGC1" || k["equipment_id"] != onuID {
		t.Fatalf("onu class %v", k)
	}
	if k := kinds["6C:1F:F7:BB:46:94"]; k["kind"] != "unknown" || k["never_asked"] != true {
		t.Fatalf("unknown class %v", k)
	}
	mk := func(mac string, extra map[string]any) map[string]any {
		b := map[string]any{"site_id": r.A.ID, "mac": mac, "unit": "N1", "apartment_id": aptA}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	if g := r.call(r.oper.Access, "POST", "/unregistered/create", mk(tOLTMAC, nil), nil); g.Status != 400 || !strings.Contains(string(g.Raw), "That MAC belongs to OLT Alpha OLT 2") {
		t.Fatalf("customer on an OLT MAC: %d %s", g.Status, g.Raw)
	}
	if g := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": aptA, "unit": "N0", "mac": tOLTMAC}, nil); g.Status != 400 {
		t.Fatalf("POST /accounts on an OLT MAC: %d %s", g.Status, g.Raw)
	}
	cf := r.call(r.oper.Access, "POST", "/unregistered/create", mk("6C:1F:F7:BB:46:94", nil), nil)
	if cf.Status != 409 || !strings.Contains(string(cf.Raw), "confirm_required") || !strings.Contains(string(cf.Raw), "never asked for an address") {
		t.Fatalf("no confirmation: %d %s", cf.Status, cf.Raw)
	}
	if ok := r.call(r.oper.Access, "POST", "/unregistered/create", mk("6C:1F:F7:BB:46:94", map[string]any{"confirm_no_address": true}), nil); ok.Status != 201 {
		t.Fatalf("confirmed: %d %s", ok.Status, ok.Raw)
	}
	// the ONU of the register is made a customer through its equipment id (the pair is used)
	if ok := r.call(r.oper.Access, "POST", "/unregistered/create", mk(tONUMAC, map[string]any{"unit": "N2", "equipment_id": onuID}), nil); ok.Status != 201 {
		t.Fatalf("from the ONU: %d %s", ok.Status, ok.Raw)
	}
	r.st.View(func(d *store.Data) {
		if e := d.Equipment[onuID]; e.Status != store.EqInstalled || e.AccountID == "" {
			t.Fatalf("ONU not installed: %+v", e)
		}
	})
}
