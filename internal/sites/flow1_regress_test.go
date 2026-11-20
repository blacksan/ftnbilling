package sites

import (
	"context"
	"errors"
	"ispbill/internal/config"
	"ispbill/internal/routeros"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/store"
)

// peers.conf is rewritten when the server starts, so a crash between the store commit and ApplyPeers does not
// leave a router unknown to wg0 (or a removed router's old key still authorised).
func TestRouterFlow_PeersNotReappliedAtStart(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	peers := filepath.Join(f.cfg.DataDir, "wg", "peers.conf")
	b, err := os.ReadFile(peers)
	if err != nil || !strings.Contains(string(b), f.site(s.ID).WGPublicKey) {
		t.Fatalf("setup: peers file should hold the key: %v", err)
	}
	// simulate: the server died after the store commit and before ApplyPeers
	os.RemoveAll(filepath.Join(f.cfg.DataDir, "wg"))
	f.start() // what main.go does at boot: mgr.Start
	time.Sleep(200 * time.Millisecond)
	b, err = os.ReadFile(peers)
	if err != nil || !strings.Contains(string(b), f.site(s.ID).WGPublicKey) {
		t.Fatalf("after a restart peers.conf is missing the key of %s: %v", s.ID, err)
	}
}

// A script that reports failure is "stalled" with the report text and the next step, not "wait two minutes" for ever.
func TestRouterFlow_FailedReportNeverBecomesStalled(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	tok, _, _ := f.m.MintJoin(s.ID)
	if _, _, err := f.m.ConsumeJoin(tok, "RouterOS 7.25.1", "1.2.3.4", false); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Minute)
	if err := f.m.SubmitJoinReport(tok, []byte("result fail ros 7.25.1\n5 DHCP: failure: already have such name")); err != nil {
		t.Fatal(err)
	}
	f.advance(3 * time.Hour)
	v, _ := f.m.SetupView(nil, s.ID)
	if v.State != StateStalled || !strings.Contains(v.NextAction.Text, "already have such name") || v.NextAction.Kind != "paste_line" {
		t.Fatalf("state %s, next action %q: a router whose script aborted is told to wait for ever", v.State, v.NextAction.Text)
	}
	if !strings.Contains(v.LastError, "already have such name") {
		t.Fatalf("last_error %q", v.LastError)
	}
}

