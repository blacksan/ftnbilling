package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/metrics"
	"ispbill/internal/payments"
	"ispbill/internal/routeros"
	"ispbill/internal/sites"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
	"ispbill/internal/web"
	"ispbill/internal/wg"
)

// fakeDaraja answers the three Daraja calls the pay flow makes and keeps the last STK push.
type fakeDaraja struct {
	mu   sync.Mutex
	stk  map[string]any
	pubs int
}

func (f *fakeDaraja) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/oauth/v1/generate":
		w.Write([]byte(`{"access_token":"TOK","expires_in":"3599"}`))
	case "/mpesa/stkpush/v1/processrequest":
		b, _ := io.ReadAll(r.Body)
		f.stk = map[string]any{}
		json.Unmarshal(b, &f.stk)
		f.pubs++
		w.Write([]byte(`{"MerchantRequestID":"m1","CheckoutRequestID":"ws_CO_CAPTIVE","ResponseCode":"0","ResponseDescription":"ok"}`))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDaraja) lastSTK() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stk
}

const (
	capToken  = "cbtoken0123456789"
	capWANIP  = "198.51.100.77" // the router's WAN address: where every walled flat appears to come from
	capAdmins = "10.99.0.1"
)

// captive is the whole story in one process: a joined router (mock) whose WireGuard peer dials from capWANIP, one
// apartment, one flat that has just run out of time, a real M-Pesa provider talking to a fake Daraja, and the web app.
type captive struct {
	*app
	rt     *routeros.Mock
	daraja *fakeDaraja
	site   store.Site
	acct   store.Account
	now    time.Time
	mu     sync.Mutex
	dump   string
}

func (c *captive) clock() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *captive) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newCaptive(t *testing.T, mpesaOn bool) *captive { return newCaptiveCfg(t, mpesaOn, nil) }

