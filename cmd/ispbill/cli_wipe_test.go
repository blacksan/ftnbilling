package main

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/metrics"
	"ispbill/internal/sites"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
	"ispbill/internal/wg"
)

// freeAddr is a listen address nothing answers on, so the "service is running"
// check of the command cannot be fooled by something on this machine.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// wipeScenario runs the whole story on one backend. open opens the store the
// command line works on (the service is "stopped": nothing else holds it).
func wipeScenario(t *testing.T, dir string, env func(string) string, open func() *store.Store) {
	t.Helper()
	st := open()
	storetest.Fixture(t, st)
	at := st.Snapshot().Sites["s111111"].CreatedAt
	if err := st.Update(func(d *store.Data) error {
		d.Settings["site.s111111.join_report"] = store.Setting{Key: "site.s111111.join_report", Value: "ok", UpdatedAt: at}
		d.Settings["admin.kb.abc"] = store.Setting{Key: "admin.kb.abc", Value: "u1", UpdatedAt: at}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := st.Snapshot()
	st.Close()

	writeFile(t, filepath.Join(dir, "metrics", "s111111.json"), "{}")
	writeFile(t, filepath.Join(dir, "spool", "1-000001-c2b.json"), `{"TransID":"X"}`)
	writeFile(t, filepath.Join(dir, "fleet.json"), `{"series":{}}`)
	writeFile(t, filepath.Join(dir, "outbox.json"), `{"messages":[{"id":"m1"}],"keys":{"k":"2026-01-01T00:00:00Z"},"paused":{"whatsapp":{"until":"2099-01-01T00:00:00Z"}}}`)
	writeFile(t, filepath.Join(dir, "alerts.json"), `{"settings":{"email":true},"active":{"offline:s1":{}},"history":[{"key":"x"}]}`)
	writeFile(t, filepath.Join(dir, "backup-status.json"), `{"dump_at":"2026-01-01T00:00:00Z"}`)
	if err := wg.WritePeers(filepath.Join(dir, "wg"), []wg.Peer{{PublicKey: "aGVsbG8gd29ybGQgaGVsbG8gd29ybGQgaGVsbG8gd29ybGQ=", TunnelIP: "10.99.0.2"}}); err != nil {
		t.Fatal(err)
	}

	unchanged := func(what string) {
		t.Helper()
		s := open()
		defer s.Close()
		if !reflect.DeepEqual(s.Snapshot(), before) {
			t.Fatalf("%s: the data changed", what)
		}
		for _, p := range []string{"metrics/s111111.json", "spool/1-000001-c2b.json", "fleet.json"} {
			if !exists(filepath.Join(dir, p)) {
				t.Fatalf("%s: %s is gone", what, p)
			}
		}
	}

	// Without -confirm WIPE: the counts, and nothing is changed.
	out, err := cli(t, env, "wipe-data")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"would delete", "routers", "2", "payments", "kept: 2 admin user(s)", "metrics histories (files)"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	unchanged("dry run")
	// A wrong word is refused, also without a change.
	if _, err := cli(t, env, "wipe-data", "-confirm", "wipe"); err == nil || !strings.Contains(err.Error(), "WIPE") {
		t.Fatalf("a wrong -confirm must fail: %v", err)
	}
	unchanged("wrong confirm")

	// The real thing.
	out, err = cli(t, env, "wipe-data", "-confirm", "WIPE")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "wiped the business data") || strings.Contains(out, "would delete") {
		t.Errorf("output:\n%s", out)
	}

	st = open()
	defer st.Close()
	after := st.Snapshot()
	for name, n := range map[string]int{"sites": len(after.Sites), "packages": len(after.Packages), "accounts": len(after.Accounts), "closed": len(after.ClosedAccounts),
		"orders": len(after.Orders), "vouchers": len(after.Vouchers), "payments": len(after.Payments), "devices": len(after.Devices), "tasks": len(after.Tasks),
		"events": len(after.Events), "customers": len(after.Customers), "tenancies": len(after.Tenancies), "contacts": len(after.Contacts),
		"equipment": len(after.Equipment), "equipment events": len(after.EquipmentEvents), "notes": len(after.Notes),
		"sessions": len(after.AdminSessions), "phones": len(after.APIDevices)} {
		if n != 0 {
			t.Errorf("%s: %d left", name, n)
		}
	}
	if !reflect.DeepEqual(after.AdminUsers, before.AdminUsers) {
		t.Errorf("admin users changed: %+v", after.AdminUsers)
	}
	for _, k := range []string{"wa.token", "wa.lang", "admin.kb.abc"} {
		if !reflect.DeepEqual(after.Settings[k], before.Settings[k]) {
			t.Errorf("setting %s: %+v", k, after.Settings[k])
		}
	}
	if _, ok := after.Settings["site.s111111.join_report"]; ok || len(after.Settings) != 3 {
		t.Errorf("settings left: %v", after.Settings)
	}
	for _, p := range []string{"metrics/s111111.json", "spool/1-000001-c2b.json", "fleet.json"} {
		if exists(filepath.Join(dir, p)) {
			t.Errorf("%s was not removed", p)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "outbox.json")); !strings.Contains(string(raw), "paused") || strings.Contains(string(raw), "m1") {
		t.Errorf("outbox.json: %s", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "alerts.json")); !strings.Contains(string(raw), "settings") || strings.Contains(string(raw), "offline") || strings.Contains(string(raw), "history") {
		t.Errorf("alerts.json: %s", raw)
	}
	if !exists(filepath.Join(dir, "backup-status.json")) {
		t.Error("backup-status.json must stay")
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "wg", "peers.conf")); strings.Contains(string(raw), "[Peer]") {
		t.Errorf("peers.conf still lists a router:\n%s", raw)
	}

	// The app works on the wiped store: a router and a flat can be created.
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	_, serverPub, _ := wg.GenerateKeypair()
	cfg.WGPublicKey, cfg.WGEndpoint, cfg.WGSubnet, cfg.PublicIP = serverPub, "203.0.113.10:51820", "10.99.0.0/22", "203.0.113.10"
	cfg.CustomerSubnet, cfg.WANInterface, cfg.OLTInterface = "10.5.50.0/24", "ether1", "sfp-sfpplus1"
	cfg.NetflowAddr = "off"
	var svc *billing.Service
	mgr := sites.New(sites.Options{Cfg: cfg, Store: st, Log: wipeLog(), DataKey: cfg.DataKey, Dir: func(id string) metrics.Directory { return svc.Directory(id) }})
	svc = billing.NewService(st, mgr, wipeLog())
	mgr.SetProvisioner(svc)
	if err := svc.SeedPackages(); err != nil {
		t.Fatal(err)
	}
	site, err := mgr.CreateSite(sites.SiteInput{Name: "Block A again"})
	if err != nil {
		t.Fatalf("create a router after the wipe: %v", err)
	}
	acct, err := svc.CreateAccount(billing.AccountInput{SiteID: site.ID, Flat: "Flat 1", MAC: "AA:BB:CC:DD:EE:01"})
	if err != nil {
		t.Fatalf("create a flat after the wipe: %v", err)
	}
	if err := st.Healthy(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "wg", "peers.conf")); raw == nil {
		t.Error("peers.conf missing after creating a router")
	}
	st.Close()
	st = open() // a restart sees them
	defer st.Close()
	now := st.Snapshot()
	if _, ok := now.Sites[site.ID]; !ok || now.Accounts[acct.ID].Flat != "Flat 1" || len(now.Packages) == 0 || len(now.AdminUsers) != 2 {
		t.Errorf("after a restart: %d sites, account %+v", len(now.Sites), now.Accounts[acct.ID])
	}
}

