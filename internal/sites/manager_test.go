package sites

import (
	"context"
	"encoding/binary"
	"fmt"
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
	"ispbill/internal/store/storetest"
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
}

func newFixture(t *testing.T, mut func(*config.Config)) *fixture {
	t.Helper()
	_, serverPub, _ := wg.GenerateKeypair()
	cfg := config.Config{DataDir: t.TempDir(), PublicBaseURL: "https://billing.example.co.ke", PublicIP: "203.0.113.10",
		WGPublicKey: serverPub, WGEndpoint: "203.0.113.10:51820", WGSubnet: "10.99.0.0/22", WGDumpPath: filepath.Join(t.TempDir(), "wg0.dump"),
		JoinTokenTTL: 30 * time.Minute, JoinRequireMikrotikUA: true, NetflowAddr: "off", BrandName: "FTN",
		WANInterface: "ether1", OLTInterface: "sfp-sfpplus1", WANCapacityMbps: 500, OLTCapacityMbps: 10000, FUPTB: 15, CustomerSubnet: "10.5.50.0/24",
		PingTargets: []string{"8.8.8.8"}, FlowEvery: time.Hour, PortEvery: time.Hour, PingEvery: time.Hour, DNSEvery: time.Hour, SaveEvery: time.Hour}
	if mut != nil {
		mut(&cfg)
	}
	st, err := storetest.Open(t, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, cfg: cfg, st: st, now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), mocks: map[string]*routeros.Mock{}}
	f.m = New(Options{Cfg: cfg, DataKey: cfg.DataKey, Store: st, Log: quiet(), Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
		Dir: func(string) metrics.Directory { return nil },
		NewClient: func(s store.Site) routeros.Client {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.mocks[s.ID] == nil {
				f.mocks[s.ID] = routeros.NewMock()
			}
			return f.mocks[s.ID]
		}})
	return f
}

func (f *fixture) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

func (f *fixture) site(id string) (s store.Site) {
	f.st.View(func(d *store.Data) { s = d.Sites[id] })
	return
}

