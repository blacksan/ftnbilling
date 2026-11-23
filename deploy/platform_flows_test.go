package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lib returns a copy of a script without its last lines (the call to main), to source.
func lib(t *testing.T, name string, cut func(lines []string) []string) string {
	t.Helper()
	bashOrSkip(t)
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "lib.sh")
	if err := os.WriteFile(p, []byte(strings.Join(cut(strings.Split(string(raw), "\n")), "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func installLib(t *testing.T) string {
	return lib(t, "install.sh", func(l []string) []string {
		for i, x := range l {
			if x == `main "$@"` {
				return l[:i]
			}
		}
		t.Fatal("main call not found")
		return nil
	})
}

func fakeBin(t *testing.T, files map[string]string) string {
	t.Helper()
	d := t.TempDir()
	for n, body := range files {
		if err := os.WriteFile(filepath.Join(d, n), []byte("#!/bin/bash\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func bash(t *testing.T, env []string, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// P5-1: install.sh waits for the dpkg lock (DPkg::Lock::Timeout) but "apt-get update"
// takes /var/lib/apt/lists/lock, which that setting does not cover. A new VPS runs
// apt-daily / unattended-upgrades in the first minutes: step 4 fails at once with
// "Could not get lock /var/lib/apt/lists/lock". This test uses the real apt-get
// with its state directories moved into a temp dir (nothing system-wide is touched).
func TestP5AptUpdateWaitsForListsLock(t *testing.T) {
	if _, err := exec.LookPath("apt-get"); err != nil {
		t.Skip("no apt-get")
	}
	root := t.TempDir()
	lists := filepath.Join(root, "lists")
	os.MkdirAll(filepath.Join(lists, "partial"), 0o755)
	os.MkdirAll(filepath.Join(root, "cache", "archives", "partial"), 0o755)
	lf, err := os.OpenFile(filepath.Join(lists, "lock"), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.FcntlFlock(lf.Fd(), syscall.F_SETLK, &syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0}); err != nil {
		t.Fatal(err)
	}
	go func() { // the other apt (unattended-upgrades) lets go after 4 s
		time.Sleep(4 * time.Second)
		syscall.FcntlFlock(lf.Fd(), syscall.F_SETLK, &syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 0})
	}()
	script := fmt.Sprintf(`set +e; source %q 2>/dev/null
apt_get -o Dir::State::Lists=%s -o Dir::Cache=%s -o Dir::Etc::sourcelist=/dev/null -o Dir::Etc::sourceparts=/dev/null update -qq
echo "rc=$?"`, installLib(t), lists, filepath.Join(root, "cache"))
	start := time.Now()
	out, _ := bash(t, nil, script)
	t.Logf("after %v: %s", time.Since(start).Round(time.Second), strings.TrimSpace(out))
	if !strings.Contains(out, "rc=0") {
		t.Fatalf("apt-get update did not wait for the lists lock: %s", out)
	}
}

// P5-2: VERSION=<tag> is a pin. When that release cannot be fetched (typo, 404, a
// missing arch bundle, a rate-limited API) the installer silently builds the
// BRANCH head instead and installs it under "src-<sha>".
func TestP5PinnedVersionNeverFallsBackToBranchHead(t *testing.T) {
	bin := fakeBin(t, map[string]string{
		// answers the GitHub API call with HTTP 404 and writes nothing else
		"curl": `out=""; while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift;; esac; shift; done; [ -n "$out" ] && echo '{"message":"Not Found"}' >"$out"; printf 404`,
	})
	script := fmt.Sprintf(`set +e; source %q 2>/dev/null
trap - ERR; set +E
get_source_and_build() { echo "FALLBACK: building branch $BRANCH from source"; exit 0; }
GOARCH=amd64; WANT_VERSION=v9.9.9; DRY=0; SELF_DIR=""
acquire_program`, installLib(t))
	out, _ := bash(t, []string{"PATH=" + bin + ":" + os.Getenv("PATH")}, script)
	t.Logf("%s", out)
	if strings.Contains(out, "FALLBACK") {
		t.Fatalf("VERSION=v9.9.9 was pinned, but the installer would build %q instead:\n%s", "main", out)
	}
}

// P5-3: "restore latest" picks the last name in sort order, and the name starts
// with the host name. After a rebuild on a new VPS (new host name) it can restore
// an older snapshot of the previous host.
func TestP5RestoreLatestIsNewestByDateNotByName(t *testing.T) {
	bin := fakeBin(t, map[string]string{
		"rclone": `case "$1" in
 version) echo "rclone v1.66.0";;
 listremotes) echo "ftn-backup:";;
 lsf) printf '%s\n' "1000 ftnbilling-vps1-2026-03-01.tar.gz" "2000 ftnbilling-billing-2026-11-22.tar.gz";;
 copy) echo "COPY $*" >>"$FTN_LOG"; exit 1;;
esac`,
	})
	root := t.TempDir()
	log := filepath.Join(root, "log")
	lb := lib(t, "ftnbilling-backup", func(l []string) []string {
		for i, x := range l {
			if strings.HasSuffix(x, "-- main") {
				return l[:i]
			}
		}
		t.Fatal("main marker not found")
		return nil
	})
	script := fmt.Sprintf(`set +e; source %q "restore" 2>/dev/null; trap - ERR; set +E
cmd_restore latest 2>&1 | tail -2`, lb)
	out, _ := bash(t, []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "FTN_ROOT=" + root, "FTN_LOG=" + log, "RCLONE_CONFIG=" + filepath.Join(root, "rc.conf")}, script)
	got, _ := os.ReadFile(log)
	t.Logf("restore asked rclone for: %s%s", got, out)
	if strings.Contains(string(got), "vps1-2026-03-01") {
		t.Fatalf("restore latest chose the March snapshot of host vps1 over the 22 Nov snapshot of host billing")
	}
}

// P5-4: the status file is read back with a sed that stops at the first comma, so a
// message with a comma ("error": the pg_dump error text) drops out of the file at the
// next rewrite (another status_set), and the failure is no longer reported.
func TestP5BackupStatusKeepsMessagesWithCommas(t *testing.T) {
	root := t.TempDir()
	lb := lib(t, "ftnbilling-backup", func(l []string) []string {
		for i, x := range l {
			if strings.HasSuffix(x, "-- main") {
				return l[:i]
			}
		}
		return l
	})
	script := fmt.Sprintf(`set +e; source %q 2>/dev/null; trap - ERR; set +E
status_set 'error=pg_dump: error: connection to server failed, no password supplied' drill_ok=false
status_set dump_at=2026-11-20T00:17:00Z
cat "$STATUS_JSON"`, lb)
	out, _ := bash(t, []string{"FTN_ROOT=" + root}, script)
	t.Logf("%s", out)
	if !strings.Contains(out, "no password supplied") {
		t.Fatalf("the failure message vanished from backup-status.json after the next write:\n%s", out)
	}
}

// P5-5: on an upgrade the owner is already enrolled; the installer still prints the
// "FIRST SIGN-IN" block, with the CLI's "already has an authenticator" note as the code.
func TestP5UpgradeBannerShowsFirstSignInForEnrolledOwner(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "ftnbilling-admin-reset"), []byte("#!/bin/bash\necho '\"admin\" already has an authenticator app, so no setup code was issued (use -reset-totp if the phone is lost)'\necho; echo 'Start the service again: sudo systemctl start ftnbilling (deploy/ftnbilling-admin-reset does this for you)'\n"), 0o755)
	script := fmt.Sprintf(`set +e; source %q 2>/dev/null; trap - ERR; set +E
SBIN=%s; make_setup_code; printf 'SETUP_OUT=[%%s]\n' "$SETUP_OUT"; [ -n "$SETUP_OUT" ] && echo "banner prints FIRST SIGN-IN with this text"`, installLib(t), bin)
	out, _ := bash(t, nil, script)
	t.Logf("%s", out)
	if strings.Contains(out, "banner prints FIRST SIGN-IN") {
		t.Fatalf("an enrolled owner gets the first-sign-in instructions on every upgrade")
	}
}

// P5-6: on a JSON-store server (NO_POSTGRES=1) the weekly Drive run is the first writer of
// backup-status.json and writes upload_* but never dump_at; the app's BackupProblem then
// reports "no database dump has been made yet" for good (see internal/alerts/p5_backup_test.go).
func TestP5JSONServerStatusFileHasNoDumpAt(t *testing.T) {
	root := t.TempDir()
	lb := lib(t, "ftnbilling-backup", func(l []string) []string {
		for i, x := range l {
			if strings.HasSuffix(x, "-- main") {
				return l[:i]
			}
		}
		return l
	})
	script := fmt.Sprintf(`set +e; source %q 2>/dev/null; trap - ERR; set +E
HOST=h; write_state ok snap.tar.gz 10 abc ""; cat "$STATUS_JSON"`, lb)
	out, _ := bash(t, []string{"FTN_ROOT=" + root}, script)
	t.Logf("%s", out)
	if !strings.Contains(out, "dump_at") {
		t.Fatalf("status file has no dump_at although a backup just succeeded:\n%s", out)
	}
}

// P5-7: a first install that stops after the settings step (healthz timeout, apt failure)
// has already written ADMIN_PASS to the env file but never printed it. The re-run sees the
// env file, calls itself an upgrade and prints "your existing admin password".
func TestP5RerunAfterFailedFirstInstallNeverShowsGeneratedPassword(t *testing.T) {
	d := t.TempDir()
	envf := filepath.Join(d, "ftnbilling.env")
	os.WriteFile(envf, []byte("ADMIN_USER=admin\nADMIN_PASS=Generated18CharsXyz\nPUBLIC_BASE_URL=https://billing.example.co.ke\n"), 0o640)
	sbin := fakeBin(t, map[string]string{"ftnbilling-doctor": "exit 0"})
	script := fmt.Sprintf(`set +e; source %q 2>/dev/null; trap - ERR; set +E
ENVF=%s; SBIN=%s; DRY=0
PROG_VERSION=v1; BASE=https://billing.example.co.ke; WG_PUB=k; MGMT_ON=0; MAC_REPORT=""; SETUP_OUT="code-1234"; REMOVED_NOTE=""; BK_MSG=""; DOC_RC=0; DB_COUNTS=""; HEALTH=""
UPGRADE=1   # what preflight sets because %s exists
doctor_and_banner 2>&1 | grep -A3 'FIRST SIGN-IN'`, installLib(t), envf, sbin, envf)
	out, _ := bash(t, nil, script)
	t.Logf("%s", out)
	if strings.Contains(out, "your existing admin password") {
		t.Fatalf("the generated password was never shown to anybody, yet the banner calls it existing")
	}
}

// P5-8: "run the same line again" is the documented upgrade. DOMAIN is only read from the
// environment of that run; the Caddyfile is rewritten from it. A re-run without DOMAIN on a
// server that has PUBLIC_BASE_URL=https://... turns the public site into plain http on :80.
func TestP5RerunWithoutDomainDowngradesCaddyfileToPlainHTTP(t *testing.T) {
	d := t.TempDir()
	envf := filepath.Join(d, "ftnbilling.env")
	os.WriteFile(envf, []byte("PUBLIC_BASE_URL=https://billing.example.co.ke\nDATABASE_URL=postgres://x\n"), 0o640)
	bin := fakeBin(t, map[string]string{"caddy": "exit 0", "systemctl": "exit 0"})
	cap := filepath.Join(d, "cap")
	os.MkdirAll(cap, 0o755)
	script := fmt.Sprintf(`set +e; source %q 2>/dev/null; trap - ERR; set +E
ENVF=%s; DRY=0; unset DOMAIN
writef() { cat >"%s/$(printf '%%s' "$3" | tr / _)"; }
app_and_caddy >/dev/null 2>&1
grep -v '^#' "%s/_etc_caddy_Caddyfile" | grep -v '^$' | head -8`, installLib(t), envf, cap, cap)
	out, _ := bash(t, []string{"PATH=" + bin + ":" + os.Getenv("PATH")}, script)
	t.Logf("Caddyfile after a re-run without DOMAIN:\n%s", out)
	if strings.Contains(out, ":80 {") {
		t.Fatalf("the HTTPS site was replaced by a plain-HTTP :80 site although PUBLIC_BASE_URL is https")
	}
}

// P5-9: WG_SUBNET and WG_PORT also come only from the environment of the run. The settings
// file keeps the first install's value (addkv), but wg-sync.env, the ufw rules and the
// remote-WinBox forwarding block are rebuilt from the default, so wg-sync then refuses
// every router peer as "outside 10.99.0.0/22".
func TestP5RerunKeepsCustomWGSubnet(t *testing.T) {
	d := t.TempDir()
	raw, _ := os.ReadFile("install.sh")
	src := strings.Replace(string(raw), "ETC=/etc/$APP", "ETC="+d, 1)
	i := strings.LastIndex(src, "\nmain \"$@\"")
	p := filepath.Join(d, "lib.sh")
	os.WriteFile(p, []byte(src[:i]), 0o644)
	os.WriteFile(filepath.Join(d, "ftnbilling.env"), []byte("WG_SUBNET=10.77.0.0/22\nWG_PORT=51999\n"), 0o640)
	out, _ := bash(t, []string{"WG_SUBNET=", "WG_PORT="}, fmt.Sprintf(`unset WG_SUBNET WG_PORT; source %q 2>/dev/null; echo "WG_SUBNET=$WG_SUBNET WG_PORT=$WG_PORT (env file has 10.77.0.0/22 and 51999)"`, p))
	t.Log(out)
	if !strings.Contains(out, "WG_SUBNET=10.77.0.0/22") {
		t.Fatalf("a re-run ignores the subnet the server was installed with")
	}
}

// P5-10: one bad peer in peers.conf makes ftnbilling-wg-sync refuse the whole file: no peer is
// applied, the only trace is the journal of a oneshot unit (nothing in the admin, nothing in the doctor),
// and as wg-quick@wg0's ExecStartPost it fails the tunnel unit itself at the next boot.
func TestP5WgSyncOneBadPeerBlocksEveryRouter(t *testing.T) {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "wg"), 0o755)
	good1 := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	good2 := "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="
	bad := "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC="
	peers := fmt.Sprintf("[Peer]\nPublicKey = %s\nAllowedIPs = 10.99.0.2/32\n\n[Peer]\nPublicKey = %s\nAllowedIPs = 10.99.0.3/32\n\n[Peer]\nPublicKey = %s\nAllowedIPs = 10.50.0.9/32\n\n", good1, good2, bad)
	os.WriteFile(filepath.Join(d, "wg", "peers.conf"), []byte(peers), 0o640)
	conf := filepath.Join(d, "wg-sync.env")
	os.WriteFile(conf, []byte(fmt.Sprintf("DATA=%s\nWG_SUBNET=10.99.0.0/22\nWG_IFACE=wg0\n", d)), 0o644)
	log := filepath.Join(d, "wg.log")
	bin := fakeBin(t, map[string]string{"wg": `echo "wg $*" >>"$FTN_LOG"; cat >/dev/null`, "wg-quick": `echo "[Interface]"`})
	out, err := bash(t, []string{"FTN_WG_SYNC_CONF=" + conf, "WG_BIN=" + filepath.Join(bin, "wg"), "WG_QUICK_BIN=" + filepath.Join(bin, "wg-quick"), "FTN_LOG=" + log}, "bash ./ftnbilling-wg-sync")
	called, _ := os.ReadFile(log)
	t.Logf("exit error=%v output=%q wg calls=%q", err, strings.TrimSpace(out), called)
	if err != nil && len(called) == 0 {
		t.Fatalf("2 valid routers were not applied because a third entry is bad: %s", strings.TrimSpace(out))
	}
}

