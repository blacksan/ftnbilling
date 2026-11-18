package sites

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/metrics"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
	"ispbill/internal/wg"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fixture struct {
	t     *testing.T
	cfg   config.Config
	st    *store.Store
	m     *Manager
	now   time.Time
	mu    sync.Mutex
	mocks map[string]*routeros.Mock
	joins []string
}

func newFixture(t *testing.T, mut func(*config.Config)) *fixture {
	t.Helper()
	_, serverPub, _ := wg.GenerateKeypair()
	cfg := config.Config{DataDir: t.TempDir(), PublicBaseURL: "https://billing.example.co.ke", PublicIP: "203.0.113.10",
		WGPublicKey: serverPub, WGEndpoint: "203.0.113.10:51820", WGSubnet: "10.99.0.0/22", WGDumpPath: filepath.Join(t.TempDir(), "wg0.dump"),
		JoinTokenTTL: 30 * time.Minute, JoinRequireMikrotikUA: true, NetflowAddr: "off", BrandName: "FTN",
		WANInterface: "ether1", OLTInterface: "sfp-sfpplus1", WANCapacityMbps: 500, OLTCapacityMbps: 10000, FUPTB: 15, CustomerSubnet: "10.5.50.0/24",
		PingTargets: []string{"8.8.8.8"}, FlowEvery: time.Hour, PortEvery: time.Hour, PingEvery: time.Hour, DNSEvery: time.Hour, SaveEvery: time.Hour,
		RouterUser: "ispbill", RouterPass: "pw", HotspotServer: "hotspot1"}
	if mut != nil {
		mut(&cfg)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, cfg: cfg, st: st, now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), mocks: map[string]*routeros.Mock{}}
	f.m = New(Options{Cfg: cfg, Store: st, Log: quiet(), Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
		Dir: func(string) metrics.Directory { return nil },
		NewClient: func(s store.Site) routeros.Client {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.mocks[s.ID] == nil {
				f.mocks[s.ID] = routeros.NewMock()
			}
			return f.mocks[s.ID]
		},
		OnJoined: func(id string) { f.mu.Lock(); f.joins = append(f.joins, id); f.mu.Unlock() }})
	return f
}

func (f *fixture) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

func (f *fixture) site(id string) (s store.Site) {
	f.st.View(func(d *store.Data) { s = d.Sites[id] })
	return
}

