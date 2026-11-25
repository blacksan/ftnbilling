package web_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"ispbill/internal/store"
)

func TestOLTAdoptAPIRolesStepUpAndSecrets(t *testing.T) {
	o := newOLTRig(t)
	p := "/olts/" + o.id
	const newPass = "OwnerTypedPassw0rdXyz12345"
	// reading the adoption state: a viewer may; it never holds a password
	for _, tok := range []string{o.view.Access, o.oper.Access, o.owner.Access} {
		g := o.call(tok, "GET", p+"/adoption", nil, nil)
		if g.Status != 200 {
			t.Fatalf("adoption: %d %s", g.Status, g.Raw)
		}
		if raw := string(g.Raw); strings.Contains(raw, o.f.Pass) || strings.Contains(raw, "Xpon@Olt9417#") {
			t.Fatalf("secret in the adoption JSON: %s", raw)
		}
	}
	// the owner-only actions
	acts := []struct{ method, path string }{{"POST", p + "/secure"}, {"PUT", p + "/credentials"}, {"POST", p + "/credentials/reveal"}}
	for _, a := range acts {
		body := map[string]any{"password": o.ownerW.Pass, "user": "admin", "pass": newPass}
		for _, tok := range []string{o.view.Access, o.oper.Access} {
			if g := o.call(tok, a.method, a.path, body, nil); g.Status != 403 {
				t.Errorf("%s %s by a non-owner: %d", a.method, a.path, g.Status)
			}
		}
		if g := o.call(o.owner.Access, a.method, a.path, map[string]any{"user": "admin", "pass": newPass}, nil); g.Status != 400 {
			t.Errorf("%s %s without the password: %d %s", a.method, a.path, g.Status, g.Raw)
		}
		if g := o.call(o.owner.Access, a.method, a.path, map[string]any{"password": "wrong-password-xx", "user": "admin", "pass": newPass}, nil); g.Status != 403 {
			t.Errorf("%s %s with a wrong password: %d %s", a.method, a.path, g.Status, g.Raw)
		}
	}
	// Secure on an OLT that was not identified (no serial check yet) is refused and sends nothing
	o.f.Reset()
	if g := o.call(o.owner.Access, "POST", p+"/secure", map[string]any{"password": o.ownerW.Pass}, nil); g.Status != 409 {
		t.Fatalf("secure of an unidentified OLT: %d %s", g.Status, g.Raw)
	}
	// Reveal: owner, password, never cached, audited, the password not in the audit text
	rv := o.call(o.owner.Access, "POST", p+"/credentials/reveal", map[string]any{"password": o.ownerW.Pass}, nil)
	if rv.Status != 200 || rv.JSON(t)["password"] != o.f.Pass {
		t.Fatalf("reveal: %d %s", rv.Status, rv.Raw)
	}
	if !strings.Contains(rv.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("reveal is cacheable: %v", rv.Header)
	}
	aud := o.auditText()
	if !strings.Contains(aud, "revealed") {
		t.Fatalf("reveal not audited: %s", aud)
	}
	// owner types a login: stored sealed, then not in any reading
	if g := o.call(o.owner.Access, "PUT", p+"/credentials", map[string]any{"password": o.ownerW.Pass, "user": "admin", "pass": newPass}, nil); g.Status != 200 {
		t.Fatalf("credentials: %d %s", g.Status, g.Raw)
	}
	for _, tok := range []string{o.view.Access, o.oper.Access, o.owner.Access} {
		for _, path := range []string{p + "/adoption", p + "/status", p, "/olts"} {
			if g := o.call(tok, "GET", path, nil, nil); strings.Contains(string(g.Raw), newPass) {
				t.Errorf("the typed password is in GET %s", path)
			}
		}
	}
	if strings.Contains(o.auditText(), newPass) || strings.Contains(o.alertText(), newPass) {
		t.Fatal("the typed password is in the audit trail or an alert")
	}
}

func TestOLTAdoptPagesOwnerOnlyNoSecretsNoInlineJS(t *testing.T) {
	o := newOLTRig(t)
	cl := &http.Client{CheckRedirect: noRedirect}
	ob := signIn(t, o.ts, o.srv, o.st, o.ownerW)
	pb := signIn(t, o.ts, o.srv, o.st, o.operW)
	manage := "/admin/olts/manage?id=" + url.QueryEscape(o.id)
	for name, b := range map[string]*browser{"owner": ob, "operator": pb} {
		resp := b.Do(cl, o.ts.URL, "GET", manage, nil)
		bs, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page := string(bs)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
		for _, secret := range []string{o.f.Pass, "Xpon@Olt9417#"} {
			if strings.Contains(page, secret) {
				t.Errorf("%s page shows a password", name)
			}
		}
		if hasInlineScript(page) || inlineHandler.MatchString(page) || strings.Contains(page, "javascript:") {
			t.Errorf("%s page has inline script", name)
		}
		if name == "operator" && strings.Contains(page, "Secure this OLT") {
			t.Errorf("the operator is offered the owner's Secure button")
		}
	}
	post := func(b *browser, path string, f url.Values) string {
		resp := b.Do(cl, o.ts.URL, "POST", path, f)
		defer resp.Body.Close()
		loc, _ := url.QueryUnescape(resp.Header.Get("Location"))
		return loc
	}
	for _, path := range []string{"/admin/olts/manage/secure", "/admin/olts/manage/credentials", "/admin/olts/manage/reveal", "/admin/olts/manage/mgmt-port", "/admin/olts/manage/manual"} {
		v := url.Values{"id": {o.id}, "current_password": {longPW}, "user": {"admin"}, "pass": {"x"}}
		if loc := post(pb, path, v); !strings.Contains(loc, "only an owner") {
			t.Errorf("operator on %s: %s", path, loc)
		}
		if strings.HasSuffix(path, "mgmt-port") || strings.HasSuffix(path, "manual") {
			continue // owner-only settings with no secret in them: no step-up
		}
		if loc := post(ob, path, url.Values{"id": {o.id}, "user": {"admin"}, "pass": {"x"}}); !strings.Contains(loc, "Error") && !strings.Contains(loc, "password") {
			t.Errorf("owner without the password on %s: %q", path, loc)
		}
	}
	// Reveal page: owner + password, no-store
	resp := ob.Do(cl, o.ts.URL, "POST", "/admin/olts/manage/reveal", url.Values{"id": {o.id}, "current_password": {o.ownerW.Pass}})
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(bs), o.f.Pass) || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("reveal page: %d %v", resp.StatusCode, resp.Header)
	}
	if hasInlineScript(string(bs)) || inlineHandler.MatchString(string(bs)) {
		t.Error("the reveal page has inline script")
	}
	if strings.Contains(o.auditText(), o.f.Pass) {
		t.Error("the revealed password is in the audit trail")
	}
}