// A server installed before the banner marker file existed, with an owner who has already set up the
// authenticator, must not have ADMIN_PASS printed again on its first upgrade.
func TestRerunNeverPrintsAdminPassForEnrolledOwner(t *testing.T) {
	d := t.TempDir()
	envf := filepath.Join(d, "ftnbilling.env")
	os.WriteFile(envf, []byte("ADMIN_USER=admin\nADMIN_PASS=Generated18CharsXyz\nPUBLIC_BASE_URL=https://billing.example.co.ke\n"), 0o640)
	sbin := fakeBin(t, map[string]string{"ftnbilling-doctor": "exit 0"})
	os.WriteFile(filepath.Join(sbin, "ftnbilling-admin-reset"), []byte("#!/bin/bash\necho '\"admin\" already has an authenticator app, so no setup code was issued'\n"), 0o755)
	script := fmt.Sprintf(`set +e; source %q 2>/dev/null; trap - ERR; set +E
ENVF=%s; SBIN=%s; DRY=0
PROG_VERSION=v1; BASE=https://billing.example.co.ke; WG_PUB=k; MGMT_ON=0; MAC_REPORT=""; REMOVED_NOTE=""; BK_MSG=""; DOC_RC=0; DB_COUNTS=""; HEALTH=""
UPGRADE=1
OWNER_ENROLLED=1; SETUP_OUT="code-1234"   # an enrolled owner whose program still issued a code
doctor_and_banner 2>&1`, installLib(t), envf, sbin)
	out, _ := bash(t, nil, script)
	if strings.Contains(out, "Generated18CharsXyz") {
		t.Fatalf("the admin password was printed again for an enrolled owner:\n%s", out)
	}
}

