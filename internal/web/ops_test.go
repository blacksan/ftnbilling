package web_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/alerts"
	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/fleet"
	"ispbill/internal/metrics"
	"ispbill/internal/notify"
	"ispbill/internal/sites"
	"ispbill/internal/store"
	"ispbill/internal/web"
)

type recSender struct {
	mu   sync.Mutex
	got  []notify.Message
	fail error
}

func (r *recSender) Send(_ context.Context, m notify.Message) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return "", r.fail
	}
	r.got = append(r.got, m)
	return "id1", nil
}

type opsRig struct {
	*multi
	ops  web.Ops
	snd  *recSender
	dial map[string]bool
	mu   sync.Mutex
}

type alertIn struct {
	st *store.Store
	fl *fleet.Prober
}

func (a alertIn) Sites() []store.Site {
	var out []store.Site
	a.st.View(func(d *store.Data) { out = d.SiteList() })
	return out
}
func (a alertIn) Fleet(id string) fleet.SiteState { return a.fl.State(id) }
func (a alertIn) Live(string) (metrics.Live, metrics.Config, bool) {
	return metrics.Live{}, metrics.Config{}, false
}

func newOpsRig(t *testing.T) *opsRig {
	t.Helper()
	r := &opsRig{snd: &recSender{}, dial: map[string]bool{}}
	r.multi = newMultiOps(t, func(c *config.Config) {
		c.WAToken, c.WAPhoneNumberID, c.WAAdminNumbers, c.WADailyLimit = "tok", "1234", []string{"254700111222"}, 240
		c.SMTPHost, c.SMTPFrom, c.AlertEmailTo = "smtp.example", "alerts@ftn.example", []string{"ops@ftn.example"}
		c.WAAppSecret, c.WAVerifyToken = "appsecret", "vtoken"
	}, func(cfg config.Config, st *store.Store, mgr *sites.Manager, svc *billing.Service) web.Ops {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		box, err := notify.Open(notify.Options{Path: filepath.Join(cfg.DataDir, "outbox.json"), Log: log,
			Senders: map[string]notify.Sender{notify.ChanEmail: r.snd, notify.ChanWhatsApp: r.snd}})
		if err != nil {
			t.Fatal(err)
		}
		fl := fleet.New(fleet.Options{Store: st, Net: mgr, Log: log, Dial: func(_ context.Context, addr string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.dial[addr] {
				return nil
			}
			return errors.New("refused")
		}})
		eng := alerts.New(alerts.Options{Path: filepath.Join(cfg.DataDir, "alerts.json"), In: alertIn{st, fl}, Out: box, Log: log,
			Targets: func() alerts.Targets { return alerts.Targets{Email: []string{"ops@ftn.example"}} }})
		ten := notify.NewTenant(notify.TenantOptions{Store: st, Box: box, Log: log, Policy: svc.Policy, PayHint: "M-Pesa Paybill 123456", MinConfirmKES: 250,
			OpenOutage: func(id string) (time.Time, bool) { o, ok := fl.OpenOutage(id); return o.Start, ok }, OutageEnd: fl.OutageEnd})
		in := notify.NewInbound(notify.InboundOptions{Store: st, Box: box, AppSecret: cfg.WAAppSecret, VerifyToken: cfg.WAVerifyToken, SupportPhone: "0700 111 222", PayHint: "M-Pesa Paybill 123456"})
		r.ops = web.Ops{Fleet: fl, Alerts: eng, Box: box, Tenant: ten, Inbound: in}
		return r.ops
	})
	return r
}

// joined adds a building with a tunnel address and marks its REST port up or down.
func (r *opsRig) joined(t *testing.T, name, ip string, up bool) store.Site {
	t.Helper()
	s := r.addSite(t, name)
	r.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.TunnelIP, x.RESTPort = ip, 8081
		d.Sites[s.ID] = x
		return nil
	})
	r.mu.Lock()
	r.dial[ip+":8081"] = up
	r.mu.Unlock()
	return r.st2(s.ID)
}

func (r *opsRig) st2(id string) (s store.Site) {
	r.st.View(func(d *store.Data) { s = d.Sites[id] })
	return
}

func (r *opsRig) probe(n int) {
	for i := 0; i < n; i++ {
		r.ops.Fleet.Tick(context.Background())
	}
}