func TestOLTSecureStatusEndpoints(t *testing.T) {
	o := newOLTRig(t)
	p := "/olts/" + o.id
	g := o.call(o.owner.Access, "GET", p+"/secure", nil, nil)
	if g.Status != 200 || g.JSON(t)["running"] != false || g.JSON(t)["known"] != false {
		t.Fatalf("status of no job: %d %s", g.Status, g.Raw)
	}
	if g := o.call(o.oper.Access, "GET", p+"/secure", nil, nil); g.Status != 403 {
		t.Errorf("status by an operator: %d", g.Status)
	}
	cl := &http.Client{CheckRedirect: noRedirect}
	ob := signIn(t, o.ts, o.srv, o.st, o.ownerW)
	pb := signIn(t, o.ts, o.srv, o.st, o.operW)
	path := "/admin/olts/manage/secure-status?id=" + url.QueryEscape(o.id)
	resp := ob.Do(cl, o.ts.URL, "GET", path, nil)
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(bs), `"running":false`) || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Errorf("owner status: %d %s", resp.StatusCode, bs)
	}
	resp = pb.Do(cl, o.ts.URL, "GET", path, nil)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Errorf("an operator reads the owner's status")
	}
}

func TestOLTRawDebugCopyOwnerOnly(t *testing.T) {
	o := newOLTRig(t)
	if err := o.st.Update(func(d *store.Data) error {
		st := d.OLTStateOf(o.id)
		st.Status.Raw, st.Status.RawDebug = "PON 1 state, not understood:\nweird  thing", `weird\x1b[2C<b>thing</b>`
		d.OLTStates[o.id] = st
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cl := &http.Client{CheckRedirect: noRedirect}
	path := "/admin/olts/manage?id=" + url.QueryEscape(o.id)
	page := func(w *who) string {
		t.Helper()
		b := signIn(t, o.ts, o.srv, o.st, w)
		resp := b.Do(cl, o.ts.URL, "GET", path, nil)
		bs, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%d", resp.StatusCode)
		}
		return string(bs)
	}
	if p := page(o.ownerW); !strings.Contains(p, `weird\x1b[2C&lt;b&gt;thing&lt;/b&gt;`) {
		t.Errorf("the owner does not see the escaped debug copy")
	}
	if p := page(o.operW); strings.Contains(p, `weird\x1b[2C`) || !strings.Contains(p, "Not understood") {
		t.Errorf("an operator sees the debug copy (or not the section)")
	}
}

// The bench: the select showed ether4 although the OLT is on ether6; saving the card unchanged must leave it on ether6.
func TestOLTMgmtPortFormKeepsTheConfiguredPort(t *testing.T) {
	o := newOLTRig(t)
	if err := o.st.Update(func(d *store.Data) error {
		s := d.Sites[o.A.ID]
		s.OLTMgmtPort = "ether6"
		d.Sites[o.A.ID] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cl := &http.Client{CheckRedirect: noRedirect}
	ob := signIn(t, o.ts, o.srv, o.st, o.ownerW)
	resp := ob.Do(cl, o.ts.URL, "GET", "/admin/olts/manage?id="+url.QueryEscape(o.id), nil)
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(bs)
	if !strings.Contains(page, `<option value="ether6" selected>`) || !strings.Contains(page, `name="net" value="192.168.8.1/24"`) {
		i := strings.Index(page, `name="port"`)
		t.Fatalf("the configured port or address is not preselected: %s", page[max(i, 0):min(len(page), max(i, 0)+400)])
	}
	resp = ob.Do(cl, o.ts.URL, "POST", "/admin/olts/manage/mgmt-port", url.Values{"id": {o.id}, "action": {"set"}, "port": {"ether6"}, "net": {"192.168.8.1/24"}})
	resp.Body.Close()
	loc, _ := url.QueryUnescape(resp.Header.Get("Location"))
	if !strings.Contains(loc, "Nothing changed") {
		t.Errorf("unchanged form: %q", loc)
	}
	var port string
	o.st.View(func(d *store.Data) { port = d.Sites[o.A.ID].OLTMgmtPort })
	if port != "ether6" {
		t.Errorf("the role moved to %q", port)
	}
}
