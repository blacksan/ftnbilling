package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/olt"
	"ispbill/internal/olt/fake"
	"ispbill/internal/secretbox"
	"ispbill/internal/store"
)

const oltKey = "0123456789abcdef0123456789abcdef"

type oltRig struct {
	*apiRig
	f      *fake.OLT
	svc2   *olt.Service
	id     string
	mu     sync.Mutex
	alerts []string
}

func newOLTRig(t *testing.T) *oltRig {
	t.Helper()
	r := newAPIRig(t)
	if err := r.svc.SetDataKey([]byte(oltKey)); err != nil {
		t.Fatal(err)
	}
	f, err := fake.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	box, err := secretbox.New([]byte(oltKey))
	if err != nil {
		t.Fatal(err)
	}
	or := &oltRig{apiRig: r, f: f}
	or.svc2 = olt.New(olt.Options{Store: r.st, Box: box, Interval: time.Hour,
		ACS: func() *olt.ACS {
			return &olt.ACS{URL: "http://10.99.0.1:7547/", User: oltACSUser, Pass: oltACSPass, InformSeconds: 300}
		},
		Reach: func(d *store.Data, e store.Equipment, s store.OLTSettings) (string, error) { return f.SSHAddr, nil },
		Dial: func(ctx context.Context, n, a string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, n, a)
		},
		Alert: func(subject, msg string) {
			or.mu.Lock()
			or.alerts = append(or.alerts, subject+": "+msg)
			or.mu.Unlock()
		}})
	r.srv.SetOLT(or.svc2)
	var aptA string
	r.st.View(func(d *store.Data) { aptA = d.SiteApartmentIDs(r.A.ID)[0] })
	if err := r.st.Update(func(d *store.Data) error {
		s := d.Sites[r.A.ID]
		s.ONUMgmtN = 5
		d.Sites[r.A.ID] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := r.call(r.oper.Access, "POST", "/olts", map[string]any{"name": "Alpha OLT", "model": "V1600GS-R", "mac": tOLTMAC, "site_id": r.A.ID, "router_port": "ether3", "pon_ports": 2,
		"pon_apartments": map[string]any{"1": []string{aptA}}, "mgmt_ip": "10.7.5.2", "mgmt_user": f.User, "mgmt_pass": f.Pass}, nil)
	if c.Status != 201 {
		t.Fatalf("%d %s", c.Status, c.Raw)
	}
	or.id = c.JSON(t)["id"].(string)
	return or
}

func (o *oltRig) alertText() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Join(o.alerts, "\n")
}

const (
	oltACSUser = "ftn-web-acs"
	oltACSPass = "web-acs-pass-77"
)