func TestFleetPageAndLanding(t *testing.T) {
	r := newOpsRig(t)
	a := r.joined(t, "Alpha Court", "10.99.0.2", true)
	b := r.joined(t, "Beta Heights", "10.99.0.3", false)
	r.probe(3)
	// With two routers and "All", /admin is the Overview and the old fleet address is the Routers page.
	resp := r.admin("GET", "/admin", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("landing: %d", resp.StatusCode)
	}
	resp = r.admin("GET", "/admin/fleet", nil)
	resp.Body.Close()
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/admin/routers" {
		t.Fatalf("fleet: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	page := r.adminGet(t, "/admin/routers")
	for _, want := range []string{"Alpha Court", "Beta Heights", "Routers online", "Tell tenants about the outage",
		"cause unknown", `name="site" value="` + b.ID + `"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("fleet page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, `value="`+a.ID+`"><button>Tell tenants`) {
		t.Fatal("the up building has no outage button")
	}
	// A single building selected: the normal dashboard, and /admin/metrics works per building.
	r.selectSite(t, a.ID)
	if resp := r.admin("GET", "/admin", nil); resp.StatusCode != 200 {
		t.Fatalf("dashboard for one building: %d", resp.StatusCode)
	}
	// All + metrics goes to the fleet page.
	r.cookie = nil
	resp = r.admin("GET", "/admin/metrics", nil)
	resp.Body.Close()
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/admin/routers" {
		t.Fatalf("metrics with All: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Sidebar entries exist.
	for _, want := range []string{`href="/admin/uptime"`, `href="/admin/alerts"`, `href="/admin/messages"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("sidebar lacks %s", want)
		}
	}
}

func TestUptimePage(t *testing.T) {
	r := newOpsRig(t)
	a := r.joined(t, "Alpha Court", "10.99.0.2", true)
	r.joined(t, "Beta Heights", "10.99.0.3", true)
	r.probe(3)
	r.mu.Lock()
	r.dial["10.99.0.2:8081"] = false
	r.mu.Unlock()
	r.probe(3)
	page := r.adminGet(t, "/admin/uptime?site="+a.ID+"&days=30")
	for _, want := range []string{"Availability, last 30 days", `class="ustrip"`, "ongoing", "Alpha Court", "All routers", "Beta Heights", "Tunnel round-trip time"} {
		if !strings.Contains(page, want) {
			t.Fatalf("uptime page lacks %q", want)
		}
	}
	if !strings.Contains(r.adminGet(t, "/admin/uptime?site="+a.ID+"&days=90"), "last 90 days") {
		t.Fatal("90 day selector")
	}
}

func TestAlertsSettingsTestAndMute(t *testing.T) {
	r := newOpsRig(t)
	a := r.joined(t, "Alpha Court", "10.99.0.2", true)
	page := r.adminGet(t, "/admin/alerts")
	for _, want := range []string{"Open alerts", "Router offline", "WAN saturated", "Packet loss", "High latency", "OLT port down", "Send test", "Mute a router"} {
		if !strings.Contains(page, want) {
			t.Fatalf("alerts page lacks %q", want)
		}
	}
	form := url.Values{"rule_offline_on": {"1"}, "rule_offline_hold": {"7"}, "rule_wan_sat_on": {"1"}, "rule_wan_sat_thr": {"90"}, "rule_wan_sat_hold": {"20"},
		"rule_loss_on": {"1"}, "rule_loss_thr": {"4"}, "rule_loss_hold": {"10"}, "rule_latency_thr": {"200"}, "rule_latency_hold": {"10"},
		"rule_olt_down_on": {"1"}, "rule_olt_down_hold": {"2"}, "ch_email": {"1"}, "cooldown": {"30"}, "repeat_on": {"1"}, "repeat_hours": {"4"}, "notice": {"auto"}, "auto_min": {"45"}}
	resp := r.admin("POST", "/admin/alerts/settings", form)
	resp.Body.Close()
	set := r.ops.Alerts.Settings()
	if resp.StatusCode != 303 || set.Rules[alerts.RuleOffline].HoldMin != 7 || set.Rules[alerts.RuleWANSat].Threshold != 90 || set.Rules[alerts.RuleLatency].Enabled ||
		!set.Email || set.WhatsApp || set.CooldownMin != 30 || !set.RepeatEnabled || set.RepeatHours != 4 || set.OutageNotice != "auto" || set.AutoMin != 45 {
		t.Fatalf("%d %+v", resp.StatusCode, set)
	}
	// invalid numbers are refused and nothing changes
	form.Set("rule_loss_thr", "0")
	resp = r.admin("POST", "/admin/alerts/settings", form)
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "msg=Error") || r.ops.Alerts.Settings().Rules[alerts.RuleLoss].Threshold != 4 {
		t.Fatalf("%s", loc)
	}
	// test button: email has a recipient in the rig, WhatsApp has none
	resp = r.admin("POST", "/admin/alerts/test", url.Values{"channel": {"email"}})
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "Test+message+sent") || len(r.snd.got) != 1 || r.snd.got[0].To != "ops@ftn.example" || !strings.Contains(r.snd.got[0].Text, "Test alert") {
		t.Fatalf("%s %+v", resp.Header.Get("Location"), r.snd.got)
	}
	resp = r.admin("POST", "/admin/alerts/test", url.Values{"channel": {"whatsapp"}})
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "Error") {
		t.Fatal("whatsapp has no recipient in this rig")
	}
	// a failing channel shows the error
	r.snd.fail = errors.New("boom")
	resp = r.admin("POST", "/admin/alerts/test", url.Values{"channel": {"email"}})
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "failed") {
		t.Fatal(resp.Header.Get("Location"))
	}
	// mute and unmute
	resp = r.admin("POST", "/admin/alerts/mute", url.Values{"site": {a.ID}, "hours": {"4"}})
	resp.Body.Close()
	if len(r.ops.Alerts.Mutes()) != 1 || !strings.Contains(r.adminGet(t, "/admin/alerts"), "muted until") {
		t.Fatal("mute")
	}
	resp = r.admin("POST", "/admin/alerts/unmute", url.Values{"site": {a.ID}})
	resp.Body.Close()
	if len(r.ops.Alerts.Mutes()) != 0 {
		t.Fatal("unmute")
	}
}