// newCaptiveCfg is newCaptive with the chance to change the server config (the support contacts, say).
func newCaptiveCfg(t *testing.T, mpesaOn bool, mod func(*config.Config)) *captive {
	t.Helper()
	dir := t.TempDir()
	_, pub, _ := wg.GenerateKeypair()
	_, sitePub, _ := wg.GenerateKeypair()
	c := &captive{now: time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC), daraja: &fakeDaraja{}, dump: filepath.Join(dir, "wg0.dump")}
	cfg := config.Config{BrandName: "Fast Tech Net", AdminUser: "admin", AdminPass: pw, SupportPhone: "0100 133 133",
		NetflowAddr: "off", PingTargets: []string{"8.8.8.8"}, WGPublicKey: pub, WGEndpoint: "203.0.113.10:51820", WGSubnet: "10.99.0.0/22",
		DataKey: testKey, JoinTokenTTL: 30 * time.Minute, PublicIP: "203.0.113.10", PublicBaseURL: "https://isp.ftw.co.ke", DataDir: dir, WGDumpPath: c.dump,
		FlowEvery: time.Hour, PortEvery: time.Hour, PingEvery: time.Hour, DNSEvery: time.Hour, SaveEvery: time.Hour}
	if mod != nil {
		mod(&cfg)
	}
	st, _ := storetest.Open(t, dir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c.rt = routeros.NewMock()
	var svc *billing.Service
	mgr := sites.New(sites.Options{Cfg: cfg, Store: st, Log: log, DataKey: cfg.DataKey,
		Dir:       func(id string) metrics.Directory { return svc.Directory(id) },
		NewClient: func(store.Site) routeros.Client { return c.rt }})
	svc = billing.NewService(st, mgr, log)
	svc.SetClock(c.clock)
	svc.SeedPackages()
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Wait(); svc.Wait() })
	daraja := httptest.NewServer(c.daraja)
	t.Cleanup(daraja.Close)
	mp := payments.NewMpesa(payments.MpesaConfig{BaseURL: daraja.URL, Key: "k", Secret: "s", Shortcode: "174379", Passkey: "PK",
		PublicBaseURL: "https://isp.ftw.co.ke", CallbackToken: capToken, Enabled: mpesaOn}, svc, st, log)
	mp.SetClock(c.clock)
	prov := map[string]payments.Provider{"voucher": payments.Voucher{C: svc}, "mpesa": mp, "test": &payments.Test{}}
	srv := web.New(cfg, st, svc, prov, mp, mgr, log)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c.app = &app{ts: ts, st: st, svc: svc, mgr: mgr, srv: srv, cl: &http.Client{CheckRedirect: noRedirect}}
	c.owner = seedUser(t, srv, st, "admin", store.RoleOwner, pw)
	c.br = signIn(t, ts, srv, st, c.owner)
	c.csrf = c.br.CSRF

	site, err := mgr.CreateSite(sites.SiteInput{Name: "Riverside Court"})
	if err != nil {
		t.Fatal(err)
	}
	st.Update(func(d *store.Data) error {
		s := d.Sites[site.ID]
		s.WGPublicKey, s.AccessMode, s.SupportPhone = sitePub, store.AccessIPoE, "0722 000 111"
		d.Sites[site.ID] = s
		return nil
	})
	mgr.Reload(site.ID)
	st.View(func(d *store.Data) { c.site = d.Sites[site.ID] })
	apt := apartmentFor(t, st, site.ID, "Riverside Court")
	c.rt.SeedRow("/ip/dhcp-server", routeros.Row{"name": "ftn-dhcp", "disabled": "false"})
	c.rt.SeedRow("/interface/bridge", routeros.Row{"name": "br-customers", "comment": "ftn: customer bridge"})
	c.setPeer(site.ID, capWANIP)

	c.acct, err = svc.CreateAccount(billing.AccountInput{ApartmentID: apt, SiteID: site.ID, Flat: "Flat 4B", MAC: "AA:BB:CC:11:22:33", Tenant: "Wanjiru", Phone: "0712 345 678"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ExtendAccount(t.Context(), c.acct.ID, c.pkg("1 Hour"), "", false); err != nil {
		t.Fatal(err)
	}
	c.advance(90 * time.Minute) // the hour is over
	c.reconcile(t)
	c.bindLease(t)
	return c
}

func (c *captive) setLease(t *testing.T, id, status string) {
	t.Helper()
	if err := c.rt.SetRow(t.Context(), "/ip/dhcp-server/lease", id, map[string]string{"status": status}); err != nil {
		t.Fatal(err)
	}
}

// bindLease is the flat's device taking its fixed address from the router's DHCP server.
func (c *captive) bindLease(t *testing.T) {
	t.Helper()
	a := c.mustAcct(t)
	for _, r := range c.rt.Table("/ip/dhcp-server/lease") {
		if strings.EqualFold(r["mac-address"], a.MAC) {
			c.setLease(t, r[routeros.RowID], "bound")
			return
		}
	}
	t.Fatal("no lease row for the flat")
}

func (c *captive) pkg(name string) string { return c.pkgID(name) }

// setPeer writes the wg status dump the way the root timer does: the router's WireGuard peer and where it dials from.
func (c *captive) setPeer(siteID, ip string) {
	var key string
	c.st.View(func(d *store.Data) { key = d.Sites[siteID].WGPublicKey })
	line := fmt.Sprintf("%s\t%s:51820\t10.99.0.2/32\t%d\t1000\t2000\n", key, ip, time.Now().Unix())
	if err := os.WriteFile(c.dump, []byte(line), 0o644); err != nil {
		panic(err)
	}
}

func (c *captive) reconcile(t *testing.T) {
	t.Helper()
	if n := c.svc.RunSiteTasks(t.Context(), c.site.ID); n == 0 {
		if _, err := c.svc.ReconcileSite(t.Context(), c.site.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func (c *captive) suspendedIPs() (ips []string) {
	for _, r := range c.rt.Table("/ip/firewall/address-list") {
		if r["list"] == billing.SuspendedList {
			ips = append(ips, r["address"])
		}
	}
	return
}

// visit is a request as a walled flat's browser makes it: the router's masquerade gives it capWANIP, Caddy writes
// that into X-Forwarded-For, and the Host is whatever site the device asked for.
func (c *captive) visit(method, path, host string, form url.Values) *http.Response {
	var rd io.Reader
	if form != nil {
		rd = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, c.ts.URL+path, rd)
	if host != "" {
		req.Host = host
	}
	req.Header.Set("X-Forwarded-For", capWANIP)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "ftn_pay_csrf", Value: payCSRFValue})
	}
	r, err := c.cl.Do(req)
	if err != nil {
		panic(err)
	}
	return r
}

func (c *captive) pay(t *testing.T, path string, form url.Values) (int, string, *http.Response) {
	t.Helper()
	form.Set("_pcsrf", payCSRFValue)
	r := c.visit("POST", path, "isp.ftw.co.ke", form)
	loc := r.Header.Get("Location")
	b := body(t, r)
	_ = loc
	return r.StatusCode, b, r
}

func (c *captive) callback(t *testing.T, receipt string, amount int) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"Body": map[string]any{"stkCallback": map[string]any{
		"MerchantRequestID": "m1", "CheckoutRequestID": "ws_CO_CAPTIVE", "ResultCode": 0, "ResultDesc": "ok",
		"CallbackMetadata": map[string]any{"Item": []map[string]any{
			{"Name": "Amount", "Value": amount}, {"Name": "MpesaReceiptNumber", "Value": receipt},
			{"Name": "TransactionDate", "Value": 20261119230000}, {"Name": "PhoneNumber", "Value": 254712345678}}}}}})
	r, err := http.Post(c.ts.URL+"/pay/cb/t/"+capToken+"/stk", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return body(t, r)
}