func (f *fixture) create(name string) store.Site {
	f.t.Helper()
	s, err := f.m.CreateSite(SiteInput{Name: name, Provision: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) start() context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	f.m.Start(ctx)
	f.t.Cleanup(func() { cancel(); f.m.Wait() })
	return cancel
}

const mt = "MikroTik"

func TestJoinTokenFlow(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	if s.Mode != store.SiteModeWG || s.TunnelIP != "" || s.WGPublicKey != "" || s.ID == "" {
		t.Fatalf("%+v", s)
	}
	tok, exp, err := f.m.MintJoin(s.ID)
	if err != nil || len(tok) != 43 || !exp.Equal(f.now.Add(30*time.Minute)) {
		t.Fatalf("%q %v %v", tok, exp, err)
	}
	// Only the hash is stored: the plaintext token is nowhere in the data file.
	raw, _ := os.ReadFile(filepath.Join(f.cfg.DataDir, "ispbill.json"))
	if strings.Contains(string(raw), tok) || !strings.Contains(string(raw), HashToken(tok)) {
		t.Fatal("the token must be stored as a hash only")
	}
	// A link-preview bot (wrong User-Agent) does not consume it.
	if _, _, err := f.m.ConsumeJoin(tok, "WhatsApp/2.23", "1.2.3.4"); err != ErrJoinInvalid {
		t.Fatalf("bot: %v", err)
	}
	if got := f.site(s.ID); !got.Join.UsedAt.IsZero() || got.WGPublicKey != "" {
		t.Fatal("a bot used the token up")
	}
	if _, _, err := f.m.ConsumeJoin("not-the-token", mt, "1.2.3.4"); err != ErrJoinInvalid {
		t.Fatalf("wrong token: %v", err)
	}
	script, id, err := f.m.ConsumeJoin(tok, "MikroTik/7.20 (fetch)", "198.51.100.7")
	if err != nil || id != s.ID {
		t.Fatalf("%v %s", err, id)
	}
	got := f.site(s.ID)
	if got.TunnelIP != "10.99.0.2" || !wg.ValidKey(got.WGPublicKey) || len(got.RESTPass) != 32 || got.RESTUser != "ftn-api" || got.RESTPort != 8081 ||
		got.Join.UsedAt.IsZero() || got.Join.UsedFrom != "198.51.100.7" {
		t.Fatalf("%+v", got)
	}
	// The script carries the private key of the stored public key and the password; neither is in the store.
	m := regexp.MustCompile(`private-key="([^"]+)"`).FindStringSubmatch(string(script))
	if m == nil {
		t.Fatal("script has no private key")
	}
	if pub, _ := wg.PublicKey(m[1]); pub != got.WGPublicKey {
		t.Fatal("stored public key does not match the script's private key")
	}
	raw, _ = os.ReadFile(filepath.Join(f.cfg.DataDir, "ispbill.json"))
	if strings.Contains(string(raw), m[1]) {
		t.Fatal("the router's private key was stored")
	}
	if !strings.Contains(string(script), `password="`+got.RESTPass+`"`) || !strings.Contains(string(script), "10.99.0.2") {
		t.Fatal("script lacks the REST password or tunnel address")
	}
	// Single use.
	if _, _, err := f.m.ConsumeJoin(tok, mt, "1.2.3.4"); err != ErrJoinInvalid {
		t.Fatalf("second use: %v", err)
	}
	// The peers file lists the router, and the status job is told to apply it.
	peers, _ := os.ReadFile(filepath.Join(f.cfg.DataDir, "wg", "peers.conf"))
	if !strings.Contains(string(peers), got.WGPublicKey) || !strings.Contains(string(peers), "10.99.0.2/32") {
		t.Fatalf("peers.conf: %s", peers)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.DataDir, "wg", "apply")); err != nil {
		t.Fatal("apply trigger missing")
	}
	f.mu.Lock()
	joins := append([]string(nil), f.joins...)
	f.mu.Unlock()
	if len(joins) != 1 || joins[0] != s.ID {
		t.Fatalf("OnJoined: %v", joins)
	}
	// A second building takes the next address.
	b := f.create("Block B")
	tokB, _, _ := f.m.MintJoin(b.ID)
	if _, _, err := f.m.ConsumeJoin(tokB, mt, "x"); err != nil {
		t.Fatal(err)
	}
	if f.site(b.ID).TunnelIP != "10.99.0.3" {
		t.Fatalf("%+v", f.site(b.ID))
	}
	// Rotation: a new token replaces the old one; keys change only when the new script is fetched.
	before := f.site(s.ID)
	tok2, _, _ := f.m.MintJoin(s.ID)
	if f.site(s.ID).WGPublicKey != before.WGPublicKey || f.site(s.ID).RESTPass != before.RESTPass {
		t.Fatal("minting must not change the live keys")
	}
	if _, _, err := f.m.ConsumeJoin(tok, mt, "x"); err != ErrJoinInvalid {
		t.Fatal("the old token must be dead after a new mint")
	}
	if _, _, err := f.m.ConsumeJoin(tok2, mt, "x"); err != nil {
		t.Fatal(err)
	}
	after := f.site(s.ID)
	if after.WGPublicKey == before.WGPublicKey || after.RESTPass == before.RESTPass || after.TunnelIP != before.TunnelIP {
		t.Fatalf("rotation must renew keys and keep the address: %+v", after)
	}
}