func TestMessagesPage(t *testing.T) {
	r := newOpsRig(t)
	r.joined(t, "Alpha Court", "10.99.0.2", true)
	box := r.ops.Box
	m1, _ := box.Enqueue(notify.Message{Channel: notify.ChanEmail, To: "ops@ftn.example", Kind: notify.KindAdminAlert, Text: "x", Priority: 1})
	m2, _ := box.Enqueue(notify.Message{Channel: notify.ChanEmail, To: "ops@ftn.example", Kind: notify.KindAdminAlert, Text: "y", NotBefore: time.Now().Add(time.Hour)})
	page := r.adminGet(t, "/admin/messages")
	for _, want := range []string{"Waiting", "admin_alert", m1.ID, "held until", "Cancel"} {
		if want != m1.ID && !strings.Contains(page, want) {
			t.Fatalf("messages page lacks %q", want)
		}
	}
	// cancel then retry
	resp := r.admin("POST", "/admin/messages/cancel", url.Values{"id": {m2.ID}})
	resp.Body.Close()
	if g, _ := box.Get(m2.ID); g.Status != notify.StatusCancelled {
		t.Fatal("cancel")
	}
	resp = r.admin("POST", "/admin/messages/retry", url.Values{"id": {m2.ID}})
	resp.Body.Close()
	if g, _ := box.Get(m2.ID); g.Status != notify.StatusQueued {
		t.Fatal("retry")
	}
	// send them: the first goes, then a bad token pauses the channel and the banner shows
	box.Step(context.Background(), notify.ChanEmail)
	if g, _ := box.Get(m1.ID); g.Status != notify.StatusSent {
		t.Fatal("sent")
	}
	if !strings.Contains(r.adminGet(t, "/admin/messages?status=sent"), "sent") {
		t.Fatal("filter")
	}
	r.snd.fail = notify.Auth("smtp: 401 Unauthorized")
	box.Enqueue(notify.Message{Channel: notify.ChanEmail, To: "ops@ftn.example", Kind: notify.KindAdminAlert, Text: "z"})
	time.Sleep(2100 * time.Millisecond) // the channel's 2 s rate gap
	for i := 0; i < 3 && len(box.Paused()) == 0; i++ {
		box.Step(context.Background(), notify.ChanEmail)
		time.Sleep(300 * time.Millisecond)
	}
	page = r.adminGet(t, "/admin/messages")
	if !strings.Contains(page, "sending is paused") || !strings.Contains(page, "401 Unauthorized") {
		t.Fatalf("auth banner missing:\n%s", page)
	}
	resp = r.admin("POST", "/admin/messages/resume", url.Values{"channel": {"email"}})
	resp.Body.Close()
	if len(box.Paused()) != 0 {
		t.Fatal("resume")
	}
}