func (c *captive) orderStatus(t *testing.T, id string) (status string, online bool) {
	t.Helper()
	r := c.visit("GET", "/api/orders/"+id, "isp.ftw.co.ke", nil)
	var v struct {
		Status string `json:"status"`
		Online bool   `json:"online"`
	}
	if err := json.Unmarshal([]byte(body(t, r)), &v); err != nil {
		t.Fatal(err)
	}
	return v.Status, v.Online
}

func priceOf(c *captive, name string) (n int) {
	c.st.View(func(d *store.Data) {
		for _, p := range d.Packages {
			if p.Name == name {
				n = p.PriceKES
			}
		}
	})
	return
}

// A walled flat's captive-portal probe (the router sent its plain-HTTP request here with the probe's own Host) is
// answered with a redirect to the payment page, never with the 204 or "Success" that mean "you are online".
func TestCaptiveProbesRedirectToPay(t *testing.T) {
	c := newCaptive(t, true)
	for _, p := range []struct{ host, path string }{
		{"captive.apple.com", "/hotspot-detect.html"},
		{"connectivitycheck.gstatic.com", "/generate_204"},
		{"clients3.google.com", "/generate_204"},
		{"www.msftconnecttest.com", "/connecttest.txt"},
		{"detectportal.firefox.com", "/success.txt"},
		{"captive.apple.com", "/"},
		{"isp.ftw.co.ke", "/"},
	} {
		r := c.visit("GET", p.path, p.host, nil)
		r.Body.Close()
		if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/pay" {
			t.Errorf("%s%s: %d -> %q", p.host, p.path, r.StatusCode, r.Header.Get("Location"))
		}
	}
}