func TestOLTAPIRolesStepUpAndSecrets(t *testing.T) {
	o := newOLTRig(t)
	o.f.SetRunning("vlan 100\n")
	o.f.Secret = "HASHED-ADMIN-SECRET"
	p := "/olts/" + o.id
	// a viewer reads the status; nothing else
	if g := o.call(o.view.Access, "GET", p+"/status", nil, nil); g.Status != 200 || strings.Contains(string(g.Raw), o.f.Pass) {
		t.Fatalf("status: %d %s", g.Status, g.Raw)
	}
	for _, c := range []struct{ m, path string }{{"POST", "/test"}, {"POST", "/poll"}} {
		if g := o.call(o.view.Access, c.m, p+c.path, nil, nil); g.Status != 403 {
			t.Errorf("viewer %s%s: %d", c.m, c.path, g.Status)
		}
	}
	// the operator tests the connection, the status then says connected
	if g := o.call(o.oper.Access, "POST", p+"/test", nil, nil); g.Status != 200 {
		t.Fatalf("test: %d %s", g.Status, g.Raw)
	}
	st := o.call(o.view.Access, "GET", p+"/status", nil, nil).JSON(t)
	if st["connected"] != true || !strings.Contains(st["version"].(string), "V1600GS-R") {
		t.Fatalf("%v", st)
	}
	// backups: owner only, redacted
	if g := o.call(o.oper.Access, "POST", p+"/poll", nil, nil); g.Status != 200 {
		t.Fatalf("poll: %d %s", g.Status, g.Raw)
	}
	if _, err := o.svc2.Backup(context.Background(), o.id, "manual", "olga"); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{o.view.Access, o.oper.Access} {
		if g := o.call(tok, "GET", p+"/backups", nil, nil); g.Status != 403 {
			t.Errorf("non-owner reads backups: %d", g.Status)
		}
	}
	list := o.call(o.owner.Access, "GET", p+"/backups", nil, nil)
	if list.Status != 200 {
		t.Fatalf("%d %s", list.Status, list.Raw)
	}
	bid := list.JSON(t)["items"].([]any)[0].(map[string]any)["id"].(string)
	bk := o.call(o.owner.Access, "GET", p+"/backups/"+bid, nil, nil)
	if bk.Status != 200 || strings.Contains(string(bk.Raw), "HASHED-ADMIN-SECRET") || !strings.Contains(string(bk.Raw), "vlan 100") {
		t.Fatalf("backup: %d %s", bk.Status, bk.Raw)
	}
	// the provisioning toggle: owner only, with the owner's password
	body := map[string]any{"enabled": true, "dialect_checked": true}
	if g := o.call(o.oper.Access, "PUT", p+"/provisioning", body, nil); g.Status != 403 {
		t.Fatalf("operator toggled: %d", g.Status)
	}
	if g := o.call(o.owner.Access, "PUT", p+"/provisioning", body, nil); g.Status != 400 {
		t.Fatalf("no password: %d %s", g.Status, g.Raw)
	}
	body["password"] = "wrong-password-xx"
	if g := o.call(o.owner.Access, "PUT", p+"/provisioning", body, nil); g.Status != 403 {
		t.Fatalf("wrong password: %d %s", g.Status, g.Raw)
	}
	var stt store.OLTState
	o.st.View(func(d *store.Data) { stt = d.OLTStateOf(o.id) })
	if stt.Settings.Provisioning || stt.Settings.DialectChecked {
		t.Fatal("a refused toggle changed the state")
	}
	body["password"] = o.ownerW.Pass
	if g := o.call(o.owner.Access, "PUT", p+"/provisioning", body, nil); g.Status != 200 {
		t.Fatalf("toggle: %d %s", g.Status, g.Raw)
	}
	o.st.View(func(d *store.Data) { stt = d.OLTStateOf(o.id) })
	if !stt.Settings.Provisioning || !stt.Settings.DialectChecked {
		t.Fatalf("%+v", stt.Settings)
	}
	// plan and apply: owner only; a stale hash is a conflict; the right one applies and verifies
	if g := o.call(o.oper.Access, "POST", p+"/plan", nil, nil); g.Status != 403 {
		t.Fatalf("operator planned: %d", g.Status)
	}
	pl := o.call(o.owner.Access, "POST", p+"/plan", nil, nil)
	if pl.Status != 200 || pl.JSON(t)["empty"] == true {
		t.Fatalf("plan: %d %s", pl.Status, pl.Raw)
	}
	hash := pl.JSON(t)["hash"].(string)
	// The plan shows the TR-069 line (ACS address) with the login hidden.
	if raw := string(pl.Raw); !strings.Contains(raw, "tr069_mng enable acs_server url http://10.99.0.1:7547/") || strings.Contains(raw, oltACSPass) || strings.Contains(raw, oltACSUser) {
		t.Fatalf("plan JSON: %s", raw)
	}
	if g := o.call(o.owner.Access, "POST", p+"/apply", map[string]any{"hash": "stale", "password": o.ownerW.Pass}, nil); g.Status != 409 {
		t.Fatalf("stale: %d %s", g.Status, g.Raw)
	}
	if g := o.call(o.owner.Access, "POST", p+"/apply", map[string]any{"hash": hash}, nil); g.Status != 400 {
		t.Fatalf("apply without password: %d", g.Status)
	}
	ap := o.call(o.owner.Access, "POST", p+"/apply", map[string]any{"hash": hash, "password": o.ownerW.Pass}, nil)
	if ap.Status != 200 || ap.JSON(t)["verified"] != true {
		t.Fatalf("apply: %d %s", ap.Status, ap.Raw)
	}
	// no secret in any response above or in the audit trail
	if raw := string(ap.Raw); strings.Contains(raw, oltACSPass) || strings.Contains(raw, oltACSUser) {
		t.Fatalf("apply response: %s", raw)
	}
	for _, secret := range []string{o.f.Pass, "HASHED-ADMIN-SECRET", oltACSPass, oltACSUser} {
		if strings.Contains(o.auditText(), secret) {
			t.Fatalf("%q in the audit trail", secret)
		}
	}
}