func TestOutageNoticeButton(t *testing.T) {
	r := newOpsRig(t)
	b := r.joined(t, "Beta Heights", "10.99.0.3", false)
	r.joined(t, "Alpha Court", "10.99.0.2", true)
	for i, wa := range []string{"254711000001", "254711000002", ""} {
		if _, err := r.svc.CreateAccount(billing.AccountInput{SiteID: b.ID, Flat: "Flat " + string(rune('A'+i)), MAC: "AA:BB:CC:00:00:0" + string(rune('1'+i)), WhatsApp: wa}); err != nil {
			t.Fatal(err)
		}
	}
	r.probe(3)
	resp := r.admin("POST", "/admin/sites/outage-notice", url.Values{"site": {b.ID}})
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "queued+for+2+tenants+in+Beta+Heights") {
		t.Fatalf("%s", resp.Header.Get("Location"))
	}
	got := r.ops.Box.List(notify.Filter{})
	if len(got) != 2 || got[0].Template != notify.TplOutageNotice || got[0].SiteID != b.ID {
		t.Fatalf("%+v", got)
	}
	// pressing it again for the same outage sends nothing new
	resp = r.admin("POST", "/admin/sites/outage-notice", url.Values{"site": {b.ID}})
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "already+told") || len(r.ops.Box.List(notify.Filter{})) != 2 {
		t.Fatalf("%s", resp.Header.Get("Location"))
	}
	// the notice count shows on the fleet page
	if !strings.Contains(r.adminGet(t, "/admin/routers"), "2 notices already sent") {
		t.Fatal("fleet page should count the notices")
	}
	// switched off: refused
	set := r.ops.Alerts.Settings()
	set.OutageNotice = "off"
	r.ops.Alerts.SetSettings(set)
	resp = r.admin("POST", "/admin/sites/outage-notice", url.Values{"site": {b.ID}})
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "switched+off") {
		t.Fatalf("%s", resp.Header.Get("Location"))
	}
}