// The whole customer story: the plan ended and the flat is walled; the page knows the building, shows the account
// and what ended, lists the packages with prices and speeds, takes the M-Pesa prompt, and the Daraja callback credits
// the account, takes the flat off the suspended list and turns the page to "back online".
func TestCaptiveExpiredFlatPaysWithMpesaAndComesBack(t *testing.T) {
	c := newCaptive(t, true)
	if ips := c.suspendedIPs(); len(ips) != 1 || ips[0] != c.mustAcct(t).IP {
		t.Fatalf("an expired flat must be on the suspended list: %v", ips)
	}
	// The page recognises the building from the router's WireGuard endpoint and asks for the flat.
	r := c.visit("GET", "/pay", "isp.ftw.co.ke", nil)
	html := body(t, r)
	for _, want := range []string{"Riverside Court", "Your internet is paused", "0722 000 111"} {
		if !strings.Contains(html, want) {
			t.Errorf("landing lacks %q", want)
		}
	}
	// A stranger on the internet is not recognised as a building.
	req, _ := http.NewRequest("GET", c.ts.URL+"/pay", nil)
	req.Header.Set("X-Forwarded-For", "192.0.2.9")
	r2, _ := c.cl.Do(req)
	if h := body(t, r2); strings.Contains(h, "Riverside Court") || strings.Contains(h, "Your internet is paused") {
		t.Error("an unknown address was treated as the building")
	}
	// The account code: shows the flat, when the plan ended, every package with price and speed, the phone prefilled.
	code, html, _ := c.pay(t, "/pay/find", url.Values{"code": {c.acct.Code}})
	if code != 200 {
		t.Fatalf("find: %d %s", code, html)
	}
	for _, want := range []string{"Flat 4B", "Your plan ended at", "Choose a package", "Use the number on file", "07•• ••• 678", "Pay with M-Pesa", "Mbps", "Have a voucher?"} {
		if !strings.Contains(html, want) {
			t.Errorf("package step lacks %q", want)
		}
	}
	for _, name := range []string{"1 Hour", "1 Day", "Monthly 40 Mbps", "Monthly 80 Mbps"} {
		if !strings.Contains(html, name) {
			t.Errorf("package %q is not offered", name)
		}
	}
	for _, full := range []string{"0712345678", "712345678", "254712345678", "0712 345 678"} {
		if strings.Contains(html, full) {
			t.Errorf("the package page carries the full number %q", full)
		}
	}
	// The flat label alone shows a package list but no state and no phone number: a neighbour could type it.
	_, byFlat, _ := c.pay(t, "/pay/find", url.Values{"code": {"Flat 4B"}})
	if !strings.Contains(byFlat, "Choose a package") || strings.Contains(byFlat, `07•• ••• 678`) || strings.Contains(byFlat, "Your plan ended") {
		t.Errorf("by flat label must show packages only:\n%s", byFlat)
	}
	// Pay a day with a different (editable) phone number, typed in local format.
	day := c.pkg("1 Day")
	st, _, resp := c.pay(t, "/pay/start", url.Values{"code": {c.acct.Code}, "package": {day}, "method": {"mpesa"}, "phone_src": {"other"}, "phone": {"0799 111 222"}})
	if st != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/portal/wait?order=") {
		t.Fatalf("start: %d %s", st, resp.Header.Get("Location"))
	}
	orderID := strings.TrimPrefix(resp.Header.Get("Location"), "/portal/wait?order=")
	stk := c.daraja.lastSTK()
	if stk["PhoneNumber"] != "254799111222" || stk["Amount"] != float64(priceOf(c, "1 Day")) || stk["PartyA"] != "254799111222" {
		t.Fatalf("the push sent to Daraja: %v", stk)
	}
	if cb, _ := stk["CallBackURL"].(string); !strings.HasPrefix(cb, "https://isp.ftw.co.ke/pay/cb/") {
		t.Fatalf("callback url %q", cb)
	}
	if s, online := c.orderStatus(t, orderID); s != "pending" || online {
		t.Fatalf("before the callback: %s %v", s, online)
	}
	if len(c.suspendedIPs()) != 1 {
		t.Fatal("nothing may change on the router before the money arrives")
	}
	// Daraja's callback: wrong token refused, short amount does not credit, the real one does, a repeat changes nothing.
	r3, _ := http.Post(c.ts.URL+"/pay/cb/t/wrong/stk", "application/json", strings.NewReader("{}"))
	r3.Body.Close()
	if r3.StatusCode != 404 {
		t.Errorf("callback with a wrong token: %d", r3.StatusCode)
	}
	c.callback(t, "RKT9SHORT1", 1)
	if s, _ := c.orderStatus(t, orderID); s == "paid" {
		t.Fatal("a short payment must not mark the order paid")
	}
	if got, _ := c.svc.GetAccount(c.acct.ID); got.ExpiresAt.After(c.clock()) {
		t.Fatal("a short payment credited the account")
	}
	c.callback(t, "RKT9PAID01", priceOf(c, "1 Day"))
	got, _ := c.svc.GetAccount(c.acct.ID)
	exp := got.ExpiresAt
	if !exp.After(c.clock()) {
		t.Fatalf("the callback did not credit the account: %v", exp)
	}
	c.callback(t, "RKT9PAID01", priceOf(c, "1 Day")) // Daraja retries
	if again, _ := c.svc.GetAccount(c.acct.ID); !again.ExpiresAt.Equal(exp) {
		t.Fatal("a repeated callback credited twice")
	}
	// The payment reconciles the router at once: the flat is off the suspended list and the page says online.
	if ips := c.suspendedIPs(); len(ips) != 0 {
		t.Fatalf("still walled after paying: %v", ips)
	}
	if s, online := c.orderStatus(t, orderID); s != "paid" || !online {
		t.Fatalf("after the router: %s %v", s, online)
	}
	r4 := c.visit("GET", "/portal/success?order="+orderID, "isp.ftw.co.ke", nil)
	if h := body(t, r4); !strings.Contains(h, "back online") || !strings.Contains(h, "Flat 4B") || !strings.Contains(h, "/api/orders/"+orderID) {
		t.Errorf("success page: %s", h)
	}
	// And when the next plan ends, the flat is walled again.
	c.advance(25 * time.Hour)
	c.reconcile(t)
	if ips := c.suspendedIPs(); len(ips) != 1 {
		t.Fatalf("expired again but not walled: %v", ips)
	}
}