// ONU management: a peer may carry its router's management /24 (inside ONU_SUPERNET) next to its /32, a subnet
// belongs to one peer, anything else is skipped, and the supernet is routed into the tunnel interface.
func TestWgSyncONUMgmtSubnet(t *testing.T) {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "wg"), 0o755)
	key := func(c string) string { return strings.Repeat(c, 43) + "=" }
	peers := fmt.Sprintf("[Peer]\nPublicKey = %s\nAllowedIPs = 10.99.0.2/32, 10.6.1.0/24\n\n"+
		"[Peer]\nPublicKey = %s\nAllowedIPs = 10.99.0.3/32, 10.6.1.0/24\n\n"+ // the same subnet again
		"[Peer]\nPublicKey = %s\nAllowedIPs = 10.99.0.4/32, 10.8.1.0/24\n\n"+ // outside the supernet
		"[Peer]\nPublicKey = %s\nAllowedIPs = 10.99.0.5/32, 10.6.2.0/25\n\n"+ // not a /24
		"[Peer]\nPublicKey = %s\nAllowedIPs = 10.99.0.6/32, 10.6.3.0/24, 0.0.0.0/0\n\n"+ // a third entry
		"[Peer]\nPublicKey = %s\nAllowedIPs = 10.6.4.0/24\n\n"+ // the management subnet alone is no peer address
		"[Peer]\nPublicKey = %s\nAllowedIPs = 10.99.0.7/32, 10.6.7.0/24\n\n",
		key("A"), key("B"), key("C"), key("D"), key("E"), key("F"), key("G"))
	os.WriteFile(filepath.Join(d, "wg", "peers.conf"), []byte(peers), 0o640)
	conf := filepath.Join(d, "wg-sync.env")
	os.WriteFile(conf, []byte(fmt.Sprintf("DATA=%s\nWG_SUBNET=10.99.0.0/22\nWG_IFACE=wg0\n", d)), 0o644)
	log := filepath.Join(d, "calls.log")
	applied := filepath.Join(d, "applied.conf")
	bin := fakeBin(t, map[string]string{
		"wg":       `echo "wg $*" >>"$FTN_LOG"; cat "$3" >"$FTN_APPLIED"`,
		"wg-quick": `echo "[Interface]"`,
		"ip":       `echo "ip $*" >>"$FTN_LOG"`,
	})
	out, err := bash(t, []string{"FTN_WG_SYNC_CONF=" + conf, "WG_BIN=" + filepath.Join(bin, "wg"), "WG_QUICK_BIN=" + filepath.Join(bin, "wg-quick"),
		"IP_BIN=" + filepath.Join(bin, "ip"), "FTN_LOG=" + log, "FTN_APPLIED=" + applied, "FTN_WG_SYNC_STATUS=" + filepath.Join(d, "status")}, "bash ./ftnbilling-wg-sync")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got, _ := os.ReadFile(applied)
	for _, want := range []string{"AllowedIPs = 10.99.0.2/32, 10.6.1.0/24", "AllowedIPs = 10.99.0.7/32, 10.6.7.0/24"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("not applied: %q\n%s", want, got)
		}
	}
	for _, bad := range []string{"10.99.0.3/32", "10.8.1.0", "10.6.2.0/25", "10.99.0.6/32", "10.6.4.0/24\n"} {
		if strings.Contains(string(got), bad) {
			t.Errorf("must be skipped: %q\n%s", bad, got)
		}
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "ip route replace 10.6.0.0/16 dev wg0") {
		t.Errorf("the ONU range is not routed into wg0: %q", calls)
	}
	if !strings.Contains(out, "already given to another peer") || !strings.Contains(out, "outside 10.6.0.0/16") {
		t.Errorf("the reasons are not reported: %s", out)
	}
}