var inlineHandler = regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*["']`)

func TestOLTPagesOwnerOnlyNoSecretsNoInlineJS(t *testing.T) {
	o := newOLTRig(t)
	o.f.SetRunning("vlan 100\n")
	o.f.Secret = "HASHED-ADMIN-SECRET"
	if err := o.svc2.SaveSettings(o.id, olt.SettingsInput{EnablePass: "enable-pass-77"}, "admin"); err != nil {
		t.Fatal(err)
	}
	o.f.EnablePass = "enable-pass-77"
	if _, err := o.svc2.TestConnection(context.Background(), o.id, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.svc2.Backup(context.Background(), o.id, "manual", "admin"); err != nil {
		t.Fatal(err)
	}
	cl := &http.Client{CheckRedirect: noRedirect}
	ob := signIn(t, o.ts, o.srv, o.st, o.ownerW)
	vb := signIn(t, o.ts, o.srv, o.st, o.vwW)
	pb := signIn(t, o.ts, o.srv, o.st, o.operW)
	manage := "/admin/olts/manage?id=" + url.QueryEscape(o.id)
	for _, path := range []string{manage, "/admin/olts/adopt?id=" + url.QueryEscape(o.id)} {
		resp := ob.Do(cl, o.ts.URL, "GET", path, nil)
		bs, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page := string(bs)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, page)
		}
		for _, secret := range []string{o.f.Pass, "enable-pass-77", "HASHED-ADMIN-SECRET", "OLT login password"} {
			if strings.Contains(page, secret) {
				t.Errorf("%s shows %q", path, secret)
			}
		}
		if strings.Contains(strings.ToLower(page), "<script>") || hasInlineScript(page) || inlineHandler.MatchString(page) || strings.Contains(page, "javascript:") {
			t.Errorf("%s has inline script", path)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self';") || strings.Contains(csp, "unsafe-inline") && strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s CSP changed: %s", path, csp)
		}
	}
	// the owner page offers the permission controls and the backup, the operator's does not
	_, owner := o.webGet(t, ob, manage)
	for _, want := range []string{"Let the server configure this OLT", "Saved configurations", "Preview the plan"} {
		if !strings.Contains(owner, want) {
			t.Errorf("owner page lacks %q", want)
		}
	}
	for name, b := range map[string]*browser{"operator": pb, "viewer": vb} {
		_, pg := o.webGet(t, b, manage)
		if strings.Contains(pg, "Let the server configure") || strings.Contains(pg, "Saved configurations") {
			t.Errorf("%s sees owner controls", name)
		}
	}
	// POSTs: owner-only with the password
	post := func(b *browser, path string, f url.Values) string {
		resp := b.Do(cl, o.ts.URL, "POST", path, f)
		defer resp.Body.Close()
		loc, _ := url.QueryUnescape(resp.Header.Get("Location"))
		return loc
	}
	f := func(extra url.Values) url.Values {
		v := url.Values{"id": {o.id}, "on": {"1"}}
		for k, x := range extra {
			v[k] = x
		}
		return v
	}
	if loc := post(pb, "/admin/olts/manage/provisioning", f(url.Values{"current_password": {longPW}})); !strings.Contains(loc, "only an owner") {
		t.Errorf("operator: %s", loc)
	}
	if loc := post(ob, "/admin/olts/manage/provisioning", f(nil)); !strings.Contains(loc, "Error") {
		t.Errorf("no password: %s", loc)
	}
	if loc := post(ob, "/admin/olts/manage/provisioning", f(url.Values{"current_password": {"wrong"}})); !strings.Contains(loc, "Error") {
		t.Errorf("wrong password: %s", loc)
	}
	var stt store.OLTState
	o.st.View(func(d *store.Data) { stt = d.OLTStateOf(o.id) })
	if stt.Settings.Provisioning {
		t.Fatal("provisioning turned on without the password")
	}
	if loc := post(ob, "/admin/olts/manage/provisioning", f(url.Values{"current_password": {o.ownerW.Pass}})); strings.Contains(loc, "Error") {
		t.Errorf("right password: %s", loc)
	}
	o.st.View(func(d *store.Data) { stt = d.OLTStateOf(o.id) })
	if !stt.Settings.Provisioning {
		t.Fatal("not turned on")
	}
	// connection settings (protocol, host key, enable password) need the password again: a switch to Telnet or a
	// forgotten host key is a downgrade
	set := func(extra url.Values) url.Values {
		v := url.Values{"id": {o.id}, "proto": {"telnet"}, "clear_hostkey": {"1"}}
		for k, x := range extra {
			v[k] = x
		}
		return v
	}
	o.st.View(func(d *store.Data) { stt = d.OLTStateOf(o.id) })
	proto0 := stt.Settings.ProtoOrDefault()
	if loc := post(ob, "/admin/olts/manage/settings", set(nil)); !strings.Contains(loc, "Error") {
		t.Errorf("settings without the password: %s", loc)
	}
	if loc := post(pb, "/admin/olts/manage/settings", set(url.Values{"current_password": {longPW}})); !strings.Contains(loc, "only an owner") {
		t.Errorf("operator settings: %s", loc)
	}
	o.st.View(func(d *store.Data) { stt = d.OLTStateOf(o.id) })
	if stt.Settings.ProtoOrDefault() != proto0 {
		t.Fatal("the protocol changed without the password")
	}
	if loc := post(ob, "/admin/olts/manage/settings", set(url.Values{"proto": {proto0}, "current_password": {o.ownerW.Pass}})); strings.Contains(loc, "Error") {
		t.Errorf("settings with the password: %s", loc)
	}
	// backups: owner only, redacted, never cached
	var bid string
	o.st.View(func(d *store.Data) { bid = d.OLTStateOf(o.id).Backups[0].ID })
	bpath := "/admin/olts/backup?id=" + url.QueryEscape(o.id) + "&b=" + bid
	resp := ob.Do(cl, o.ts.URL, "GET", bpath, nil)
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(bs), "HASHED-ADMIN-SECRET") || !strings.Contains(string(bs), "vlan 100") || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("owner backup: %d %s %v", resp.StatusCode, bs, resp.Header)
	}
	if code, _ := o.webGet(t, pb, bpath); code == 200 {
		t.Error("an operator read a saved configuration")
	}
}