func (c *captive) mustAcct(t *testing.T) store.Account {
	a, ok := c.svc.GetAccount(c.acct.ID)
	if !ok {
		t.Fatal("account gone")
	}
	return a
}

// A voucher brings the flat back too.
func TestCaptiveVoucherUnwalls(t *testing.T) {
	c := newCaptive(t, true)
	_, vs, err := c.svc.GenerateVouchers(c.pkg("1 Day"), c.site.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	st, _, resp := c.pay(t, "/pay/voucher", url.Values{"code": {c.acct.Code}, "voucher": {vs[0].Code}})
	if st != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/portal/success?order=") {
		t.Fatalf("voucher: %d %s", st, resp.Header.Get("Location"))
	}
	c.svc.RunSiteTasks(t.Context(), c.site.ID)
	if ips := c.suspendedIPs(); len(ips) != 0 {
		t.Fatalf("still walled after a voucher: %v", ips)
	}
}

// M-Pesa switched off: the page says so in words (and offers the voucher), and a forged push is refused without a crash.
func TestCaptiveWithoutMpesaSaysSo(t *testing.T) {
	c := newCaptive(t, false)
	_, html, _ := c.pay(t, "/pay/find", url.Values{"code": {c.acct.Code}})
	if !strings.Contains(html, "Online payment is not available right now. Call 0722 000 111") || strings.Contains(html, "Pay with M-Pesa") || !strings.Contains(html, "Have a voucher?") {
		t.Errorf("page without M-Pesa:\n%s", html)
	}
	st, html, _ := c.pay(t, "/pay/start", url.Values{"code": {c.acct.Code}, "package": {c.pkg("1 Day")}, "method": {"mpesa"}, "phone": {"0712345678"}})
	if st != 400 || !strings.Contains(html, "not available") {
		t.Errorf("forged push: %d", st)
	}
	if c.daraja.pubs != 0 {
		t.Error("a push reached Daraja although M-Pesa is off")
	}
	r, _ := http.Post(c.ts.URL+"/pay/cb/t/"+capToken+"/stk", "application/json", strings.NewReader("{}"))
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Errorf("callback while M-Pesa is off: %d", r.StatusCode)
	}
}