func TestWipeDataJSON(t *testing.T) {
	dir := t.TempDir()
	base := cliEnv(dir)
	env := func(k string) string {
		if k == "LISTEN_ADDR" {
			return freeAddr(t)
		}
		return base(k)
	}
	wipeScenario(t, dir, env, func() *store.Store {
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		return st
	})
	// The file was replaced atomically: no temp file is left behind.
	if tmp, _ := filepath.Glob(filepath.Join(dir, ".ispbill-*.tmp")); len(tmp) > 0 {
		t.Errorf("temp files left: %v", tmp)
	}
}

// With the service answering, the real wipe is refused on the JSON file.
func TestWipeDataRefusedWhileServiceRuns(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveOK(ln)
	base := cliEnv(dir)
	env := func(k string) string {
		if k == "LISTEN_ADDR" {
			return ln.Addr().String()
		}
		return base(k)
	}
	st, _ := store.Open(dir)
	storetest.Fixture(t, st)
	st.Close()
	if _, err := cli(t, env, "wipe-data", "-confirm", "WIPE"); err == nil || !strings.Contains(err.Error(), "ftnbilling-wipe") {
		t.Fatalf("want a refusal that names ftnbilling-wipe, got %v", err)
	}
	// The dry run is read-only and works while the service runs.
	if out, err := cli(t, env, "wipe-data"); err != nil || !strings.Contains(out, "would delete") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	st, _ = store.Open(dir)
	defer st.Close()
	if len(st.Snapshot().Sites) != 2 {
		t.Fatal("the refused wipe changed the data")
	}
}

func serveOK(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			buf := make([]byte, 1024)
			c.Read(buf)
			c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		}()
	}
}

func wipeLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The old full copies of the data next to the database are counted in the dry run and are not touched by it.
func TestWipeDryRunCountsOldDatabaseCopies(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ispbill.json.v6.bak"), "{}")
	writeFile(t, filepath.Join(dir, "ispbill.json.imported-20260101"), "{}")
	writeFile(t, filepath.Join(dir, "unrelated.txt"), "x")
	if got := dataFiles(dir).old; len(got) != 2 {
		t.Fatalf("old copies: %v", got)
	}
	var out strings.Builder
	printWipe(&out, "the JSON file", store.WipeReport{}, dataFiles(dir), false)
	if !strings.Contains(out.String(), "old database copies (.bak, imported)") || !strings.Contains(out.String(), " 2\n") {
		t.Fatalf("not listed:\n%s", out.String())
	}
	if !exists(filepath.Join(dir, "ispbill.json.v6.bak")) {
		t.Fatal("the dry run removed a file")
	}
}