func TestAccountWhatsAppForm(t *testing.T) {
	r := newOpsRig(t)
	a := r.joined(t, "Alpha Court", "10.99.0.2", true)
	r.selectSite(t, a.ID)
	acct, err := r.svc.CreateAccount(billing.AccountInput{SiteID: a.ID, Flat: "Flat 1", MAC: "AA:BB:CC:00:00:01", Phone: "0712345678"})
	if err != nil {
		t.Fatal(err)
	}
	page := r.adminGet(t, "/admin/accounts/view?code="+acct.Code)
	for _, want := range []string{`name="whatsapp"`, `name="wa_same"`, `name="wa_optout"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("account page lacks %s", want)
		}
	}
	resp := r.admin("POST", "/admin/accounts/update", url.Values{"id": {acct.ID}, "flat": {"Flat 1"}, "phone": {"0712345678"}, "wa_same": {"1"}, "wa_optout": {"1"}})
	resp.Body.Close()
	got, _ := r.svc.GetAccount(acct.ID)
	if got.WhatsApp != "254712345678" || !got.WAOptOut {
		t.Fatalf("%+v", got)
	}
	resp = r.admin("POST", "/admin/accounts/update", url.Values{"id": {acct.ID}, "flat": {"Flat 1"}, "phone": {"0712345678"}, "whatsapp": {"0722000111"}})
	resp.Body.Close()
	if got, _ = r.svc.GetAccount(acct.ID); got.WhatsApp != "254722000111" || got.WAOptOut {
		t.Fatalf("%+v", got)
	}
	resp = r.admin("POST", "/admin/accounts/update", url.Values{"id": {acct.ID}, "flat": {"Flat 1"}, "whatsapp": {"bad"}})
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "Error") {
		t.Fatal("a bad number is refused")
	}
	// create form
	resp = r.admin("POST", "/admin/accounts/create", url.Values{"site": {a.ID}, "flat": {"Flat 2"}, "mac": {"AA:BB:CC:00:00:02"}, "phone": {"0733111222"}, "wa_same": {"1"}})
	resp.Body.Close()
	var n int
	r.st.View(func(d *store.Data) {
		for _, x := range d.Accounts {
			if x.Flat == "Flat 2" && x.WhatsApp == "254733111222" {
				n++
			}
		}
	})
	if n != 1 {
		t.Fatal("create with WhatsApp same as phone")
	}
}

func TestWhatsAppWebhookRoutes(t *testing.T) {
	r := newOpsRig(t)
	a := r.joined(t, "Alpha Court", "10.99.0.2", true)
	acct, _ := r.svc.CreateAccount(billing.AccountInput{SiteID: a.ID, Flat: "Flat 1", MAC: "AA:BB:CC:00:00:01", WhatsApp: "0712345678"})
	get := func(q string) (int, string) {
		resp, err := http.Get(r.ts.URL + "/wa/webhook?" + q)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if code, b := get("hub.mode=subscribe&hub.verify_token=vtoken&hub.challenge=777"); code != 200 || b != "777" {
		t.Fatalf("handshake: %d %q", code, b)
	}
	if code, _ := get("hub.mode=subscribe&hub.verify_token=nope&hub.challenge=777"); code != 403 {
		t.Fatalf("wrong token: %d", code)
	}
	post := func(body []byte, sig string) int {
		req, _ := http.NewRequest("POST", r.ts.URL+"/wa/webhook", bytes.NewReader(body))
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	body := []byte(`{"entry":[{"changes":[{"value":{"messages":[{"from":"254712345678","id":"wamid.1","type":"text","text":{"body":"STOP"}}]}}]}]}`)
	if code := post(body, ""); code != 403 {
		t.Fatalf("unsigned: %d", code)
	}
	if code := post(body, notify.Sign("wrong", body)); code != 403 {
		t.Fatalf("badly signed: %d", code)
	}
	if got, _ := r.svc.GetAccount(acct.ID); got.WAOptOut {
		t.Fatal("an unsigned request must change nothing")
	}
	if code := post(body, notify.Sign("appsecret", body)); code != 200 {
		t.Fatalf("signed: %d", code)
	}
	if got, _ := r.svc.GetAccount(acct.ID); !got.WAOptOut {
		t.Fatal("STOP over the webhook sets the opt-out")
	}
	if code := post([]byte("not json"), notify.Sign("appsecret", []byte("not json"))); code != 200 {
		t.Fatalf("a signed bad payload still gets 200: %d", code)
	}
}

func TestWebhookNotRegisteredWithoutSecrets(t *testing.T) {
	m := newMulti(t) // no Ops, no secrets
	resp, err := http.Get(m.ts.URL + "/wa/webhook?hub.mode=subscribe&hub.verify_token=x&hub.challenge=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("the route must not exist: %d", resp.StatusCode)
	}
}

// L-5: an operator cannot cancel a queued alert to the owners; an owner can.
func TestOperatorCannotCancelAdminAlerts(t *testing.T) {
	r := newOpsRig(t)
	op := seedUser(t, r.srv, r.st, "olga", store.RoleOperator, longPW)
	ob := signIn(t, r.ts, r.srv, r.st, op)
	cl := &http.Client{CheckRedirect: noRedirect}
	box := r.ops.Box
	alert, _ := box.Enqueue(notify.Message{Channel: notify.ChanEmail, To: "ops@ftn.example", Kind: notify.KindAdminAlert, Text: "3 wrong authenticator codes", NotBefore: time.Now().Add(time.Hour)})
	note, _ := box.Enqueue(notify.Message{Channel: notify.ChanEmail, To: "t@example.com", Kind: notify.KindExpiryReminder, Text: "expiring", NotBefore: time.Now().Add(time.Hour)})
	resp := ob.Do(cl, r.ts.URL, "POST", "/admin/messages/cancel", url.Values{"id": {alert.ID}})
	resp.Body.Close()
	if g, _ := box.Get(alert.ID); g.Status != notify.StatusQueued {
		t.Fatalf("an operator cancelled an alert to the owners: %s", g.Status)
	}
	if !strings.Contains(resp.Header.Get("Location"), "Error") {
		t.Errorf("no error shown: %q", resp.Header.Get("Location"))
	}
	resp = ob.Do(cl, r.ts.URL, "POST", "/admin/messages/cancel", url.Values{"id": {note.ID}})
	resp.Body.Close()
	if g, _ := box.Get(note.ID); g.Status != notify.StatusCancelled {
		t.Errorf("operators may still cancel tenant messages: %s", g.Status)
	}
	resp = r.admin("POST", "/admin/messages/cancel", url.Values{"id": {alert.ID}})
	resp.Body.Close()
	if g, _ := box.Get(alert.ID); g.Status != notify.StatusCancelled {
		t.Errorf("the owner could not cancel: %s", g.Status)
	}
}