func (f *fixture) create(name string) store.Site {
	f.t.Helper()
	s, err := f.m.CreateSite(SiteInput{Name: name})
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
	if !storetest.Postgres() {
		raw, _ := os.ReadFile(filepath.Join(f.cfg.DataDir, "ispbill.json"))
		if strings.Contains(string(raw), tok) || !strings.Contains(string(raw), HashToken(tok)) {
			t.Fatal("the token must be stored as a hash only")
		}
	}
	f.st.View(func(d *store.Data) {
		if d.Sites[s.ID].Join.Hash != HashToken(tok) || strings.Contains(fmt.Sprintf("%+v", d.Sites[s.ID]), tok) {
			t.Fatal("the token must be stored as a hash only")
		}
	})
	// A link-preview bot (wrong User-Agent) does not consume it.
	if _, _, err := f.m.ConsumeJoin(tok, "WhatsApp/2.23", "1.2.3.4", false); err != ErrJoinInvalid {
		t.Fatalf("bot: %v", err)
	}
	if got := f.site(s.ID); !got.Join.UsedAt.IsZero() || got.WGPublicKey != "" {
		t.Fatal("a bot used the token up")
	}
	if _, _, err := f.m.ConsumeJoin("not-the-token", mt, "1.2.3.4", false); err != ErrJoinInvalid {
		t.Fatalf("wrong token: %v", err)
	}
	script, id, err := f.m.ConsumeJoin(tok, "MikroTik/7.20 (fetch)", "198.51.100.7", false)
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
	f.st.View(func(d *store.Data) {
		if strings.Contains(fmt.Sprintf("%+v", d.Sites[got.ID]), m[1]) {
			t.Fatal("the router's private key was stored")
		}
	})
	if !strings.Contains(string(script), `password="`+got.RESTPass+`"`) || !strings.Contains(string(script), "10.99.0.2") {
		t.Fatal("script lacks the REST password or tunnel address")
	}
	// Single use.
	if _, _, err := f.m.ConsumeJoin(tok, mt, "1.2.3.4", false); err != ErrJoinInvalid {
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
	// A second building takes the next address.
	b := f.create("Block B")
	tokB, _, _ := f.m.MintJoin(b.ID)
	if _, _, err := f.m.ConsumeJoin(tokB, mt, "x", false); err != nil {
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
	if _, _, err := f.m.ConsumeJoin(tok, mt, "x", false); err != ErrJoinInvalid {
		t.Fatal("the old token must be dead after a new mint")
	}
	if _, _, err := f.m.ConsumeJoin(tok2, mt, "x", false); err != nil {
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
	if _, _, err := f.m.ConsumeJoin(tok, mt, "x", false); err != ErrJoinInvalid {
		t.Fatalf("expired: %v", err)
	}
	if f.site(s.ID).WGPublicKey != "" {
		t.Fatal("expired token changed the site")
	}
	tok, _, _ = f.m.MintJoin(s.ID)
	if err := f.m.RevokeJoin(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.ConsumeJoin(tok, mt, "x", false); err != ErrJoinInvalid {
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
	if _, _, err := f.m.ConsumeJoin(tok, "curl/8", "x", false); err != nil {
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
			if _, _, err := f.m.ConsumeJoin(tok, mt, "x", false); err == nil {
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
	a := f.create("Block A")
	tok, _, _ := f.m.MintJoin(a.ID)
	f.start()
	if f.m.Collector(a.ID) != nil || f.m.Router(a.ID).Ping(context.Background()) != routeros.ErrNoRouter {
		t.Fatal("an unjoined building must have no runtime")
	}
	if f.m.Router("nope").Ping(context.Background()) != routeros.ErrNoRouter {
		t.Fatal("unknown site")
	}
	// Joining starts the runtime (join consumed -> ApplyPeers + Reload).
	if _, _, err := f.m.ConsumeJoin(tok, mt, "x", false); err != nil {
		t.Fatal(err)
	}
	if f.m.Collector(a.ID) == nil || f.m.Router(a.ID).Ping(context.Background()) != nil {
		t.Fatal("joined building must run")
	}
	ctx := context.Background()
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
	if err := f.m.SetDisabled(a.ID, false); err != nil {
		t.Fatal(err)
	}
	if f.m.Collector(a.ID) == nil {
		t.Fatal("resume")
	}
	// Edit restarts it.
	c1 := f.m.Collector(a.ID)
	// the WAN port of a router that has joined is locked, unless a new script is being made for it
	if siteIsSetUp(f.site(a.ID)) {
		if err := f.m.UpdateSite(a.ID, SiteInput{Name: "Block A2", WANInterface: "ether3"}); err == nil || !strings.Contains(err.Error(), "cannot be changed here") {
			t.Fatalf("a joined router's WAN port was changed: %v", err)
		}
		if _, _, err := f.m.MintJoin(a.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.m.UpdateSite(a.ID, SiteInput{Name: "Block A2", WANInterface: "ether3"}); err != nil {
		t.Fatal(err)
	}
	if f.m.Collector(a.ID) == c1 || f.m.Collector(a.ID).Config().WAN != "ether3" || f.site(a.ID).Name != "Block A2" {
		t.Fatal("update must rebuild the collector")
	}
	if cfg := f.m.Collector(a.ID).Config(); len(cfg.PortAllow) != 3 || cfg.SiteID != a.ID {
		t.Fatalf("wg collector config: %+v", cfg)
	}
}

func TestRemoveSite(t *testing.T) {
	f := newFixture(t, nil)
	a := f.create("Block A")
	tok, _, _ := f.m.MintJoin(a.ID)
	f.start()
	f.m.ConsumeJoin(tok, mt, "x", false)
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
	f.m.ConsumeJoin(tokB, mt, "x", false)
	if f.site(b.ID).TunnelIP != joined.TunnelIP {
		t.Fatalf("address not reused: %s vs %s", f.site(b.ID).TunnelIP, joined.TunnelIP)
	}
}

func TestWatchJoinMarksJoined(t *testing.T) {
	f := newFixture(t, nil)
	a := f.create("Block A")
	tok, _, _ := f.m.MintJoin(a.ID)
	f.start()
	f.m.ConsumeJoin(tok, mt, "x", false)
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
	if _, _, err := f.m.ConsumeJoin(tok, mt, "x", false); err != nil {
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

func TestNetflowSocket(t *testing.T) {
	f := newFixture(t, nil)
	f.start()
	// The loopback sender is the building's tunnel address.
	envID := joinNow(f, "Main").ID
	f.st.Update(func(d *store.Data) error {
		s := d.Sites[envID]
		s.TunnelIP = "127.0.0.1"
		d.Sites[envID] = s
		return nil
	})
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

var _ = billing.ErrNoSite