// A static WAN needs real host addresses: the gateway is not the router's own address, nor the network or broadcast address.
func TestRouterFlow_StaticWANAddressesMustBeHosts(t *testing.T) {
	f := newFixture(t, nil)
	for name, in := range map[string]SiteInput{
		"gw == own address": {Name: "A", WANMode: WANStatic, WANAddress: "203.0.113.10/29", WANGateway: "203.0.113.10"},
		"gw == network":     {Name: "B", WANMode: WANStatic, WANAddress: "203.0.113.10/29", WANGateway: "203.0.113.8"},
		"gw == broadcast":   {Name: "C", WANMode: WANStatic, WANAddress: "203.0.113.10/29", WANGateway: "203.0.113.15"},
		"addr == network":   {Name: "D", WANMode: WANStatic, WANAddress: "203.0.113.8/29", WANGateway: "203.0.113.9"},
		"addr == broadcast": {Name: "E", WANMode: WANStatic, WANAddress: "203.0.113.15/29", WANGateway: "203.0.113.9"},
	} {
		if _, err := f.m.CreateSite(in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := f.m.CreateSite(SiteInput{Name: "Good", WANMode: WANStatic, WANAddress: "203.0.113.10/29", WANGateway: "203.0.113.9"}); err != nil {
		t.Fatalf("a valid static WAN was refused: %v", err)
	}
}

// Resetting a router keeps the owner's pause of the automatic fixes (and still drops the join report).
func TestRouterFlow_ResetKeepsPause(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	if err := f.m.SetAutoFixPause(s.ID, true, time.Time{}, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.ResetRouter(s.ID, "Block A", "owner"); err != nil {
		t.Fatal(err)
	}
	if !f.m.AutoFixPause(s.ID).Paused {
		t.Fatal("the reset removed the pause")
	}
}

type pauseOnWrite struct {
	*routeros.Mock
	f     *fixture
	id    string
	once  sync.Once
	after int
	mu    sync.Mutex
}

func (p *pauseOnWrite) note() {
	p.once.Do(func() { p.f.m.SetAutoFixPause(p.id, true, time.Time{}, "owner") })
}
func (p *pauseOnWrite) SetRow(ctx context.Context, path, id string, diff map[string]string) error {
	p.mu.Lock()
	paused := p.f.m.AutoFixPause(p.id).Paused
	if paused {
		p.after++
	}
	p.mu.Unlock()
	err := p.Mock.SetRow(ctx, path, id, diff)
	p.note()
	return err
}
func (p *pauseOnWrite) AddRow(ctx context.Context, path string, body map[string]string) (string, error) {
	p.mu.Lock()
	if p.f.m.AutoFixPause(p.id).Paused {
		p.after++
	}
	p.mu.Unlock()
	r, err := p.Mock.AddRow(ctx, path, body)
	p.note()
	return r, err
}

// The owner presses "pause automatic fixes" while a pass is running: the pass stops writing.
func TestRouterFlow_PauseDoesNotStopARunningPass(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.NetflowAddr = "0.0.0.0:2055" })
	f.m.autoFixDelay = time.Hour
	s := f.joined("Block A")
	mock := routeros.NewMock()
	pw := &pauseOnWrite{Mock: mock, f: f, id: s.ID}
	f.m.opt.NewClient = func(store.Site) routeros.Client { return pw }
	f.start()
	f.m.Reload(s.ID)
	seedHealthy(t, f, s, mock)
	mock.SeedRow("/ip/address", R{"address": "10.5.50.1/24", "interface": "br-customers"})
	mock.SeedOne("/ip/traffic-flow", R{"enabled": "false", "interfaces": "all"})
	for _, r := range mock.Table("/ip/dhcp-server") {
		mock.SetRow(context.Background(), "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "true"})
	}
	out := f.m.AutoMaintain(context.Background(), s.ID)
	t.Logf("fixes applied in the pass: %d, writes made AFTER the pause was saved: %d", len(out), pw.after)
	if pw.after > 0 {
		t.Fatalf("pause was saved but the pass wrote %d more times", pw.after)
	}
}

// A manual fix waits on the sweep's lock only as long as its own context allows.
func TestRouterFlow_ManualFixBlocksPastItsDeadline(t *testing.T) {
	f, s, _ := checkRig(t)
	e := f.m.cport.entry(s.ID)
	e.run.Lock()
	go func() { time.Sleep(1500 * time.Millisecond); e.run.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := f.m.ApplyFix(ctx, s.ID, CheckDHCP, false, "operator")
	el := time.Since(t0)
	if el > 1*time.Second || !errors.Is(err, ErrRouterBusy) {
		t.Fatalf("ApplyFix blocked %v past a 200ms deadline (err=%v)", el, err)
	}
}

// A script that cannot be rendered (PUBLIC_IP missing) leaves a trace the admin can see: activity, log, Setup, Status.
func TestRouterFlow_CannotRenderLeavesNoTrace(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.PublicIP = "" })
	s := f.create("Block A")
	tok, _, err := f.m.MintJoin(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.m.ConsumeJoin(tok, "RouterOS 7.25.1", "1.2.3.4", false)
	t.Logf("ConsumeJoin: %v", err)
	var evs []string
	f.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.SiteID == s.ID {
				evs = append(evs, e.Msg)
			}
		}
	})
	v, _ := f.m.SetupView(context.Background(), s.ID)
	st, _ := f.m.Status(context.Background(), s.ID)
	if v.LastError == "" || len(evs) == 0 || st.RenderError == "" {
		t.Fatalf("the reason is recorded nowhere the admin can see: events %q last_error %q status %q", evs, v.LastError, st.RenderError)
	}
	// a repeated fetch does not flood the activity list with the same reason
	f.m.ConsumeJoin(tok, "RouterOS 7.25.1", "1.2.3.4", false)
	n := 0
	f.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.SiteID == s.ID && strings.Contains(e.Msg, "could not be made") {
				n++
			}
		}
	})
	if n != 1 {
		t.Fatalf("%d events for the same reason", n)
	}
}