func TestJoinTokenExpiryAndRevoke(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	tok, _, _ := f.m.MintJoin(s.ID)
	f.advance(29 * time.Minute)
	if st, _ := f.m.Status(context.Background(), s.ID); st.Token != TokenPending {
		t.Fatalf("%+v", st)
	}
	f.advance(2 * time.Minute)
	if st, _ := f.m.Status(context.Background(), s.ID); st.Token != TokenExpired {
		t.Fatalf("%+v", st)
	}
	if _, _, err := f.m.ConsumeJoin(tok, mt, "x"); err != ErrJoinInvalid {
		t.Fatalf("expired: %v", err)
	}
	if f.site(s.ID).WGPublicKey != "" {
		t.Fatal("expired token changed the site")
	}
	tok, _, _ = f.m.MintJoin(s.ID)
	if err := f.m.RevokeJoin(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.ConsumeJoin(tok, mt, "x"); err != ErrJoinInvalid {
		t.Fatalf("revoked: %v", err)
	}
	if st, _ := f.m.Status(context.Background(), s.ID); st.Token != TokenNone {
		t.Fatalf("%+v", st)
	}
}

func TestJoinSettings(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.PublicBaseURL = "http://billing.example.co.ke" })
	s := f.create("Block A")
	if _, _, err := f.m.MintJoin(s.ID); err != ErrInsecureBase {
		t.Fatalf("http base: %v", err)
	}
	f = newFixture(t, func(c *config.Config) {
		c.PublicBaseURL = "http://10.0.0.5"
		c.AllowInsecureJoin = true
		c.JoinRequireMikrotikUA = false
	})
	s = f.create("Block A")
	tok, _, err := f.m.MintJoin(s.ID)
	if err != nil {
		t.Fatalf("bench override: %v", err)
	}
	if _, _, err := f.m.ConsumeJoin(tok, "curl/8", "x"); err != nil {
		t.Fatalf("UA check disabled: %v", err)
	}
	f = newFixture(t, func(c *config.Config) { c.WGPublicKey = "" })
	s = f.create("Block A")
	if _, _, err := f.m.MintJoin(s.ID); err == nil || !strings.Contains(err.Error(), "WG_PUBLIC_KEY") {
		t.Fatalf("no server key: %v", err)
	}
	if _, _, err := f.m.MintJoin("nope"); err == nil {
		t.Fatal("unknown site")
	}
}