// A device on another router than its account is told where it is, and nothing is sent.
func TestCaptiveWrongRouterIsRefused(t *testing.T) {
	c := newCaptive(t, true)
	_, otherPub, _ := wg.GenerateKeypair()
	other := seedSite(t, c.st, "Other Block")
	c.st.Update(func(d *store.Data) error {
		s := d.Sites[other]
		s.WGPublicKey = otherPub
		d.Sites[other] = s
		return nil
	})
	// The visitor's address is now the other router's, and the account's own router dials from somewhere else.
	line := func(key, ip string) string {
		return fmt.Sprintf("%s\t%s:51820\t10.99.0.2/32\t%d\t1\t2\n", key, ip, time.Now().Unix())
	}
	var ownKey string
	c.st.View(func(d *store.Data) { ownKey = d.Sites[c.site.ID].WGPublicKey })
	_ = os.WriteFile(c.dump, []byte(line(otherPub, capWANIP)+line(ownKey, "198.51.100.200")), 0o644)
	st, html, _ := c.pay(t, "/pay/start", url.Values{"code": {c.acct.Code}, "package": {c.pkg("1 Day")}, "method": {"mpesa"}, "phone": {"0712345678"}})
	if st != 409 || !strings.Contains(html, "Other Block") || c.daraja.pubs != 0 {
		t.Errorf("wrong router: %d pushes=%d\n%s", st, c.daraja.pubs, html)
	}
}

// The router is unreachable when the money arrives: the account is credited anyway, the page does not claim "online",
// and the queued task takes the flat off the suspended list as soon as the router answers.
func TestCaptivePaidWhileRouterDownComesBackLater(t *testing.T) {
	c := newCaptive(t, true)
	st, _, resp := c.pay(t, "/pay/start", url.Values{"code": {c.acct.Code}, "package": {c.pkg("1 Day")}, "method": {"mpesa"}, "phone": {"0712345678"}})
	if st != 303 {
		t.Fatalf("start: %d", st)
	}
	orderID := strings.TrimPrefix(resp.Header.Get("Location"), "/portal/wait?order=")
	c.rt.SetFail(fmt.Errorf("tunnel down"))
	c.callback(t, "RKT9DOWN01", priceOf(c, "1 Day"))
	if got, _ := c.svc.GetAccount(c.acct.ID); !got.ExpiresAt.After(c.clock()) {
		t.Fatal("the payment must be credited even though the router is down")
	}
	if s, online := c.orderStatus(t, orderID); s != "paid" || online {
		t.Fatalf("router down: %s online=%v", s, online)
	}
	c.rt.SetFail(nil)
	c.advance(10 * time.Minute) // past the retry back-off
	if n := c.svc.RunSiteTasks(t.Context(), c.site.ID); n != 1 {
		t.Fatalf("task run: %d", n)
	}
	if ips := c.suspendedIPs(); len(ips) != 0 {
		t.Fatalf("still walled: %v", ips)
	}
	if s, online := c.orderStatus(t, orderID); s != "paid" || !online {
		t.Fatalf("router back: %s online=%v", s, online)
	}
}