func TestOLTAPIStatusShowsNotInInventory(t *testing.T) {
	o := newOLTRig(t)
	o.f.SetRunning("vlan 100\n")
	o.f.AutoFind = [][2]string{{"1", "VSOL99999999"}}
	if g := o.call(o.oper.Access, "POST", "/olts/"+o.id+"/poll", nil, nil); g.Status != 200 {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
	var st struct {
		NotIn []struct{ Serial string } `json:"not_in_inventory"`
	}
	if err := json.Unmarshal(o.call(o.view.Access, "GET", "/olts/"+o.id+"/status", nil, nil).Raw, &st); err != nil || len(st.NotIn) != 1 || st.NotIn[0].Serial != "VSOL99999999" {
		t.Fatalf("%+v %v", st, err)
	}
	if !strings.Contains(o.alertText(), "VSOL99999999") {
		t.Fatalf("no owner alert: %q", o.alertText())
	}
	// nothing was written to the OLT
	for _, l := range o.f.Lines() {
		if strings.HasPrefix(l, "onu ") || l == "write" {
			t.Fatalf("wrote %q", l)
		}
	}
}

var scriptTag = regexp.MustCompile(`(?i)<script\b[^>]*>`)

// hasInlineScript is true when a page has a script tag with no src (inline code, which the CSP forbids).
func hasInlineScript(page string) bool {
	for _, tag := range scriptTag.FindAllString(page, -1) {
		if !strings.Contains(strings.ToLower(tag), "src=") {
			return true
		}
	}
	return false
}
