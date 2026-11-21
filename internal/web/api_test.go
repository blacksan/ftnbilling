package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// ---- helpers ---------------------------------------------------------------------------------

type tokens struct {
	Access, Refresh, DeviceID string
}

type reply struct {
	Status int
	Header http.Header
	Raw    []byte
}

func (r reply) JSON(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Raw, &m); err != nil {
		t.Fatalf("not JSON (%d): %q", r.Status, r.Raw)
	}
	return m
}

// errCode returns the envelope's code, failing the test when it is malformed.
func (r reply) errCode(t testing.TB) string {
	t.Helper()
	m := r.JSON(t)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %q", r.Raw)
	}
	for _, k := range []string{"code", "message", "details"} {
		if _, ok := e[k]; !ok {
			t.Fatalf("envelope lacks %q: %q", k, r.Raw)
		}
	}
	return e["code"].(string)
}

func (m *multi) call(tok, method, path string, body any, hdr map[string]string) reply {
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
	}
	req, _ := http.NewRequest(method, m.ts.URL+"/api/v1"+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := m.cl.Do(req)
	if err != nil {
		panic(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return reply{resp.StatusCode, resp.Header, b}
}

func (m *multi) apiLogin(t testing.TB, w *who, install string) tokens {
	t.Helper()
	r := m.call("", "POST", "/auth/login", map[string]any{"username": w.Name, "password": w.Pass, "totp": w.Code(t, m.st),
		"device": map[string]any{"install_id": install, "name": "Test phone", "model": "Pixel", "platform": "android", "app_version": "1.0"}}, nil)
	if r.Status != 200 {
		t.Fatalf("api login: %d %s", r.Status, r.Raw)
	}
	j := r.JSON(t)
	return tokens{j["access_token"].(string), j["refresh_token"].(string), j["device_id"].(string)}
}

// apiRig is a two-building server with an owner, an operator and a viewer signed in on phones.
type apiRig struct {
	*multi
	A, B               store.Site
	owner, oper, view  tokens
	ownerW, operW, vwW *who
}

func newAPIRig(t *testing.T) *apiRig {
	t.Helper()
	m := newOpsRig(t).multi
	r := &apiRig{multi: m}
	r.A, r.B = m.addSite(t, "Alpha"), m.addSite(t, "Beta")
	r.ownerW = m.owner
	r.operW = seedUser(t, m.srv, m.st, "olga", store.RoleOperator, longPW)
	r.vwW = seedUser(t, m.srv, m.st, "vic", store.RoleViewer, longPW)
	r.owner = m.apiLogin(t, r.ownerW, "install-owner")
	r.oper = m.apiLogin(t, r.operW, "install-oper")
	r.view = m.apiLogin(t, r.vwW, "install-view")
	return r
}

func (r *apiRig) acct(t *testing.T, site store.Site, flat, mac string) store.Account {
	t.Helper()
	a, err := r.svc.CreateAccount(billing.AccountInput{SiteID: site.ID, Flat: flat, MAC: mac})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// noMACAcct is an open account that has no MAC yet (waiting for one to be attached).
func (r *apiRig) noMACAcct(t *testing.T, site store.Site, flat string) store.Account {
	t.Helper()
	a := r.acct(t, site, flat, "AA:BB:CC:00:FF:"+strings.ToUpper(fmt.Sprintf("%02x", len(flat)+int(flat[0]))))
	if err := r.st.Update(func(d *store.Data) error {
		x := d.Accounts[a.ID]
		x.MAC = ""
		d.Accounts[a.ID] = x
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.MAC = ""
	return a
}

func (r *apiRig) pkgID(t *testing.T, name string) string {
	t.Helper()
	var id string
	r.st.View(func(d *store.Data) {
		for _, p := range d.Packages {
			if p.Name == name {
				id = p.ID
			}
		}
	})
	if id == "" {
		t.Fatalf("no package %q", name)
	}
	return id
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

// ---- auth ------------------------------------------------------------------------------------

func TestAPILoginShapeAndErrors(t *testing.T) {
	r := newAPIRig(t)
	dev := map[string]any{"install_id": "i-1", "name": "P", "model": "M", "platform": "android", "app_version": "1"}
	login := func(user, pass, code string) reply {
		b := map[string]any{"username": user, "password": pass, "device": dev}
		if code != "" {
			b["totp"] = code
		}
		return r.call("", "POST", "/auth/login", b, nil)
	}
	ok := login("admin", pw, r.ownerW.Code(t, r.st))
	if ok.Status != 200 {
		t.Fatalf("login: %d %s", ok.Status, ok.Raw)
	}
	j := ok.JSON(t)
	for _, k := range []string{"access_token", "access_expires_at", "refresh_token", "refresh_expires_at", "device_id", "user"} {
		if j[k] == nil || j[k] == "" {
			t.Errorf("response lacks %s", k)
		}
	}
	if !strings.HasPrefix(str(j, "access_token"), "ftna_") || !strings.HasPrefix(str(j, "refresh_token"), "ftnr_") {
		t.Error("token prefixes")
	}
	exp, err := time.Parse(time.RFC3339, str(j, "access_expires_at"))
	if left := time.Until(exp); err != nil || left < 14*time.Minute || left > 15*time.Minute+5*time.Second {
		t.Errorf("access token life: %v (%v)", left, err)
	}
	if u := j["user"].(map[string]any); u["username"] != "admin" || u["role"] != "owner" || u["id"] == "" {
		t.Errorf("user: %v", u)
	}
	// Only hashes are stored.
	r.st.View(func(d *store.Data) {
		for _, x := range d.APIDevices {
			if strings.Contains(x.AccessHash, str(j, "access_token")) || x.AccessHash == str(j, "access_token") || x.RefreshHash == str(j, "refresh_token") {
				t.Error("a raw token is stored")
			}
		}
	})
	// Wrong password and unknown user: the same answer.
	a, b := login("admin", "wrong-password-x", r.ownerW.Code(t, r.st)), login("nobody", "wrong-password-x", "123456")
	if a.Status != 401 || b.Status != 401 || a.errCode(t) != "invalid_credentials" || b.errCode(t) != "invalid_credentials" {
		t.Fatalf("wrong password: %d %s / %d %s", a.Status, a.Raw, b.Status, b.Raw)
	}
	if string(a.Raw) != string(b.Raw) {
		t.Error("the two refusals differ (user enumeration)")
	}
	// Right password, no code or a wrong code.
	if x := login("admin", pw, ""); x.Status != 401 || x.errCode(t) != "totp_invalid" {
		t.Errorf("no code: %d %s", x.Status, x.Raw)
	}
	if x := login("admin", pw, "000000"); x.Status != 401 || x.errCode(t) != "totp_invalid" {
		t.Errorf("wrong code: %d %s", x.Status, x.Raw)
	}
	// A code works once.
	code := r.ownerW.Code(t, r.st)
	if x := login("admin", pw, code); x.Status != 200 {
		t.Fatalf("code: %d", x.Status)
	}
	if x := login("admin", pw, code); x.Status != 401 || x.errCode(t) != "totp_invalid" {
		t.Errorf("a replayed code: %d %s", x.Status, x.Raw)
	}
	// Bad JSON and missing fields use the envelope too.
	if x := r.call("", "POST", "/auth/login", "{nope", nil); x.Status != 400 || x.errCode(t) != "bad_request" {
		t.Errorf("bad json: %d %s", x.Status, x.Raw)
	}
	if x := r.call("", "POST", "/auth/login", map[string]any{"username": "admin"}, nil); x.Status != 400 || x.errCode(t) != "validation_failed" {
		t.Errorf("missing password: %d %s", x.Status, x.Raw)
	}
}

func TestAPILoginRecoveryCodeAndEnrolRequired(t *testing.T) {
	r := newAPIRig(t)
	codes, _ := r.srv.Auth().RegenerateRecoveryCodes(r.ownerW.ID, "test", "")
	dev := map[string]any{"install_id": "rec"}
	x := r.call("", "POST", "/auth/login", map[string]any{"username": "admin", "password": pw, "recovery_code": codes[0], "device": dev}, nil)
	if x.Status != 200 {
		t.Fatalf("recovery login: %d %s", x.Status, x.Raw)
	}
	x = r.call("", "POST", "/auth/login", map[string]any{"username": "admin", "password": pw, "recovery_code": codes[0], "device": dev}, nil)
	if x.Status != 401 || x.errCode(t) != "totp_invalid" {
		t.Fatalf("recovery code reused: %d %s", x.Status, x.Raw)
	}
	// A user who has not enrolled is told to do it on the web.
	hash := fastHash(t, longPW)
	if _, err := r.srv.Auth().CreateUser("newbie", store.RoleOperator, hash, "test", ""); err != nil {
		t.Fatal(err)
	}
	x = r.call("", "POST", "/auth/login", map[string]any{"username": "newbie", "password": longPW, "device": dev}, nil)
	if x.Status != 403 || x.errCode(t) != "totp_enrol_required" {
		t.Fatalf("not enrolled: %d %s", x.Status, x.Raw)
	}
}

func TestAPIRateLimit429(t *testing.T) {
	r := newAPIRig(t)
	bad := func() reply {
		return r.call("", "POST", "/auth/login", map[string]any{"username": "admin", "password": "wrong-password-x", "device": map[string]any{}},
			map[string]string{"X-Forwarded-For": "203.0.113.77"})
	}
	for i := 0; i < 5; i++ {
		if x := bad(); x.Status != 401 {
			t.Fatalf("attempt %d: %d", i, x.Status)
		}
	}
	x := bad()
	if x.Status != 429 || x.errCode(t) != "rate_limited" {
		t.Fatalf("sixth: %d %s", x.Status, x.Raw)
	}
	ra := x.Header.Get("Retry-After")
	d := x.JSON(t)["error"].(map[string]any)["details"].(map[string]any)
	if ra == "" || d["retry_after_s"] == nil || ra != strings.TrimSuffix(strings.TrimSpace(jsonNum(d["retry_after_s"])), ".0") {
		t.Fatalf("Retry-After %q vs details %v", ra, d)
	}
}

func jsonNum(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestAPIBearerChecks(t *testing.T) {
	r := newAPIRig(t)
	for name, tok := range map[string]string{"none": "", "junk": "ftna_nope.nope", "refresh as access": r.owner.Refresh} {
		x := r.call(tok, "GET", "/me", nil, nil)
		if x.Status != 401 || x.errCode(t) != "token_revoked" {
			t.Errorf("%s: %d %s", name, x.Status, x.Raw)
		}
	}
	// A browser cookie never opens the API.
	req, _ := http.NewRequest("GET", r.ts.URL+"/api/v1/me", nil)
	req.AddCookie(r.br.Cookie)
	resp, _ := r.cl.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("cookie on the API: %d", resp.StatusCode)
	}
	me := r.call(r.owner.Access, "GET", "/me", nil, nil).JSON(t)
	if me["user"].(map[string]any)["username"] != "admin" || me["device"].(map[string]any)["id"] != r.owner.DeviceID {
		t.Errorf("me: %v", me)
	}
	// An expired access token says so (the app then refreshes).
	if err := r.st.Update(func(d *store.Data) error {
		x := d.APIDevices[r.owner.DeviceID]
		x.AccessExpiresAt = time.Now().Add(-time.Minute)
		d.APIDevices[r.owner.DeviceID] = x
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if x := r.call(r.owner.Access, "GET", "/me", nil, nil); x.Status != 401 || x.errCode(t) != "token_expired" {
		t.Errorf("expired: %d %s", x.Status, x.Raw)
	}
	// Unknown calls are a JSON 404 with the envelope.
	if x := r.call(r.view.Access, "GET", "/nothing-here", nil, nil); x.Status != 404 || x.errCode(t) != "not_found" {
		t.Errorf("unknown path: %d %s", x.Status, x.Raw)
	}
}

func TestAPIRefreshRotationReuseGraceAndRevoke(t *testing.T) {
	r := newAPIRig(t)
	refresh := func(tok string) reply {
		return r.call("", "POST", "/auth/refresh", map[string]any{"refresh_token": tok}, nil)
	}
	first := r.owner
	x := refresh(first.Refresh)
	if x.Status != 200 {
		t.Fatalf("refresh: %d %s", x.Status, x.Raw)
	}
	j := x.JSON(t)
	if j["user"] == nil || j["device_id"] != first.DeviceID || j["refresh_token"] == first.Refresh || j["access_token"] == first.Access {
		t.Fatalf("refresh response: %v", j)
	}
	second := tokens{str(j, "access_token"), str(j, "refresh_token"), first.DeviceID}
	// The old access token no longer works; the new one does.
	if x := r.call(first.Access, "GET", "/me", nil, nil); x.Status != 401 {
		t.Errorf("the old access token still works: %d", x.Status)
	}
	if x := r.call(second.Access, "GET", "/me", nil, nil); x.Status != 200 {
		t.Errorf("new access token: %d", x.Status)
	}
	// Presenting the previous refresh token right away is a retry (the app lost the answer): allowed.
	x = refresh(first.Refresh)
	if x.Status != 200 {
		t.Fatalf("a retry inside the grace period: %d %s", x.Status, x.Raw)
	}
	third := tokens{str(x.JSON(t), "access_token"), str(x.JSON(t), "refresh_token"), first.DeviceID}
	// After the grace period the old token means theft: the device is revoked, new tokens included.
	// (The clock of the sign-in service moves on: rewriting the stored time would look like a stale write.)
	skip := authn.RefreshGrace + time.Minute
	r.srv.Auth().SetClock(func() time.Time { return time.Now().Add(skip) })
	// The grace window is measured from the last rotation, so rotate once more then age it.
	_ = third
	if x := refresh(first.Refresh); x.Status != 401 || x.errCode(t) != "token_revoked" {
		t.Fatalf("a reused refresh token: %d %s", x.Status, x.Raw)
	}
	if x := r.call(third.Access, "GET", "/me", nil, nil); x.Status != 401 || x.errCode(t) != "token_revoked" {
		t.Errorf("the device survived a reuse: %d %s", x.Status, x.Raw)
	}
	if x := refresh(third.Refresh); x.Status != 401 || x.errCode(t) != "token_revoked" {
		t.Errorf("the revoked device refreshed: %d", x.Status)
	}
	// Junk refresh tokens are token_revoked and revoke nothing.
	if x := refresh("ftnr_" + r.oper.DeviceID + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); x.Status != 401 {
		t.Errorf("guessed token: %d", x.Status)
	}
	if x := r.call(r.oper.Access, "GET", "/me", nil, nil); x.Status != 200 {
		t.Error("a guessed refresh token revoked a stranger's device")
	}
}

func TestAPILogoutDevicesAndRevoke(t *testing.T) {
	r := newAPIRig(t)
	second := r.apiLogin(t, r.ownerW, "install-owner-2")
	list := r.call(r.owner.Access, "GET", "/auth/devices", nil, nil).JSON(t)["items"].([]any)
	if len(list) != 2 {
		t.Fatalf("devices: %v", list)
	}
	cur := 0
	for _, d := range list {
		if d.(map[string]any)["current"] == true {
			cur++
		}
	}
	if cur != 1 {
		t.Errorf("current flags: %d", cur)
	}
	// A viewer cannot revoke the owner's phone; the owner can revoke anyone's.
	if x := r.call(r.view.Access, "DELETE", "/auth/devices/"+second.DeviceID, nil, nil); x.Status != 404 && x.Status != 403 {
		t.Errorf("a viewer revoking another user's device: %d", x.Status)
	}
	if x := r.call(r.owner.Access, "DELETE", "/auth/devices/"+r.view.DeviceID, nil, nil); x.Status != 204 {
		t.Fatalf("owner revokes the viewer's phone: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.view.Access, "GET", "/me", nil, nil); x.Status != 401 || x.errCode(t) != "token_revoked" {
		t.Errorf("revoked device: %d %s", x.Status, x.Raw)
	}
	if x := r.call(second.Access, "POST", "/auth/logout", nil, nil); x.Status != 204 {
		t.Fatalf("logout: %d", x.Status)
	}
	if x := r.call(second.Access, "GET", "/me", nil, nil); x.Status != 401 {
		t.Errorf("after logout: %d", x.Status)
	}
	// Changing the password on the web revokes every phone.
	if err := r.srv.Auth().SetPassword(r.ownerW.ID, fastHash(t, longPW), "test", "", ""); err != nil {
		t.Fatal(err)
	}
	if x := r.call(r.owner.Access, "GET", "/me", nil, nil); x.Status != 401 {
		t.Errorf("a password change left the phone signed in: %d", x.Status)
	}
}

// ---- roles -----------------------------------------------------------------------------------

func TestAPIRoleMatrix(t *testing.T) {
	r := newAPIRig(t)
	a := r.acct(t, r.A, "Flat 1", "AA:BB:CC:00:10:01")
	type tc struct {
		method, path string
		body         any
		viewer, oper int // forbidden or not (403 vs anything else)
		owner        int
	}
	const F, OK = 403, 0 // OK: anything but 403
	for _, c := range []tc{
		{"GET", "/me", nil, OK, OK, OK},
		{"GET", "/accounts", nil, OK, OK, OK},
		{"GET", "/dashboard", nil, OK, OK, OK},
		{"GET", "/sites", nil, OK, OK, OK},
		{"GET", "/payments", nil, OK, OK, OK},
		{"GET", "/alerts", nil, OK, OK, OK},
		{"GET", "/unregistered", nil, OK, OK, OK},
		{"GET", "/audit", nil, F, F, OK},
		{"POST", "/accounts", map[string]any{}, F, OK, OK},
		{"PATCH", "/accounts/" + a.ID, map[string]any{}, F, OK, OK},
		{"PUT", "/accounts/" + a.ID + "/mac", map[string]any{}, F, OK, OK},
		{"POST", "/accounts/" + a.ID + "/suspend", nil, F, OK, OK},
		{"POST", "/accounts/" + a.ID + "/reconcile", nil, F, OK, OK},
		{"POST", "/payments", map[string]any{}, F, OK, OK},
		{"POST", "/alerts/mute", map[string]any{}, F, OK, OK},
		{"POST", "/sites/" + r.A.ID + "/unregistered/refresh", nil, F, OK, OK},
		{"POST", "/unregistered/attach", map[string]any{}, F, OK, OK},
		{"POST", "/unregistered/create", map[string]any{}, F, OK, OK},
		{"POST", "/unregistered/dismiss", map[string]any{}, F, OK, OK},
	} {
		for who, want := range map[string]int{r.view.Access: c.viewer, r.oper.Access: c.oper, r.owner.Access: c.owner} {
			got := r.call(who, c.method, c.path, c.body, nil)
			role := map[string]string{r.view.Access: "viewer", r.oper.Access: "operator", r.owner.Access: "owner"}[who]
			if (want == F) != (got.Status == 403) {
				t.Errorf("%s %s %s %s: got %d", role, c.method, c.path, got.Raw, got.Status)
			}
			if got.Status == 403 && got.errCode(t) != "forbidden_role" {
				t.Errorf("%s %s: wrong code", role, c.path)
			}
		}
	}
}

// ---- accounts --------------------------------------------------------------------------------

func TestAPIAccountsCRUDAndShapes(t *testing.T) {
	r := newAPIRig(t)
	x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": aptOf(t, r.st, r.A.ID), "site_id": r.A.ID, "flat": "A-101", "mac": "aa-bb-cc-00-20-01", "tenant": "Wanjiku", "phone": "0712345678"}, nil)
	if x.Status != 201 {
		t.Fatalf("create: %d %s", x.Status, x.Raw)
	}
	acct := x.JSON(t)
	if acct["mac"] != "AA:BB:CC:00:20:01" || acct["flat"] != "A-101" || acct["site_name"] != "Alpha" || acct["state"] != "new" || acct["suspended"] != false {
		t.Fatalf("account: %v", acct)
	}
	rt := acct["router"].(map[string]any)
	if ls := rt["lease_status"]; ls != "none" && ls != "waiting" && ls != "bound" {
		t.Errorf("lease_status %v", ls)
	}
	id := str(acct, "id")
	// Validation.
	if x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": aptOf(t, r.st, r.A.ID), "site_id": r.A.ID, "flat": "A-102", "mac": "nonsense"}, nil); x.Status != 400 || x.errCode(t) != "validation_failed" {
		t.Errorf("bad mac: %d %s", x.Status, x.Raw)
	} else if f := x.JSON(t)["error"].(map[string]any)["details"].(map[string]any)["fields"].(map[string]any); f["mac"] == nil {
		t.Errorf("no fields.mac: %s", x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": aptOf(t, r.st, r.A.ID), "site_id": r.A.ID, "flat": "A-101", "mac": "AA:BB:CC:00:20:02"}, nil); x.Status != 409 || x.errCode(t) != "unit_in_use" {
		t.Errorf("duplicate flat: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"site_id": "nope", "flat": "Z", "mac": "AA:BB:CC:00:20:03"}, nil); x.Status != 400 && x.Status != 404 {
		t.Errorf("unknown site: %d", x.Status)
	}
	// Detail.
	d := r.call(r.view.Access, "GET", "/accounts/"+id, nil, nil).JSON(t)
	cust := d["customer"].(map[string]any)
	if cust["name"] != "Wanjiku" || cust["phone"] == "" {
		t.Errorf("customer: %v", cust)
	}
	for _, k := range []string{"payments", "events", "usage", "customer"} {
		if d[k] == nil {
			t.Errorf("detail lacks %s", k)
		}
	}
	if _, ok := d["usage"].(map[string]any)["month_bytes"]; !ok {
		t.Error("usage.month_bytes")
	}
	for _, e := range d["events"].([]any) {
		em := e.(map[string]any)
		if em["time"] == nil || em["msg"] == nil {
			if _, has := em["actor"]; !has {
				t.Errorf("event shape: %v", em)
			}
		}
	}
	if x := r.call(r.view.Access, "GET", "/accounts/nope", nil, nil); x.Status != 404 || x.errCode(t) != "not_found" {
		t.Errorf("unknown account: %d", x.Status)
	}
	// Patch.
	p := r.call(r.oper.Access, "PATCH", "/accounts/"+id, map[string]any{"tenant": "Njeri", "notes": "second floor"}, nil)
	if p.Status != 200 || p.JSON(t)["tenant"] != "Njeri" || p.JSON(t)["notes"] != "second floor" {
		t.Errorf("patch: %d %s", p.Status, p.Raw)
	}
	// MAC change.
	if x := r.call(r.oper.Access, "PUT", "/accounts/"+id+"/mac", map[string]any{"mac": "AA:BB:CC:00:20:09"}, nil); x.Status != 200 || x.JSON(t)["mac"] != "AA:BB:CC:00:20:09" {
		t.Errorf("mac: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "PUT", "/accounts/"+id+"/mac", map[string]any{"mac": "zz"}, nil); x.Status != 400 || x.errCode(t) != "validation_failed" {
		t.Errorf("bad mac put: %d %s", x.Status, x.Raw)
	}
	// Suspend / unsuspend.
	if x := r.call(r.oper.Access, "POST", "/accounts/"+id+"/suspend", nil, nil); x.Status != 200 || x.JSON(t)["suspended"] != true || x.JSON(t)["state"] != "suspended" {
		t.Errorf("suspend: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/accounts/"+id+"/unsuspend", nil, nil); x.Status != 200 || x.JSON(t)["suspended"] != false {
		t.Errorf("unsuspend: %d %s", x.Status, x.Raw)
	}
	// Extend (activate) with a package, then change package, then reconcile.
	hour := r.pkgID(t, "1 Hour")
	x = r.call(r.oper.Access, "POST", "/accounts/"+id+"/extend", map[string]any{"package_id": hour, "note": "cash at the door"}, nil)
	if x.Status != 200 || x.JSON(t)["state"] != "active" || x.JSON(t)["expires_at"] == nil || x.JSON(t)["package"] == nil {
		t.Fatalf("extend: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/accounts/"+id+"/package", map[string]any{"package_id": hour, "keep_time": true}, nil); x.Status != 200 {
		t.Errorf("package: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/accounts/"+id+"/package", map[string]any{"keep_time": true}, nil); x.Status != 400 {
		t.Errorf("package without id: %d", x.Status)
	}
	if x := r.call(r.oper.Access, "POST", "/accounts/"+id+"/reconcile", nil, nil); x.Status != 202 || x.JSON(t)["queued"] != true {
		t.Errorf("reconcile: %d %s", x.Status, x.Raw)
	}
	// The audit log names the phone user.
	au := r.call(r.owner.Access, "GET", "/audit?limit=100", nil, nil)
	if au.Status != 200 || !strings.Contains(string(au.Raw), `"actor":"olga"`) {
		t.Errorf("audit: %d %s", au.Status, au.Raw)
	}
}

func TestAPIMACInUseBody(t *testing.T) {
	r := newAPIRig(t)
	a := r.acct(t, r.A, "Flat 7", "AA:BB:CC:00:30:07")
	b := r.acct(t, r.B, "B-2", "AA:BB:CC:00:30:08")
	// Another apartment, same MAC: refused as mac_other_building, and the body names the owner.
	x := r.call(r.oper.Access, "PUT", "/accounts/"+b.ID+"/mac", map[string]any{"mac": "aa:bb:cc:00:30:07"}, nil)
	if x.Status != 409 || x.errCode(t) != "mac_other_building" {
		t.Fatalf("put mac: %d %s", x.Status, x.Raw)
	}
	det := x.JSON(t)["error"].(map[string]any)["details"].(map[string]any)
	acc := det["belongs_to"].(map[string]any)
	if det["mac"] != "AA:BB:CC:00:30:07" || acc["account_id"] != a.ID || acc["code"] != a.Code || acc["unit"] != "Flat 7" || acc["site_id"] != r.A.ID || acc["site_name"] != "Alpha" ||
		acc["apartment"].(map[string]any)["name"] != "Alpha" {
		t.Fatalf("details: %v", det)
	}
	x = r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": aptOf(t, r.st, r.B.ID), "flat": "B-9", "mac": "AA:BB:CC:00:30:07"}, nil)
	if x.Status != 409 || x.errCode(t) != "mac_other_building" {
		t.Fatalf("create with a used mac: %d %s", x.Status, x.Raw)
	}
	// The same apartment keeps the older code and body.
	x = r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": aptOf(t, r.st, r.A.ID), "flat": "A-9", "mac": "AA:BB:CC:00:30:07"}, nil)
	if x.Status != 409 || x.errCode(t) != "mac_in_use" {
		t.Fatalf("create with a mac used in the same apartment: %d %s", x.Status, x.Raw)
	}
}

func TestAPIAccountListFiltersAndPagination(t *testing.T) {
	r := newAPIRig(t)
	for i := 0; i < 7; i++ {
		r.acct(t, r.A, "A-"+string(rune('1'+i)), "AA:BB:CC:00:40:0"+string(rune('1'+i)))
	}
	r.acct(t, r.B, "B-1", "AA:BB:CC:00:40:11")
	var seen []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		q := "/accounts?limit=3"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		j := r.call(r.view.Access, "GET", q, nil, nil).JSON(t)
		items := j["items"].([]any)
		if len(items) > 3 {
			t.Fatalf("page too large: %d", len(items))
		}
		for _, it := range items {
			seen = append(seen, str(it.(map[string]any), "id"))
		}
		nc, _ := j["next_cursor"].(string)
		if nc == "" {
			if j["next_cursor"] != nil {
				t.Fatalf("last page cursor: %v", j["next_cursor"])
			}
			break
		}
		cursor = nc
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		uniq[id] = true
	}
	if len(seen) != 8 || len(uniq) != 8 {
		t.Fatalf("paging returned %d items, %d distinct", len(seen), len(uniq))
	}
	if j := r.call(r.view.Access, "GET", "/accounts?site="+r.B.ID, nil, nil).JSON(t); len(j["items"].([]any)) != 1 {
		t.Errorf("site filter: %v", j)
	}
	if j := r.call(r.view.Access, "GET", "/accounts?q=B-1", nil, nil).JSON(t); len(j["items"].([]any)) != 1 {
		t.Errorf("q filter: %v", j)
	}
	if j := r.call(r.view.Access, "GET", "/accounts?state=active", nil, nil).JSON(t); len(j["items"].([]any)) != 0 {
		t.Errorf("state filter: %v", j)
	}
	for _, bad := range []string{"limit=0", "limit=abc", "cursor=!!!", "state=weird"} {
		if x := r.call(r.view.Access, "GET", "/accounts?"+bad, nil, nil); x.Status != 400 || x.errCode(t) != "validation_failed" {
			t.Errorf("%s: %d %s", bad, x.Status, x.Raw)
		}
	}
	if j := r.call(r.view.Access, "GET", "/accounts?limit=9999", nil, nil); j.Status != 200 {
		t.Errorf("limit above the maximum is clamped, got %d", j.Status)
	}
}

// ---- idempotency -----------------------------------------------------------------------------

func TestAPIIdempotencyReplay(t *testing.T) {
	r := newAPIRig(t)
	body := map[string]any{"apartment_id": aptOf(t, r.st, r.A.ID), "site_id": r.A.ID, "flat": "Idem-1", "mac": "AA:BB:CC:00:50:01"}
	h := map[string]string{"Idempotency-Key": "7d6f5f4e-0c63-4a3c-8f6b-0e2c7a1d9b11"}
	first := r.call(r.oper.Access, "POST", "/accounts", body, h)
	second := r.call(r.oper.Access, "POST", "/accounts", body, h)
	if first.Status != 201 || second.Status != 201 || string(first.Raw) != string(second.Raw) {
		t.Fatalf("replay differs: %d %q / %d %q", first.Status, first.Raw, second.Status, second.Raw)
	}
	n := 0
	r.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if a.Flat == "Idem-1" {
				n++
			}
		}
	})
	if n != 1 {
		t.Fatalf("%d accounts were created", n)
	}
	// Without the key the same call is a conflict (and an error is stored the same way).
	if x := r.call(r.oper.Access, "POST", "/accounts", body, nil); x.Status != 409 {
		t.Errorf("no key: %d", x.Status)
	}
	bad := map[string]string{"Idempotency-Key": "key-for-an-error"}
	e1 := r.call(r.oper.Access, "POST", "/accounts", body, bad)
	e2 := r.call(r.oper.Access, "POST", "/accounts", body, bad)
	if e1.Status != 409 || e2.Status != 409 || string(e1.Raw) != string(e2.Raw) {
		t.Errorf("an error replay differs: %d %q / %d %q", e1.Status, e1.Raw, e2.Status, e2.Raw)
	}
	// The same key for a different request is refused; another device has its own keys.
	if x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"site_id": r.A.ID, "flat": "Idem-2", "mac": "AA:BB:CC:00:50:02"}, h); x.Status != 400 {
		t.Errorf("key reuse for another body: %d %s", x.Status, x.Raw)
	}
	other := r.call(r.owner.Access, "POST", "/accounts", map[string]any{"apartment_id": aptOf(t, r.st, r.A.ID), "site_id": r.A.ID, "flat": "Idem-3", "mac": "AA:BB:CC:00:50:03"}, h)
	if other.Status != 201 {
		t.Errorf("another device with the same key: %d %s", other.Status, other.Raw)
	}
	// Parallel duplicates run once.
	var wg sync.WaitGroup
	hp := map[string]string{"Idempotency-Key": "parallel-1"}
	res := make([]reply, 5)
	pb := map[string]any{"apartment_id": aptOf(t, r.st, r.A.ID), "site_id": r.A.ID, "flat": "Idem-P", "mac": "AA:BB:CC:00:50:09"}
	for i := range res {
		wg.Add(1)
		go func() { defer wg.Done(); res[i] = r.call(r.oper.Access, "POST", "/accounts", pb, hp) }()
	}
	wg.Wait()
	for _, x := range res {
		if x.Status != 201 || string(x.Raw) != string(res[0].Raw) {
			t.Fatalf("parallel replay: %d %q vs %q", x.Status, x.Raw, res[0].Raw)
		}
	}
}

// ---- payments --------------------------------------------------------------------------------

func TestAPIPayments(t *testing.T) {
	r := newAPIRig(t)
	a := r.acct(t, r.A, "P-1", "AA:BB:CC:00:60:01")
	hour := r.pkgID(t, "1 Hour")
	x := r.call(r.oper.Access, "POST", "/payments", map[string]any{"account_id": a.ID, "amount_kes": 20, "trans_code": "QWE123ABC1", "package_id": hour, "note": "paid at office"}, nil)
	if x.Status != 201 {
		t.Fatalf("create: %d %s", x.Status, x.Raw)
	}
	p := x.JSON(t)
	if p["status"] != "matched" || p["account_id"] != a.ID || p["flat"] != "P-1" || p["amount_kes"] != float64(20) || p["key"] == "" {
		t.Fatalf("payment shape: %v", p)
	}
	if x := r.call(r.oper.Access, "POST", "/payments", map[string]any{"account_id": a.ID, "amount_kes": 20, "trans_code": "QWE123ABC1", "package_id": hour}, nil); x.Status != 409 || x.errCode(t) != "duplicate_payment" {
		t.Errorf("duplicate: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/payments", map[string]any{"account_id": a.ID}, nil); x.Status != 400 || x.errCode(t) != "validation_failed" {
		t.Errorf("empty: %d %s", x.Status, x.Raw)
	}
	// A cash payment needs no transaction code.
	if x := r.call(r.oper.Access, "POST", "/payments", map[string]any{"account_id": a.ID, "amount_kes": 20, "package_id": hour}, nil); x.Status != 201 || !strings.HasPrefix(str(x.JSON(t), "key"), "CASH-") || x.JSON(t)["method"] != "cash" {
		t.Errorf("cash: %d %s", x.Status, x.Raw)
	}
	// Money that matched nothing waits as unmatched, and can be assigned or resolved.
	_, err := r.svc.RecordUnmatched(billing.UnmatchedInput{Key: "UNM1", Method: "mpesa", AmountKES: 20, SiteID: r.A.ID, Phone: "254700000001", Name: "J DOE", BillRef: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = r.svc.RecordUnmatched(billing.UnmatchedInput{Key: "UNM2", Method: "mpesa", AmountKES: 50, SiteID: r.A.ID, Phone: "254700000002", BillRef: "x"})
	list := r.call(r.view.Access, "GET", "/payments?status=unmatched", nil, nil).JSON(t)
	items := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("unmatched list: %s", jsonNum(list))
	}
	if list["totals"].(map[string]any)["count"] != float64(2) {
		t.Errorf("totals: %v", list["totals"])
	}
	as := r.call(r.oper.Access, "POST", "/payments/"+"UNM1"+"/assign", map[string]any{"account_id": a.ID, "package_id": hour}, nil)
	if as.Status != 200 || as.JSON(t)["status"] != "matched" || as.JSON(t)["account_id"] != a.ID {
		t.Fatalf("assign: %d %s", as.Status, as.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/payments/"+"UNM1"+"/assign", map[string]any{"account_id": a.ID, "package_id": hour}, nil); x.Status != 409 || x.errCode(t) != "bad_state" {
		t.Errorf("assign twice: %d %s", x.Status, x.Raw)
	}
	rs := r.call(r.oper.Access, "POST", "/payments/"+"UNM2"+"/resolve", map[string]any{"status": "refunded", "note": "sent back"}, nil)
	if rs.Status != 200 || rs.JSON(t)["status"] != "refunded" {
		t.Fatalf("resolve: %d %s", rs.Status, rs.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/payments/"+"UNM2"+"/resolve", map[string]any{"status": "bogus"}, nil); x.Status != 400 {
		t.Errorf("bad status: %d", x.Status)
	}
	if x := r.call(r.oper.Access, "POST", "/payments/NOPE/resolve", map[string]any{"status": "ignored"}, nil); x.Status != 404 {
		t.Errorf("unknown payment: %d", x.Status)
	}
	// Filters.
	if j := r.call(r.view.Access, "GET", "/payments?status=refunded", nil, nil).JSON(t); len(j["items"].([]any)) != 1 {
		t.Errorf("refunded filter: %s", jsonNum(j))
	}
	if j := r.call(r.view.Access, "GET", "/payments?method=cash", nil, nil).JSON(t); len(j["items"].([]any)) != 1 {
		t.Errorf("method filter: %s", jsonNum(j))
	}
	today := time.Now().In(billing.EAT).Format("2006-01-02")
	if j := r.call(r.view.Access, "GET", "/payments?from="+today+"&to="+today, nil, nil).JSON(t); len(j["items"].([]any)) < 3 {
		t.Errorf("date filter: %s", jsonNum(j))
	}
	for _, bad := range []string{"status=zzz", "from=yesterday"} {
		if x := r.call(r.view.Access, "GET", "/payments?"+bad, nil, nil); x.Status != 400 {
			t.Errorf("%s: %d", bad, x.Status)
		}
	}
}

// ---- read endpoints --------------------------------------------------------------------------

func TestAPIReadEndpoints(t *testing.T) {
	r := newAPIRig(t)
	a := r.acct(t, r.A, "R-1", "AA:BB:CC:00:70:01")
	_ = r.acct(t, r.B, "R-2", "AA:BB:CC:00:70:02")
	hour := r.pkgID(t, "1 Hour")
	if err := r.svc.ExtendAccount(nilCtx(), a.ID, hour, "", false); err != nil {
		t.Fatal(err)
	}
	dash := r.call(r.view.Access, "GET", "/dashboard", nil, nil).JSON(t)
	for _, k := range []string{"sites_total", "sites_online", "accounts", "revenue", "unmatched_payments", "unregistered_devices", "open_alerts", "pending_router_tasks", "generated_at"} {
		if _, ok := dash[k]; !ok {
			t.Errorf("dashboard lacks %s", k)
		}
	}
	if dash["sites_total"] != float64(2) || dash["accounts"].(map[string]any)["active"] != float64(1) {
		t.Errorf("dashboard: %s", jsonNum(dash))
	}
	if j := r.call(r.view.Access, "GET", "/dashboard?site="+r.B.ID, nil, nil).JSON(t); j["accounts"].(map[string]any)["active"] != float64(0) {
		t.Errorf("dashboard by site: %s", jsonNum(j))
	}
	r.st.Update(func(d *store.Data) error {
		x := d.Sites[r.A.ID]
		x.TunnelIP = "10.99.0.7"
		d.Sites[r.A.ID] = x
		return nil
	})
	sites := r.call(r.view.Access, "GET", "/sites", nil, nil).JSON(t)["items"].([]any)
	if len(sites) != 2 {
		t.Fatalf("sites: %d", len(sites))
	}
	s0 := sites[0].(map[string]any)
	for _, k := range []string{"id", "name", "access_mode", "online", "cause", "tunnel", "tunnel_address", "accounts", "open_alerts"} {
		if _, ok := s0[k]; !ok {
			t.Errorf("site lacks %s", k)
		}
	}
	for _, it := range sites {
		m := it.(map[string]any)
		if m["id"] == r.A.ID && m["tunnel_address"] != "10.99.0.7" {
			t.Errorf("tunnel_address %v, want 10.99.0.7", m["tunnel_address"])
		}
		for k, v := range m { // no key material or password in the router JSON
			if lk := strings.ToLower(k); strings.Contains(lk, "private") || strings.Contains(lk, "rest_pass") || lk == "password" {
				t.Errorf("router JSON has %s=%v", k, v)
			}
		}
	}
	one := r.call(r.view.Access, "GET", "/sites/"+r.A.ID, nil, nil).JSON(t)
	if one["provision"] == nil || one["uptime_30d_pct"] == nil && one["uptime_30d_pct"] != float64(0) {
		if _, ok := one["uptime_30d_pct"]; !ok {
			t.Errorf("site detail: %s", jsonNum(one))
		}
	}
	if x := r.call(r.view.Access, "GET", "/sites/nope", nil, nil); x.Status != 404 {
		t.Errorf("unknown site: %d", x.Status)
	}
	for _, p := range []string{"24h", "7d", "30d"} {
		q := r.call(r.view.Access, "GET", "/sites/"+r.A.ID+"/qoe?period="+p, nil, nil)
		if q.Status != 200 {
			t.Fatalf("qoe %s: %d %s", p, q.Status, q.Raw)
		}
		j := q.JSON(t)
		for _, k := range []string{"latency_ms", "loss_pct", "wan", "series", "flags"} {
			if _, ok := j[k]; !ok {
				t.Errorf("qoe %s lacks %s", p, k)
			}
		}
		if s := j["series"].(map[string]any); s["t"] == nil || s["down_bps"] == nil || s["up_bps"] == nil || s["rtt_ms"] == nil {
			t.Errorf("qoe series: %v", s)
		}
	}
	if x := r.call(r.view.Access, "GET", "/sites/"+r.A.ID+"/qoe?period=1y", nil, nil); x.Status != 400 {
		t.Errorf("bad period: %d", x.Status)
	}
	onts := r.call(r.view.Access, "GET", "/sites/"+r.A.ID+"/onts", nil, nil).JSON(t)["items"].([]any)
	if len(onts) != 1 {
		t.Fatalf("onts: %v", onts)
	}
	o := onts[0].(map[string]any)
	if o["account_id"] != a.ID || o["mac"] != "AA:BB:CC:00:70:01" || o["lease_status"] == nil {
		t.Errorf("ont: %v", o)
	}
	if _, ok := o["optical"]; !ok {
		t.Error("ont lacks optical")
	}
	pk := r.call(r.view.Access, "GET", "/packages", nil, nil).JSON(t)["items"].([]any)
	if len(pk) == 0 {
		t.Fatal("no packages")
	}
	for _, k := range []string{"id", "name", "speed_mbps", "duration_min", "price_kes", "disabled"} {
		if _, ok := pk[0].(map[string]any)[k]; !ok {
			t.Errorf("package lacks %s", k)
		}
	}
	if x := r.call(r.view.Access, "GET", "/packages?site=nope", nil, nil); x.Status != 404 {
		t.Errorf("packages of an unknown site: %d", x.Status)
	}
	al := r.call(r.view.Access, "GET", "/alerts", nil, nil).JSON(t)
	for _, k := range []string{"active", "history", "mutes"} {
		if _, ok := al[k].([]any); !ok {
			t.Errorf("alerts.%s is not a list: %s", k, jsonNum(al))
		}
	}
	// Search.
	sr := r.call(r.view.Access, "GET", "/search?q=R-1", nil, nil).JSON(t)
	if len(sr["accounts"].([]any)) != 1 || sr["customers"] == nil || sr["payments"] == nil {
		t.Errorf("search: %s", jsonNum(sr))
	}
	if x := r.call(r.view.Access, "GET", "/search", nil, nil); x.Status != 400 {
		t.Errorf("search without q: %d", x.Status)
	}
	// Mute.
	if x := r.call(r.oper.Access, "POST", "/alerts/mute", map[string]any{"site_id": r.A.ID, "minutes": 30}, nil); x.Status != 204 {
		t.Errorf("mute: %d %s", x.Status, x.Raw)
	}
	if m := r.call(r.view.Access, "GET", "/alerts", nil, nil).JSON(t)["mutes"].([]any); len(m) != 1 {
		t.Errorf("mutes: %v", m)
	}
	if x := r.call(r.oper.Access, "POST", "/alerts/mute", map[string]any{"site_id": r.A.ID, "minutes": 0}, nil); x.Status != 204 {
		t.Errorf("unmute: %d", x.Status)
	}
	// Audit pages by cursor.
	au := r.call(r.owner.Access, "GET", "/audit?limit=2", nil, nil).JSON(t)
	if len(au["items"].([]any)) != 2 || au["next_cursor"] == nil {
		t.Fatalf("audit page: %s", jsonNum(au))
	}
	next := au["next_cursor"].(string)
	au2 := r.call(r.owner.Access, "GET", "/audit?limit=2&cursor="+next, nil, nil).JSON(t)
	if len(au2["items"].([]any)) == 0 || au2["items"].([]any)[0].(map[string]any)["seq"].(float64) >= au["items"].([]any)[1].(map[string]any)["seq"].(float64) {
		t.Errorf("audit cursor did not move on: %s", jsonNum(au2))
	}
}

// ---- unregistered devices (addendum H) -------------------------------------------------------

func TestAPIUnregistered(t *testing.T) {
	r := newAPIRig(t)
	noMAC := r.noMACAcct(t, r.A, "Empty-1")
	r.acct(t, r.A, "Has-2", "AA:BB:CC:00:80:02")
	r.svc.NoteDevice(r.A.ID, "AA:BB:CC:00:80:AA", "10.5.50.21", "dhcp")
	r.svc.NoteDevice(r.A.ID, "AA:BB:CC:00:80:AB", "10.5.50.22", "arp")
	r.svc.NoteDevice(r.B.ID, "AA:BB:CC:00:80:AC", "10.5.50.23", "portal")
	// A stored, online, hostnamed device, to see every field.
	if err := r.st.Update(func(d *store.Data) error {
		now := time.Now()
		for k, dv := range d.Devices {
			if dv.MAC == "AA:BB:CC:00:80:AA" {
				dv.Online, dv.HostName, dv.Vendor, dv.LastSeen = true, "Pixel-7", "Google", now
				d.Devices[k] = dv
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	j := r.call(r.view.Access, "GET", "/unregistered", nil, nil).JSON(t)
	items := j["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("unregistered: %s", jsonNum(j))
	}
	first := items[0].(map[string]any)
	if first["mac"] != "AA:BB:CC:00:80:AA" || first["online"] != true || first["site_name"] != "Alpha" || first["host_name"] != "Pixel-7" || first["vendor"] != "Google" {
		t.Errorf("first item (online first): %v", first)
	}
	for _, k := range []string{"site_id", "site_name", "mac", "vendor", "host_name", "ip", "source", "online", "first_seen", "last_seen", "hits", "suggested_account"} {
		if _, ok := first[k]; !ok {
			t.Errorf("item lacks %s", k)
		}
	}
	sg, _ := first["suggested_account"].(map[string]any)
	if sg["id"] != noMAC.ID || sg["flat"] != "Empty-1" || sg["code"] != noMAC.Code {
		t.Errorf("suggested_account must be the open account without a MAC: %v", first["suggested_account"])
	}
	// Building B has no candidate: null.
	for _, it := range items {
		m := it.(map[string]any)
		if m["site_id"] == r.B.ID && m["suggested_account"] != nil {
			t.Errorf("a suggestion in a building with no candidate: %v", m)
		}
	}
	on := r.call(r.view.Access, "GET", "/unregistered?online=true", nil, nil).JSON(t)["items"].([]any)
	if len(on) != 1 {
		t.Errorf("online=true: %d", len(on))
	}
	if site := r.call(r.view.Access, "GET", "/unregistered?site="+r.B.ID, nil, nil).JSON(t)["items"].([]any); len(site) != 1 {
		t.Errorf("site filter: %d", len(site))
	}
	if x := r.call(r.view.Access, "GET", "/unregistered?online=maybe", nil, nil); x.Status != 400 {
		t.Errorf("online=maybe: %d", x.Status)
	}
	// With nobody lacking a MAC, an account unseen for over a day is the suggestion.
	r.call(r.oper.Access, "PUT", "/accounts/"+noMAC.ID+"/mac", map[string]any{"mac": "AA:BB:CC:00:80:55"}, nil)
	if err := r.st.Update(func(d *store.Data) error {
		for id, a := range d.Accounts {
			a.CreatedAt = time.Now().Add(-72 * time.Hour)
			d.Accounts[id] = a
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first = r.call(r.view.Access, "GET", "/unregistered?site="+r.A.ID, nil, nil).JSON(t)["items"].([]any)[0].(map[string]any)
	if first["suggested_account"] == nil {
		t.Errorf("a stale account should be suggested: %v", first)
	}

	// Refresh.
	if x := r.call(r.oper.Access, "POST", "/sites/"+r.A.ID+"/unregistered/refresh", nil, nil); x.Status != 202 {
		t.Errorf("refresh: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/sites/nope/unregistered/refresh", nil, nil); x.Status != 404 {
		t.Errorf("refresh unknown site: %d", x.Status)
	}
	// Attach: into the account, then 409 for a MAC another account owns.
	target := r.noMACAcct(t, r.A, "Target-3")
	x := r.call(r.oper.Access, "POST", "/unregistered/attach", map[string]any{"site_id": r.A.ID, "mac": "AA:BB:CC:00:80:AB", "account_id": target.ID}, nil)
	if x.Status != 200 || x.JSON(t)["mac"] != "AA:BB:CC:00:80:AB" {
		t.Fatalf("attach: %d %s", x.Status, x.Raw)
	}
	other := r.noMACAcct(t, r.A, "Target-4")
	x = r.call(r.oper.Access, "POST", "/unregistered/attach", map[string]any{"site_id": r.A.ID, "mac": "AA:BB:CC:00:80:AB", "account_id": other.ID}, nil)
	if x.Status != 409 || x.errCode(t) != "mac_in_use" {
		t.Fatalf("attach a used mac: %d %s", x.Status, x.Raw)
	}
	if acc := x.JSON(t)["error"].(map[string]any)["details"].(map[string]any)["account"].(map[string]any); acc["flat"] != "Target-3" {
		t.Errorf("409 details: %v", acc)
	}
	if x := r.call(r.oper.Access, "POST", "/unregistered/attach", map[string]any{"site_id": r.B.ID, "mac": "AA:BB:CC:00:80:AC", "account_id": other.ID}, nil); x.Status != 400 {
		t.Errorf("attach to an account of another building: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/unregistered/attach", map[string]any{"site_id": r.A.ID, "mac": "bad", "account_id": other.ID}, nil); x.Status != 400 {
		t.Errorf("attach bad mac: %d", x.Status)
	}
	// Create from a device.
	x = r.call(r.oper.Access, "POST", "/unregistered/create", map[string]any{"apartment_id": aptOf(t, r.st, r.B.ID), "site_id": r.B.ID, "mac": "AA:BB:CC:00:80:AC", "flat": "New-9", "tenant": "Otieno", "phone": "0711000111", "package_id": r.pkgID(t, "1 Hour")}, nil)
	if x.Status != 201 || x.JSON(t)["flat"] != "New-9" || x.JSON(t)["mac"] != "AA:BB:CC:00:80:AC" || x.JSON(t)["package"] == nil {
		t.Fatalf("create: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/unregistered/create", map[string]any{"apartment_id": aptOf(t, r.st, r.B.ID), "site_id": r.B.ID, "mac": "AA:BB:CC:00:80:AC", "flat": "New-10"}, nil); x.Status != 409 || x.errCode(t) != "mac_in_use" {
		t.Errorf("create with a used mac: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/unregistered/create", map[string]any{"apartment_id": aptOf(t, r.st, r.B.ID), "site_id": r.B.ID, "mac": "AA:BB:CC:00:80:AD", "flat": "New-9"}, nil); x.Status != 409 || x.errCode(t) != "unit_in_use" {
		t.Errorf("create with a used flat: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/unregistered/create", map[string]any{"apartment_id": aptOf(t, r.st, r.B.ID), "site_id": r.B.ID, "mac": "AA:BB:CC:00:80:AD"}, nil); x.Status != 400 || x.errCode(t) != "validation_failed" {
		t.Errorf("create without a flat: %d %s", x.Status, x.Raw)
	}
	// Dismiss.
	r.svc.NoteDevice(r.A.ID, "AA:BB:CC:00:80:AE", "10.5.50.30", "dhcp")
	if x := r.call(r.oper.Access, "POST", "/unregistered/dismiss", map[string]any{"site_id": r.A.ID, "mac": "AA:BB:CC:00:80:AE"}, nil); x.Status != 204 {
		t.Errorf("dismiss: %d %s", x.Status, x.Raw)
	}
	for _, it := range r.call(r.view.Access, "GET", "/unregistered", nil, nil).JSON(t)["items"].([]any) {
		if it.(map[string]any)["mac"] == "AA:BB:CC:00:80:AE" {
			t.Error("a dismissed device is still listed")
		}
	}
}

// ---- headers, tunnel, logs -------------------------------------------------------------------

func TestAPIHeadersAndLogRedaction(t *testing.T) {
	r := newAPIRig(t)
	x := r.call(r.owner.Access, "GET", "/me", nil, nil)
	if x.Header.Get("Content-Type") != "application/json; charset=utf-8" || x.Header.Get("Cache-Control") == "" || x.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", x.Header)
	}
	if csp := x.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("api CSP: %q", csp)
	}
	r.call(r.owner.Access, "GET", "/auth/devices", nil, nil)
	r.call(r.owner.Access, "DELETE", "/auth/devices/"+r.view.DeviceID, nil, nil)
	r.call("", "POST", "/auth/refresh", map[string]any{"refresh_token": r.owner.Refresh}, nil)
	r.call("", "POST", "/auth/login", map[string]any{"username": "admin", "password": "leak-me-not-123", "totp": "123456", "device": map[string]any{}}, nil)
	r.adminGet(t, "/admin/security")
	logs := r.logs.String()
	if logs == "" {
		t.Skip("nothing was logged")
	}
	for _, secret := range []string{r.owner.Access, r.owner.Refresh, r.oper.Access, r.oper.Refresh, "leak-me-not-123", pw, longPW, r.br.Cookie.Value, r.br.CSRF} {
		if secret != "" && strings.Contains(logs, secret) {
			t.Fatalf("a secret reached the log: %q", secret)
		}
	}
	if strings.Contains(logs, "ftna_") || strings.Contains(logs, "ftnr_") {
		t.Fatal("a token reached the log")
	}
	r.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			for _, secret := range []string{r.owner.Access, r.owner.Refresh, "leak-me-not-123"} {
				if strings.Contains(e.Msg, secret) {
					t.Fatalf("a secret reached the audit log: %s", e.Msg)
				}
			}
		}
	})
}

func nilCtx() context.Context { return context.Background() }

// The account detail carries the bound MAC with its maker and the equipment register items; PATCH takes the
// WhatsApp number and the "same as the phone" flag, and the flag comes back.
func TestAPIAccountDetailDevicesAndWhatsApp(t *testing.T) {
	r := newAPIRig(t)
	x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": aptOf(t, r.st, r.A.ID), "site_id": r.A.ID, "flat": "A-201", "mac": "00:E0:4C:68:01:02", "tenant": "Wanjiku", "phone": "0712345678"}, nil)
	if x.Status != 201 {
		t.Fatalf("create: %d %s", x.Status, x.Raw)
	}
	acct := x.JSON(t)
	id := str(acct, "id")
	if acct["mac_vendor"] != "Realtek" {
		t.Errorf("mac_vendor %v", acct["mac_vendor"])
	}
	d := r.call(r.view.Access, "GET", "/accounts/"+id, nil, nil).JSON(t)
	dev, ok := d["device"].(map[string]any)
	if !ok || dev["mac"] != "00:E0:4C:68:01:02" || dev["vendor"] != "Realtek" || dev["online"] != false || dev["port"] != "" {
		t.Fatalf("device: %v", d["device"])
	}
	if eq, ok := d["equipment"].([]any); !ok || len(eq) != 0 {
		t.Fatalf("equipment: %v", d["equipment"])
	}
	if _, has := d["router"].(map[string]any)["port"]; !has {
		t.Error("router.port missing")
	}
	p := r.call(r.oper.Access, "PATCH", "/accounts/"+id, map[string]any{"wa_same_phone": true}, nil)
	if j := p.JSON(t); p.Status != 200 || j["wa_same_phone"] != true || j["whatsapp"] != "254712345678" {
		t.Fatalf("patch same: %d %s", p.Status, p.Raw)
	}
	d = r.call(r.view.Access, "GET", "/accounts/"+id, nil, nil).JSON(t)
	if d["wa_same_phone"] != true || d["whatsapp"] != d["phone"] {
		t.Fatalf("after reload: %v %v", d["wa_same_phone"], d["whatsapp"])
	}
	p = r.call(r.oper.Access, "PATCH", "/accounts/"+id, map[string]any{"phone": "0722000111"}, nil)
	if j := p.JSON(t); j["whatsapp"] != "254722000111" || j["wa_same_phone"] != true {
		t.Fatalf("WhatsApp did not follow the phone: %s", p.Raw)
	}
	p = r.call(r.oper.Access, "PATCH", "/accounts/"+id, map[string]any{"wa_same_phone": false, "whatsapp": "0733555666"}, nil)
	if j := p.JSON(t); j["wa_same_phone"] != false || j["whatsapp"] != "254733555666" {
		t.Fatalf("unticked: %s", p.Raw)
	}
	// a number typed on its own clears the box; an explicit wa_same_phone=true wins over a number sent with it
	r.call(r.oper.Access, "PATCH", "/accounts/"+id, map[string]any{"wa_same_phone": true}, nil)
	p = r.call(r.oper.Access, "PATCH", "/accounts/"+id, map[string]any{"whatsapp": "0733555666"}, nil)
	if j := p.JSON(t); j["wa_same_phone"] != false || j["whatsapp"] != "254733555666" {
		t.Fatalf("number alone: %s", p.Raw)
	}
	p = r.call(r.oper.Access, "PATCH", "/accounts/"+id, map[string]any{"wa_same_phone": true, "whatsapp": "0744777888"}, nil)
	if j := p.JSON(t); p.Status != 200 || j["wa_same_phone"] != true || j["whatsapp"] == "254744777888" || j["whatsapp"] != j["phone"] {
		t.Fatalf("flag with a number: %s", p.Raw)
	}
}