// "Use the number on file" (the default): the page holds only the masked number; the server fills in the real one.
func TestCaptiveUseNumberOnFile(t *testing.T) {
	c := newCaptive(t, true)
	st, _, resp := c.pay(t, "/pay/start", url.Values{"code": {c.acct.Code}, "package": {c.pkg("1 Day")}, "method": {"mpesa"}, "phone_src": {"file"}})
	if st != 303 {
		t.Fatalf("start: %d", st)
	}
	if stk := c.daraja.lastSTK(); stk["PhoneNumber"] != "254712345678" {
		t.Fatalf("the number on file was not used: %v", stk)
	}
	_ = resp
	// Through the token step too (the page after "Continue" carries a token, not the code).
	_, html, _ := c.pay(t, "/pay/find", url.Values{"code": {c.acct.Code}})
	i := strings.Index(html, `name="tok" value="`)
	tok := html[i+len(`name="tok" value="`):]
	tok = tok[:strings.Index(tok, `"`)]
	c.advance(time.Minute)
	if st, _, _ := c.pay(t, "/pay/start", url.Values{"tok": {tok}, "package": {c.pkg("1 Week")}, "method": {"mpesa"}, "phone_src": {"file"}}); st != 303 {
		t.Fatalf("token start: %d", st)
	}
	if stk := c.daraja.lastSTK(); stk["PhoneNumber"] != "254712345678" {
		t.Fatalf("token flow: %v", stk)
	}
	// A flat label never gets the number on file, even if the form asks for it.
	_, byFlat, _ := c.pay(t, "/pay/find", url.Values{"code": {"Flat 4B"}})
	j := strings.Index(byFlat, `name="tok" value="`)
	ftok := byFlat[j+len(`name="tok" value="`):]
	ftok = ftok[:strings.Index(ftok, `"`)]
	st, html, _ = c.pay(t, "/pay/start", url.Values{"tok": {ftok}, "package": {c.pkg("1 Day")}, "method": {"mpesa"}, "phone_src": {"file"}})
	if st != 400 {
		t.Errorf("a flat label used the number on file: %d", st)
	}
}

// "You're back online" is only said when the flat can really be online: not for an account the reconciler skips, and
// not while its device has no bound lease on this router.
func TestCaptiveOnlineNeedsAProvisionedBoundFlat(t *testing.T) {
	cases := []struct {
		name   string
		online bool
		break_ func(c *captive, t *testing.T)
	}{
		{"healthy flat", true, func(*captive, *testing.T) {}},
		{"no MAC", false, func(c *captive, t *testing.T) {
			c.st.Update(func(d *store.Data) error { a := d.Accounts[c.acct.ID]; a.MAC = ""; d.Accounts[a.ID] = a; return nil })
		}},
		{"flagged duplicate MAC", false, func(c *captive, t *testing.T) {
			c.st.Update(func(d *store.Data) error {
				a := d.Accounts[c.acct.ID]
				a.MACConflict = true
				d.Accounts[a.ID] = a
				return nil
			})
		}},
		{"router does not serve the apartment", false, func(c *captive, t *testing.T) {
			c.st.Update(func(d *store.Data) error {
				for k, v := range d.ApartmentSites {
					if v.SiteID == c.site.ID {
						delete(d.ApartmentSites, k)
					}
				}
				return nil
			})
		}},
		{"ONT behind another router (lease not bound here)", false, func(c *captive, t *testing.T) {
			for _, r := range c.rt.Table("/ip/dhcp-server/lease") {
				c.setLease(t, r[routeros.RowID], "waiting")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCaptive(t, true)
			_, vs, err := c.svc.GenerateVouchers(c.pkg("1 Day"), c.site.ID, 1)
			if err != nil {
				t.Fatal(err)
			}
			_, _, resp := c.pay(t, "/pay/voucher", url.Values{"code": {c.acct.Code}, "voucher": {vs[0].Code}})
			orderID := strings.TrimPrefix(resp.Header.Get("Location"), "/portal/success?order=")
			tc.break_(c, t)
			c.advance(time.Second)
			if _, err := c.svc.ReconcileSite(t.Context(), c.site.ID); err != nil {
				t.Fatal(err)
			}
			if _, online := c.orderStatus(t, orderID); online != tc.online {
				t.Fatalf("online = %v, want %v", online, tc.online)
			}
		})
	}
}