// The "remote WinBox" flag does not depend on the one-shot join report alone: the checks read the rule from the router.
func TestRouterFlow_MgmtWinboxDependsOnOneShotReport(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	tok, _, _ := f.m.MintJoin(s.ID)
	script, _, err := f.m.ConsumeJoin(tok, "RouterOS 7.25.1", "1.2.3.4", false)
	if err != nil || !strings.Contains(string(script), "ftn: mgmt winbox") {
		t.Fatalf("script lacks the mgmt rule: %v", err)
	}
	// the report POST is lost: the router still has the rule, and the next read of the router shows it
	if f.m.MgmtWinbox(s.ID) {
		t.Fatal("no report and no read yet: unknown is false")
	}
	mock := routeros.NewMock()
	f.m.opt.NewClient = func(store.Site) routeros.Client { return mock }
	f.start()
	f.m.Reload(s.ID)
	seedHealthy(t, f, s, mock)
	mock.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "accept", "protocol": "tcp", "dst-port": "8291", "src-address": f.m.mgmt.String(), "in-interface": WGInterface, "comment": MgmtWinboxComment})
	if _, err := f.m.Checks(context.Background(), s.ID, true); err != nil {
		t.Fatal(err)
	}
	if !f.m.MgmtWinbox(s.ID) {
		t.Fatal("the router has the remote-WinBox rule but the page says it was built without")
	}
}

type hangRows struct{ *routeros.Mock }

func (h hangRows) Rows(ctx context.Context, path string, q map[string]string) ([]routeros.Row, error) {
	select {
	case <-ctx.Done():
	case <-time.After(300 * time.Millisecond):
	}
	return nil, context.DeadlineExceeded
}
func (h hangRows) RowsProps(ctx context.Context, path string, q map[string]string, p []string) ([]routeros.Row, error) {
	return h.Rows(ctx, path, q)
}

// The sweep does not hold the router's writer lock while it reads an unresponsive router: saving the form is not blocked.
func TestRouterFlow_SweepOnDeadRouterBlocksEdit(t *testing.T) {
	old := readTimeout
	readTimeout = 400 * time.Millisecond
	defer func() { readTimeout = old }()
	f := newFixture(t, nil)
	f.m.autoFixDelay = time.Hour
	s := f.joined("Block A")
	f.m.opt.NewClient = func(store.Site) routeros.Client { return hangRows{routeros.NewMock()} }
	f.start()
	f.m.Reload(s.ID)
	done := make(chan struct{})
	go func() { f.m.SweepSite(context.Background(), s.ID); close(done) }()
	time.Sleep(100 * time.Millisecond)
	t0 := time.Now()
	err := f.m.UpdateSite(s.ID, SiteInput{Name: "Block A", Notes: "edited"})
	el := time.Since(t0)
	<-done
	if err != nil || el > time.Second {
		t.Fatalf("a notes edit waited %v for the sweep", el)
	}
}

// A "fail" report is posted over the WAN before the first handshake: for the first two minutes the page keeps
// waiting (a non-fatal section may fail on a router that is about to join); then it says stalled.
func TestRouterFlow_FailedReportWaitsForTheTunnelFirst(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	tok, _, _ := f.m.MintJoin(s.ID)
	if _, _, err := f.m.ConsumeJoin(tok, "RouterOS 7.25.1", "1.2.3.4", false); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Minute)
	if err := f.m.SubmitJoinReport(tok, []byte("result fail ros 7.25.1\n5 NTP: failure: no server")); err != nil {
		t.Fatal(err)
	}
	f.advance(30 * time.Second)
	if v, _ := f.m.SetupView(nil, s.ID); v.State == StateStalled {
		t.Fatalf("30 s after a failed report the page already says stalled")
	}
	f.advance(3 * time.Minute)
	if v, _ := f.m.SetupView(nil, s.ID); v.State != StateStalled {
		t.Fatalf("3 min after a failed report with no tunnel the state is %q, want stalled", v.State)
	}
}