func TestConcurrentConsumeUsesTokenOnce(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	tok, _, _ := f.m.MintJoin(s.ID)
	var wgp sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wgp.Add(1)
		go func() {
			defer wgp.Done()
			if _, _, err := f.m.ConsumeJoin(tok, mt, "x"); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wgp.Wait()
	if ok != 1 {
		t.Fatalf("%d consumers succeeded", ok)
	}
}

func TestCreateSiteValidation(t *testing.T) {
	f := newFixture(t, nil)
	f.create("Block A")
	for name, in := range map[string]SiteInput{
		"empty name":     {Name: " "},
		"duplicate name": {Name: "block a"},
		"bad iface":      {Name: "X", OLTInterface: `x"y`},
		"bad vlan":       {Name: "X", VLANID: 5000},
		"bad subnet":     {Name: "X", CustomerNet: "10.0.0.0/8"},
	} {
		if _, err := f.m.CreateSite(in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	s, err := f.m.CreateSite(SiteInput{Name: "Block B", VLANID: 30, CustomerNet: "10.5.51.0/24", WANCapMbps: 200})
	if err != nil || s.VLANID != 30 || s.WANCapMbps != 200 || s.OLTInterface != "sfp-sfpplus1" || s.RESTPort != 8081 || s.RESTUser != "ftn-api" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestManagerLifecycle(t *testing.T) {
	f := newFixture(t, nil)
	envID := store.NewSiteID()
	f.st.Update(func(d *store.Data) error {
		d.Sites[envID] = store.Site{ID: envID, Name: "Main building", Mode: store.SiteModeEnv}
		return nil
	})
	a := f.create("Block A")
	tok, _, _ := f.m.MintJoin(a.ID)
	f.start()
	if f.m.Collector(a.ID) != nil || f.m.Router(a.ID).Ping(context.Background()) != routeros.ErrNoRouter {
		t.Fatal("an unjoined building must have no runtime")
	}
	if f.m.Collector(envID) == nil || f.m.Router(envID).Ping(context.Background()) != nil {
		t.Fatal("env building must run")
	}
	if f.m.Router("nope").Ping(context.Background()) != routeros.ErrNoRouter {
		t.Fatal("unknown site")
	}
	// Joining starts the runtime (join consumed -> ApplyPeers + Reload).
	if _, _, err := f.m.ConsumeJoin(tok, mt, "x"); err != nil {
		t.Fatal(err)
	}
	if f.m.Collector(a.ID) == nil || f.m.Router(a.ID).Ping(context.Background()) != nil {
		t.Fatal("joined building must run")
	}
	// The guard is always on for wg buildings; the env building only with VERIFY_HOTSPOT_HOST.
	if f.m.Guard(a.ID) == nil || f.m.Guard(envID) != nil {
		t.Fatal("guard policy")
	}
	f.mocks[a.ID].AddHost("AA:BB:CC:DD:EE:01", "10.5.50.20")
	ctx := context.Background()
	if !f.m.VerifyHost(ctx, a.ID, "AA:BB:CC:DD:EE:01", "10.5.50.20") {
		t.Fatal("device on A's hotspot")
	}
	time.Sleep(1100 * time.Millisecond)
	if f.m.VerifyHost(ctx, a.ID, "AA:BB:CC:DD:EE:02", "10.5.50.21") {
		t.Fatal("unknown device accepted")
	}
	if !f.m.VerifyHost(ctx, envID, "AA:BB:CC:DD:EE:99", "") {
		t.Fatal("env building without VERIFY_HOTSPOT_HOST trusts the request")
	}
	if f.m.VerifyHost(ctx, "nope", "AA:BB:CC:DD:EE:01", "") {
		t.Fatal("unknown site must fail closed")
	}
	// Disabling stops the runtime and saves its metrics; the peer stays.
	if err := f.m.SetDisabled(a.ID, true); err != nil {
		t.Fatal(err)
	}
	if f.m.Collector(a.ID) != nil || f.m.Router(a.ID).Ping(ctx) != routeros.ErrNoRouter {
		t.Fatal("disabled building still running")
	}
	if _, err := os.Stat(MetricsPath(f.cfg.DataDir, a.ID)); err != nil {
		t.Fatalf("metrics not saved on stop: %v", err)
	}
	peers, _ := os.ReadFile(filepath.Join(f.cfg.DataDir, "wg", "peers.conf"))
	if !strings.Contains(string(peers), f.site(a.ID).WGPublicKey) {
		t.Fatal("a paused building keeps its peer")
	}
	if f.m.VerifyHost(ctx, a.ID, "AA:BB:CC:DD:EE:01", "10.5.50.20") {
		t.Fatal("a paused wg building has no guard: fail closed")
	}
	if err := f.m.SetDisabled(a.ID, false); err != nil {
		t.Fatal(err)
	}
	if f.m.Collector(a.ID) == nil {
		t.Fatal("resume")
	}
	// Edit restarts it.
	c1 := f.m.Collector(a.ID)
	if err := f.m.UpdateSite(a.ID, SiteInput{Name: "Block A2", WANInterface: "ether2", Provision: true}); err != nil {
		t.Fatal(err)
	}
	if f.m.Collector(a.ID) == c1 || f.m.Collector(a.ID).Config().WAN != "ether2" || f.site(a.ID).Name != "Block A2" {
		t.Fatal("update must rebuild the collector")
	}
	if cfg := f.m.Collector(a.ID).Config(); len(cfg.PortAllow) != 3 || cfg.SiteID != a.ID {
		t.Fatalf("wg collector config: %+v", cfg)
	}
	if cfg := f.m.Collector(envID).Config(); len(cfg.PortAllow) != 0 {
		t.Fatal("env collector keeps the legacy port filter")
	}
}

func TestRemoveSite(t *testing.T) {
	f := newFixture(t, nil)
	a := f.create("Block A")
	tok, _, _ := f.m.MintJoin(a.ID)
	f.start()
	f.m.ConsumeJoin(tok, mt, "x")
	joined := f.site(a.ID)
	f.st.Update(func(d *store.Data) error {
		d.Accounts["x"] = store.Account{ID: "x", SiteID: a.ID, Flat: "Flat 1"}
		d.Tasks["t"] = store.Task{ID: "t", SiteID: a.ID}
		return nil
	})
	if err := f.m.RemoveSite(a.ID, "Block A"); err != ErrSiteHasAccounts {
		t.Fatalf("with accounts: %v", err)
	}
	f.st.Update(func(d *store.Data) error { delete(d.Accounts, "x"); return nil })
	if err := f.m.RemoveSite(a.ID, "wrong name"); err == nil {
		t.Fatal("name must be typed to confirm")
	}
	if err := f.m.RemoveSite(a.ID, "Block A"); err != nil {
		t.Fatal(err)
	}
	got := f.site(a.ID)
	if !got.Removed || got.Name != "Block A" || got.RESTPass != "" || got.WGPublicKey != "" || got.TunnelIP != "" || got.Join.Hash != "" {
		t.Fatalf("tombstone must keep the name and drop everything else: %+v", got)
	}
	if f.m.Collector(a.ID) != nil {
		t.Fatal("runtime still running")
	}
	if _, err := os.Stat(MetricsPath(f.cfg.DataDir, a.ID)); !os.IsNotExist(err) {
		t.Fatal("metrics file must be deleted")
	}
	peers, _ := os.ReadFile(filepath.Join(f.cfg.DataDir, "wg", "peers.conf"))
	if strings.Contains(string(peers), joined.WGPublicKey) {
		t.Fatal("peer must be dropped")
	}
	f.st.View(func(d *store.Data) {
		if len(d.Tasks) != 0 || len(d.SiteList()) != 0 {
			t.Fatal("tasks or listing")
		}
	})
	// The freed address is reused.
	b := f.create("Block B")
	tokB, _, _ := f.m.MintJoin(b.ID)
	f.m.ConsumeJoin(tokB, mt, "x")
	if f.site(b.ID).TunnelIP != joined.TunnelIP {
		t.Fatalf("address not reused: %s vs %s", f.site(b.ID).TunnelIP, joined.TunnelIP)
	}
}

func TestWatchJoinMarksJoined(t *testing.T) {
	f := newFixture(t, nil)
	a := f.create("Block A")
	tok, _, _ := f.m.MintJoin(a.ID)
	f.start()
	f.m.ConsumeJoin(tok, mt, "x")
	deadline := time.Now().Add(3 * time.Second)
	for f.site(a.ID).JoinedAt.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("JoinedAt never set after the router answered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---- NetFlow demux ---------------------------------------------------------

func v5(src, dst string, octets uint32) []byte {
	h := make([]byte, 24)
	binary.BigEndian.PutUint16(h[0:], 5)
	binary.BigEndian.PutUint16(h[2:], 1)
	r := make([]byte, 48)
	a, b := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(r[0:], a[:])
	copy(r[4:], b[:])
	binary.BigEndian.PutUint32(r[16:], 10)
	binary.BigEndian.PutUint32(r[20:], octets)
	binary.BigEndian.PutUint16(r[32:], 40000)
	binary.BigEndian.PutUint16(r[34:], 443)
	r[38] = 6
	return append(h, r...)
}

func joinNow(f *fixture, name string) store.Site {
	s := f.create(name)
	tok, _, err := f.m.MintJoin(s.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err := f.m.ConsumeJoin(tok, mt, "x"); err != nil {
		f.t.Fatal(err)
	}
	return f.site(s.ID)
}

func TestNetflowDemux(t *testing.T) {
	f := newFixture(t, nil)
	f.start()
	a, b := joinNow(f, "Block A"), joinNow(f, "Block B")
	pkt := v5("10.5.50.20", "142.250.0.10", 1000) // the same customer subnet in every building
	got := f.m.HandleNetflowPacket(f.now, pkt, a.TunnelIP+":40001")
	if got != a.ID {
		t.Fatalf("A's packet went to %q", got)
	}
	if n := f.m.Collector(a.ID).NetflowStats().Packets; n != 1 {
		t.Fatalf("collector A saw %d", n)
	}
	if n := f.m.Collector(b.ID).NetflowStats().Packets; n != 0 {
		t.Fatalf("collector B saw %d packets that were A's", n)
	}
	if f.m.HandleNetflowPacket(f.now, pkt, b.TunnelIP+":1") != b.ID || f.m.Collector(b.ID).NetflowStats().Packets != 1 || f.m.Collector(a.ID).NetflowStats().Packets != 1 {
		t.Fatal("B's packet must reach only B")
	}
	// Unknown source: dropped and counted; no collector sees it.
	if f.m.HandleNetflowPacket(f.now, pkt, "198.51.100.9:2055") != "" || f.m.Stray() != 1 {
		t.Fatal("unknown source must be dropped")
	}
	if f.m.Collector(a.ID).NetflowStats().Packets != 1 || f.m.Collector(b.ID).NetflowStats().Packets != 1 {
		t.Fatal("a stray packet reached a collector")
	}
	// A paused building's packets are dropped too.
	f.m.SetDisabled(a.ID, true)
	if f.m.HandleNetflowPacket(f.now, pkt, a.TunnelIP+":1") != "" || f.m.Stray() != 2 {
		t.Fatal("paused building")
	}
}

func TestNetflowUnknownSourcesGoToTheEnvSite(t *testing.T) {
	f := newFixture(t, nil)
	envID := store.NewSiteID()
	f.st.Update(func(d *store.Data) error {
		d.Sites[envID] = store.Site{ID: envID, Name: "Main building", Mode: store.SiteModeEnv}
		return nil
	})
	f.start()
	b := joinNow(f, "Block B")
	pkt := v5("10.5.50.20", "142.250.0.10", 1000)
	if f.m.HandleNetflowPacket(f.now, pkt, "198.51.100.9:2055") != envID || f.m.Collector(envID).NetflowStats().Packets != 1 {
		t.Fatal("legacy router exporting from its public address")
	}
	if f.m.HandleNetflowPacket(f.now, pkt, b.TunnelIP+":1") != b.ID {
		t.Fatal("B has its own tunnel address")
	}
	if f.m.Stray() != 0 {
		t.Fatal("nothing was stray")
	}
}

func TestNetflowSocket(t *testing.T) {
	f := newFixture(t, nil)
	envID := store.NewSiteID()
	f.st.Update(func(d *store.Data) error {
		d.Sites[envID] = store.Site{ID: envID, Name: "Main", Mode: store.SiteModeEnv, TunnelIP: ""}
		return nil
	})
	f.start()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- f.m.ListenNetflow(ctx, "127.0.0.1:0", ready) }()
	addr := <-ready
	conn, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Write(v5("10.5.50.20", "151.101.1.1", 1234))
	deadline := time.Now().Add(2 * time.Second)
	for f.m.Collector(envID).NetflowStats().Flows != 1 {
		if time.Now().After(deadline) {
			t.Fatal("flow not received")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := f.m.ListenNetflow(context.Background(), "off", nil); err != nil {
		t.Fatal("off must be a no-op")
	}
}

// ---- bootstrap ------------------------------------------------------------

func TestBootstrap(t *testing.T) {
	run := func(mut func(*config.Config), pre func(dir string, st *store.Store)) (string, *store.Store, string, error) {
		dir := t.TempDir()
		cfg := config.Config{DataDir: dir, RouterPass: "pw"}
		if mut != nil {
			mut(&cfg)
		}
		st, _ := store.Open(dir)
		if pre != nil {
			pre(dir, st)
		}
		id, err := Bootstrap(cfg, st, quiet())
		return id, st, dir, err
	}
	// Fresh VPS: nothing set, zero buildings, no ROUTER_PASS needed.
	id, st, _, err := run(func(c *config.Config) { c.RouterPass = "" }, nil)
	if err != nil || id != "" {
		t.Fatalf("%q %v", id, err)
	}
	st.View(func(d *store.Data) {
		if len(d.Sites) != 0 {
			t.Fatal("fresh VPS must start empty")
		}
	})
	// Container mode: ROUTER_URL set -> seeded env building.
	id, st, _, err = run(func(c *config.Config) { c.RouterURLSet = true; c.SiteName = "Tower 1" }, nil)
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	st.View(func(d *store.Data) {
		s := d.Sites[id]
		if s.Name != "Tower 1" || s.Mode != store.SiteModeEnv || !s.LegacyContainer {
			t.Fatalf("%+v", s)
		}
	})
	// ROUTER_MOCK also seeds, and needs no password.
	id, _, _, err = run(func(c *config.Config) { c.RouterMock = true; c.RouterPass = "" }, nil)
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	// An env building without ROUTER_PASS (and no mock) is a start-up error.
	_, _, _, err = run(func(c *config.Config) { c.RouterURLSet = true; c.RouterPass = "" }, nil)
	if err == nil || !strings.Contains(err.Error(), "ROUTER_PASS") {
		t.Fatalf("%v", err)
	}
	// SITE_NAME renames a migrated "Main building" once, and never a renamed one.
	pre := func(name string) func(string, *store.Store) {
		return func(dir string, st *store.Store) {
			st.Update(func(d *store.Data) error {
				d.Sites["s111111"] = store.Site{ID: "s111111", Name: name, Mode: store.SiteModeEnv}
				return nil
			})
			os.WriteFile(filepath.Join(dir, "metrics.json"), []byte(`{"version":1}`), 0o600)
		}
	}
	id, st, dir, err := run(func(c *config.Config) { c.SiteName = "Tower 1" }, pre("Main building"))
	if err != nil || id != "s111111" {
		t.Fatalf("%q %v", id, err)
	}
	st.View(func(d *store.Data) {
		if d.Sites[id].Name != "Tower 1" {
			t.Fatal("not renamed")
		}
	})
	// metrics.json moved into the per-building file.
	if _, err := os.Stat(MetricsPath(dir, id)); err != nil {
		t.Fatal("metrics.json was not moved")
	}
	if _, err := os.Stat(filepath.Join(dir, "metrics.json")); !os.IsNotExist(err) {
		t.Fatal("old file left behind")
	}
	_, st, dir, _ = run(func(c *config.Config) { c.SiteName = "Tower 1" }, pre("Custom name"))
	st.View(func(d *store.Data) {
		if d.Sites["s111111"].Name != "Custom name" {
			t.Fatal("a renamed building must keep its name")
		}
	})
	// If DATA/metrics/ already exists the old file is left alone.
	_, _, dir, _ = run(nil, func(dir string, st *store.Store) {
		pre("Main building")(dir, st)
		os.MkdirAll(filepath.Join(dir, "metrics"), 0o755)
	})
	if _, err := os.Stat(filepath.Join(dir, "metrics.json")); err != nil {
		t.Fatal("metrics.json must stay when metrics/ exists")
	}
}

func TestEffectiveEnvLayoutComesFromConfig(t *testing.T) {
	f := newFixture(t, func(c *config.Config) {
		c.WANInterface = "ether9"
		c.WANCapacityMbps = 1000
		c.CustomerSubnet = "10.8.0.0/24"
	})
	s := f.m.Effective(store.Site{ID: "s1", Mode: store.SiteModeEnv, WANInterface: "stale"})
	if s.WANInterface != "ether9" || s.WANCapMbps != 1000 || s.CustomerSubnet != "10.8.0.0/24" || s.HotspotServer != "hotspot1" {
		t.Fatalf("%+v", s)
	}
	w := f.m.Effective(store.Site{ID: "s2", Mode: store.SiteModeWG, WANInterface: "ether3"})
	if w.WANInterface != "ether3" || w.RESTPort != 8081 || w.RESTUser != "ftn-api" || w.WANList != "WAN" {
		t.Fatalf("%+v", w)
	}
}

var _ = billing.ErrNoSite
