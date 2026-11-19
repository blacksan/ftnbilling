package deploy

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A fake wg and qrencode stand in for the real programs. The fake wg keeps the
// "running interface" in $FAKE/peers (public key and address per line), logs
// every call's arguments to $FAKE/wg.log and never logs what it reads on stdin.
const fakeWG = `#!/bin/bash
D=$FAKE
echo "wg $*" >>"$D/wg.log"
case "$1" in
  genkey) n=$(( $(cat "$D/n" 2>/dev/null || echo 0) + 1 )); echo $n >"$D/n"; printf 'CLIENTPRIVATEKEY%03d00000000000000000000000=\n' "$n" ;;
  pubkey) read -r k; printf 'PUB%s=\n' "$(printf '%s' "$k" | sha256sum | cut -c1-40)" ;;
  show)
    [ -e "$D/up" ] || exit 1
    case "${3:-}" in
      allowed-ips) while read -r p a; do printf '%s\t%s/32\n' "$p" "$a"; done <"$D/peers" ;;
      latest-handshakes) while read -r p a; do printf '%s\t%s\n' "$p" "$(grep "^$p " "$D/hs" 2>/dev/null | cut -d' ' -f2 || true)"; done <"$D/peers" | sed 's/\t$/\t0/' ;;
      peers) cut -d' ' -f1 "$D/peers" ;;
    esac ;;
  set)
    [ -e "$D/up" ] || exit 1
    if [ "$5" = remove ]; then grep -v "^$4 " "$D/peers" >"$D/peers.new" || true; mv "$D/peers.new" "$D/peers"
    else echo "$4 ${6%/32}" >>"$D/peers"; fi ;;
esac
`

const fakeQR = `#!/bin/bash
echo "qrencode $*" >>"$FAKE/qr.log"
n=$(wc -c)
echo "QRCODE[$n bytes of profile on stdin]"
`

// pubOf is what the fake "wg pubkey" prints for a private key: a hash, so the public key
// (which is stored) never contains the private key (which must not be).
func pubOf(priv string) string {
	return "PUB" + fmt.Sprintf("%x", sha256.Sum256([]byte(priv)))[:40] + "="
}

// clientKey is the n-th private key the fake "wg genkey" hands out.
func clientKey(n int) string { return fmt.Sprintf("CLIENTPRIVATEKEY%03d00000000000000000000000=", n) }

const serverKey = "SERVERPRIVATEKEY0000000000000000000000000="

type mgmtRig struct {
	t    *testing.T
	dir  string
	conf string
	env  string
}

func newMgmtRig(t *testing.T, subnet string) *mgmtRig {
	t.Helper()
	bashOrSkip(t)
	dir := t.TempDir()
	r := &mgmtRig{t: t, dir: dir, conf: filepath.Join(dir, "wg-mgmt.conf"), env: filepath.Join(dir, "mgmt.env")}
	write := func(name, body string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("wg", fakeWG, 0o755)
	write("qrencode", fakeQR, 0o755)
	write("up", "", 0o644)
	write("peers", "", 0o644)
	write("mgmt.env", fmt.Sprintf("MGMT_IF=wg-mgmt\nMGMT_SUBNET=%s\nMGMT_PORT=51821\nMGMT_ENDPOINT_HOST=billing.example.co.ke\nWG_SUBNET=10.99.0.0/22\n", subnet), 0o644)
	write("wg-mgmt.conf", "[Interface]\nAddress = 10.98.0.1/24\nListenPort = 51821\nMTU = 1380\nPrivateKey = SERVERPRIVATEKEY0000000000000000000000000=\n", 0o600)
	return r
}

func (r *mgmtRig) run(args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command("bash", append([]string{"ftnbilling-mgmt"}, args...)...)
	cmd.Env = append(os.Environ(), "FAKE="+r.dir, "FTN_MGMT_NOROOT=1", "FTN_MGMT_ENV="+r.env, "FTN_MGMT_CONF="+r.conf,
		"FTN_MGMT_LOCK="+filepath.Join(r.dir, "lock"), "WG_BIN="+filepath.Join(r.dir, "wg"), "QRENCODE_BIN="+filepath.Join(r.dir, "qrencode"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *mgmtRig) must(args ...string) string {
	r.t.Helper()
	out, err := r.run(args...)
	if err != nil {
		r.t.Fatalf("ftnbilling-mgmt %v: %v\n%s", args, err, out)
	}
	return out
}

func (r *mgmtRig) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(r.dir, name))
	return string(b)
}

func TestMgmtAddListRemove(t *testing.T) {
	r := newMgmtRig(t, "10.98.0.0/24")
	r.env = filepath.Join(r.dir, "mgmt.env")

	out := r.must("add", "laptop")
	for _, want := range []string{
		"[Interface]", "PrivateKey = " + clientKey(1), "Address = 10.98.0.2/32", "MTU = 1380",
		"[Peer]", "PublicKey = " + pubOf(serverKey), "AllowedIPs = 10.99.0.0/22",
		"Endpoint = billing.example.co.ke:51821", "PersistentKeepalive = 25", "QRCODE[", "ONCE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("add output lacks %q:\n%s", want, out)
		}
	}
	// A split tunnel with no DNS: never the whole internet, never the management subnet.
	for _, bad := range []string{"DNS", "0.0.0.0/0", "::/0", "AllowedIPs = 10.98"} {
		if strings.Contains(out, bad) {
			t.Errorf("the profile contains %q:\n%s", bad, out)
		}
	}
	if n := strings.Count(out, "PrivateKey = "+clientKey(1)); n != 1 {
		t.Errorf("the device key is printed %d times, want once", n)
	}
	if !regexp.MustCompile(`qrencode -t ansiutf8`).MatchString(r.read("qr.log")) {
		t.Errorf("qrencode was not called with -t ansiutf8: %q", r.read("qr.log"))
	}
	conf := r.read("wg-mgmt.conf")
	for _, want := range []string{"# ftn-mgmt peer: laptop", "[Peer]", "PublicKey = " + pubOf(clientKey(1)), "AllowedIPs = 10.98.0.2/32"} {
		if !strings.Contains(conf, want) {
			t.Errorf("saved config lacks %q:\n%s", want, conf)
		}
	}
	if !strings.Contains(r.read("wg.log"), "wg set wg-mgmt peer "+pubOf(clientKey(1))+" allowed-ips 10.98.0.2/32") {
		t.Errorf("the peer was not applied to the running interface:\n%s", r.read("wg.log"))
	}

	r.must("add", "phone")
	if got := r.read("peers"); !strings.Contains(got, " 10.98.0.2") || !strings.Contains(got, " 10.98.0.3") {
		t.Errorf("running peers: %q", got)
	}
	// The conf still starts with the server's own settings and keeps the server key.
	if c := r.read("wg-mgmt.conf"); !strings.HasPrefix(c, "[Interface]\nAddress = 10.98.0.1/24\n") || !strings.Contains(c, "SERVERPRIVATEKEY") {
		t.Errorf("the interface section changed:\n%s", c)
	}

	// list: name, address, last handshake
	pub := pubOf(clientKey(1))
	os.WriteFile(filepath.Join(r.dir, "hs"), []byte(fmt.Sprintf("%s %d\n", pub, time.Now().Add(-5*time.Minute).Unix())), 0o644)
	list := r.must("list")
	lines := strings.Split(strings.TrimSpace(list), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("list:\n%s", list)
	}
	if !regexp.MustCompile(`^laptop\s+10\.98\.0\.2\s+5 min ago$`).MatchString(lines[1]) {
		t.Errorf("laptop row: %q", lines[1])
	}
	if !regexp.MustCompile(`^phone\s+10\.98\.0\.3\s+never$`).MatchString(lines[2]) {
		t.Errorf("phone row: %q", lines[2])
	}
	if strings.Contains(list, "PrivateKey") || strings.Contains(list, "CLIENTPRIVATEKEY") {
		t.Errorf("list shows key material:\n%s", list)
	}

	// remove: cut off live and forgotten in the config; the address is free again.
	r.must("remove", "laptop")
	if strings.Contains(r.read("wg-mgmt.conf"), "laptop") || strings.Contains(r.read("wg-mgmt.conf"), "10.98.0.2/32") {
		t.Errorf("laptop is still in the config:\n%s", r.read("wg-mgmt.conf"))
	}
	if !strings.Contains(r.read("wg.log"), "wg set wg-mgmt peer "+pub+" remove") {
		t.Errorf("the peer was not removed from the running interface:\n%s", r.read("wg.log"))
	}
	if !strings.Contains(r.read("wg-mgmt.conf"), "# ftn-mgmt peer: phone") {
		t.Errorf("removing one device removed another:\n%s", r.read("wg-mgmt.conf"))
	}
	if out, err := r.run("remove", "laptop"); err == nil || !strings.Contains(out, "no device called") {
		t.Errorf("removing an unknown device: %v %s", err, out)
	}
	out = r.must("add", "tablet")
	if !strings.Contains(out, "Address = 10.98.0.2/32") {
		t.Errorf("the lowest free address is reused:\n%s", out)
	}
	if out, err := r.run("add", "tablet"); err == nil || !strings.Contains(out, "already exists") {
		t.Errorf("a duplicate name: %v %s", err, out)
	}
}

// The device's private key is shown once and never stored: not in the config,
// the settings, a temporary file, or any program's command line.
func TestMgmtNoPrivateKeyLeftOnDisk(t *testing.T) {
	r := newMgmtRig(t, "10.98.0.0/24")
	out := r.must("add", "laptop")
	m := regexp.MustCompile(`PrivateKey = (CLIENTPRIVATEKEY\w+=)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no device key in the output:\n%s", out)
	}
	key := m[1]
	n := 0
	filepath.WalkDir(r.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n++
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), key) {
			t.Errorf("%s holds the device's private key", p)
		}
		return nil
	})
	if n < 5 {
		t.Fatalf("walked only %d files", n)
	}
	if left, _ := filepath.Glob(r.conf + ".*"); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
	if fi, err := os.Stat(r.conf); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode: %v %v", fi, err)
	}
	// "list" and "remove" never print it either.
	if l := r.must("list"); strings.Contains(l, key) {
		t.Error("list prints the key")
	}
	if l := r.must("remove", "laptop"); strings.Contains(l, key) {
		t.Error("remove prints the key")
	}
}

func TestMgmtNameValidation(t *testing.T) {
	r := newMgmtRig(t, "10.98.0.0/24")
	for _, bad := range []string{"Laptop", "my laptop", "a_b", "x.y", "../etc", "é", strings.Repeat("a", 33), "a/b", "a;b", "$(id)", ""} {
		out, err := r.run("add", bad)
		if err == nil {
			t.Errorf("add %q was accepted:\n%s", bad, out)
		}
		if strings.Contains(r.read("wg-mgmt.conf"), "ftn-mgmt peer") {
			t.Fatalf("add %q changed the config", bad)
		}
	}
	for _, bad := range []string{"Laptop", "a b", "a;b"} {
		if _, err := r.run("remove", bad); err == nil {
			t.Errorf("remove %q was accepted", bad)
		}
	}
	for _, good := range []string{"a", "my-laptop-2", strings.Repeat("a", 32), "0"} {
		if out, err := r.run("add", good); err != nil {
			t.Errorf("add %q refused: %v\n%s", good, err, out)
		}
	}
	if out, err := r.run("bogus"); err == nil || !strings.Contains(out, "ftnbilling-mgmt add") {
		t.Errorf("unknown command: %v %s", err, out)
	}
}

func TestMgmtRootOnlyAndErrors(t *testing.T) {
	r := newMgmtRig(t, "10.98.0.0/24")
	if os.Geteuid() != 0 {
		cmd := exec.Command("bash", "ftnbilling-mgmt", "list")
		cmd.Env = append(os.Environ(), "FTN_MGMT_ENV="+r.env, "FTN_MGMT_CONF="+r.conf)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "run as root") {
			t.Errorf("a non-root run: %v %s", err, out)
		}
	}
	// no config: remote WinBox is not set up on this server
	os.Remove(r.conf)
	if out, err := r.run("list"); err == nil || !strings.Contains(out, "not set up") {
		t.Errorf("no config: %v %s", err, out)
	}
}

// With the interface down the device is still saved; a small subnet runs out of addresses cleanly.
func TestMgmtInterfaceDownAndSubnetFull(t *testing.T) {
	r := newMgmtRig(t, "10.98.0.0/29")
	os.WriteFile(r.env, []byte("MGMT_IF=wg-mgmt\nMGMT_SUBNET=10.98.0.0/29\nMGMT_PORT=51822\nMGMT_ENDPOINT_HOST=203.0.113.7\nWG_SUBNET=10.99.0.0/22\n"), 0o644)
	os.Remove(filepath.Join(r.dir, "up"))
	out := r.must("add", "d1")
	if !strings.Contains(out, "Address = 10.98.0.2/32") || !strings.Contains(out, "Endpoint = 203.0.113.7:51822") || !strings.Contains(out, "not up") {
		t.Errorf("down interface:\n%s", out)
	}
	for i := 2; i <= 5; i++ { // /29: .2 to .6
		r.must("add", fmt.Sprintf("d%d", i))
	}
	if out, err := r.run("add", "d6"); err == nil || !strings.Contains(out, "no free address") {
		t.Errorf("full subnet: %v %s", err, out)
	}
	if got := strings.Count(r.read("wg-mgmt.conf"), "[Peer]"); got != 5 {
		t.Errorf("%d peers saved, want 5", got)
	}
}

// install.sh puts the tool in place, checks it is in the bundle and says so in the banner.
func TestInstallInstallsMgmtTool(t *testing.T) {
	src, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{
		"for t in wg-sync backup admin-reset wipe doctor mgmt; do install -m 0755 \"$ASSETS/$APP-$t\" \"$SBIN/$APP-$t\"; done",
		"$APP-doctor $APP-mgmt $APP-backup.service", // the bundle check
		`echo " Remote WinBox: sudo $APP-mgmt add <your-device>"`,
		"MGMT_PORT (51821", "MGMT_SUBNET (10.98.0.0/24",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("install.sh lacks %q", want)
		}
	}
	fi, err := os.Stat("ftnbilling-mgmt")
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("deploy/ftnbilling-mgmt must exist and be executable: %v", err)
	}
	ci, _ := os.ReadFile("../.github/workflows/ci.yml")
	if strings.Count(string(ci), "deploy/ftnbilling-mgmt") < 2 {
		t.Error("CI must run bash -n and shellcheck on deploy/ftnbilling-mgmt")
	}
}

// The dry run shows the step, the port and the firewall block, and still
// creates nothing of the retired admin VPN.
func TestInstallDryRunMgmt(t *testing.T) {
	bashOrSkip(t)
	run := func(extra ...string) (string, error) {
		cmd := exec.Command("bash", "install.sh")
		cmd.Env = append(os.Environ(), append([]string{"DRY_RUN=1", "DOMAIN=billing.example.co.ke", "PUBLIC_IP=203.0.113.7"}, extra...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"remote WinBox: WireGuard wg-mgmt (10.98.0.0/24, udp/51821)", "ufw allow 51821/udp", "forwarding block", "iptables-restore --test", "ufw default deny routed", "systemctl start wg-quick@wg-mgmt", "91-ftnbilling-mgmt.conf"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q", want)
		}
	}
	// settings are validated before anything is done
	for name, env := range map[string]string{
		"public subnet": "MGMT_SUBNET=8.8.8.0/24", "overlaps wg0": "MGMT_SUBNET=10.99.1.0/24", "too big": "MGMT_SUBNET=10.0.0.0/8",
		"too small": "MGMT_SUBNET=10.98.0.0/30", "not an address": "MGMT_SUBNET=10.98.0/24", "bad octet": "MGMT_SUBNET=10.300.0.0/24",
		"same port as wg0": "MGMT_PORT=51820", "not a port": "MGMT_PORT=abc", "port too big": "MGMT_PORT=70000",
	} {
		if o, err := run(env); err == nil {
			t.Errorf("%s (%s) was accepted:\n%s", name, env, o)
		}
	}
	if o, err := run("MGMT_SUBNET=172.20.9.0/24", "MGMT_PORT=51999"); err != nil || !strings.Contains(o, "(172.20.9.0/24, udp/51999)") || !strings.Contains(o, "ufw allow 51999/udp") {
		t.Errorf("custom settings: %v\n%s", err, o)
	}
	// with the firewall left alone the step is skipped: no forwarding without the rules
	if o, err := run("NO_FIREWALL=1"); err != nil || !strings.Contains(o, "Remote WinBox needs this server to forward") || strings.Contains(o, "ip_forward=1") {
		t.Errorf("NO_FIREWALL=1: %v\n%s", err, o)
	}
}

// The ufw block: first in the chain, replaced (never duplicated) on a re-run, and
// it allows nothing beyond WinBox and ping towards the routers.
func TestInstallMgmtFirewallBlock(t *testing.T) {
	bashOrSkip(t)
	before := `*filter
:ufw-before-input - [0:0]
:ufw-before-output - [0:0]
:ufw-before-forward - [0:0]
:ufw-not-local - [0:0]
# End required lines

-A ufw-before-input -i lo -j ACCEPT
-A ufw-before-forward -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A ufw-before-forward -p icmp --icmp-type echo-request -j ACCEPT
COMMIT
`
	script := "set -e\n" + fnSrc(t, "mgmt_rules") + "\n" + fnSrc(t, "mgmt_before_rules") + `
MGMT_IF=wg-mgmt; WG_IF=wg0; WG_SUBNET=10.99.0.0/22; MGMT_SUBNET=10.98.0.0/24; MGMT_MARK=ftnbilling-mgmt
printf '%s' "$1" >"$2/in"
mgmt_before_rules <"$2/in" >"$2/once"
mgmt_before_rules <"$2/once" >"$2/twice"
MGMT_SUBNET=172.20.9.0/24
mgmt_before_rules <"$2/twice" >"$2/changed"
`
	dir := t.TempDir()
	if out, err := exec.Command("bash", "-c", script, "bash", before, dir).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	rd := func(n string) string { b, _ := os.ReadFile(filepath.Join(dir, n)); return string(b) }
	once, twice, changed := rd("once"), rd("twice"), rd("changed")
	if once != twice {
		t.Errorf("a second pass changed the file:\n%s\n---\n%s", once, twice)
	}
	if strings.Count(changed, "# BEGIN ftnbilling-mgmt") != 1 || strings.Contains(changed, "10.98.0.0/24") || !strings.Contains(changed, "-s 172.20.9.0/24") {
		t.Errorf("a changed subnet must replace the block:\n%s", changed)
	}
	// the original lines are all still there, after the block
	for _, l := range strings.Split(strings.TrimSpace(before), "\n") {
		if !strings.Contains(once, l+"\n") {
			t.Errorf("original line %q lost", l)
		}
	}
	blk := once[strings.Index(once, "# BEGIN"):strings.Index(once, "# END ftnbilling-mgmt")]
	if strings.Index(once, "# End required lines") > strings.Index(once, "# BEGIN") || strings.Index(once, "# END ftnbilling-mgmt") > strings.Index(once, "-A ufw-before-input -i lo") {
		t.Error("the block must sit between '# End required lines' and ufw's own rules")
	}
	want := []string{
		"-A ufw-before-input -i wg-mgmt -j DROP",
		"-A ufw-before-forward -i wg-mgmt -m conntrack --ctstate INVALID -j DROP",
		"-A ufw-before-forward -i wg-mgmt -o wg0 -s 10.98.0.0/24 -d 10.99.0.0/22 -p tcp --dport 8291 -j ACCEPT",
		"-A ufw-before-forward -i wg-mgmt -o wg0 -s 10.98.0.0/24 -d 10.99.0.0/22 -p icmp --icmp-type echo-request -j ACCEPT",
		"-A ufw-before-forward -i wg-mgmt -j DROP",
		"-A ufw-before-forward -i wg0 -o wg-mgmt -s 10.99.0.0/22 -d 10.98.0.0/24 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
		"-A ufw-before-forward -i wg0 -j DROP", // router to router, or router to anywhere else: never
		"-A ufw-before-forward -o wg-mgmt -j DROP",
	}
	var got []string
	for _, l := range strings.Split(blk, "\n") {
		if strings.HasPrefix(l, "-A ") {
			got = append(got, l)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("block rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// every ACCEPT comes before the catch-all DROP of the interface, and none is NAT or mentions the internet
	if strings.Contains(blk, "MASQUERADE") || strings.Contains(blk, "-t nat") || strings.Contains(blk, "0.0.0.0/0") {
		t.Error("the block must not NAT or open anything wider")
	}
}

// install.sh validates MGMT_SUBNET in bash the way the app does in Go.
func TestInstallMgmtSettingsHelpers(t *testing.T) {
	bashOrSkip(t)
	script := "set -e\n"
	for _, f := range []string{"ip2int", "in_private_v4", "prefixes_overlap", "int2ip_masked", "mgmt_settings"} {
		script += fnSrcOne(t, f) + "\n"
	}
	script += `die() { echo "DIE: $*"; exit 3; }
envget() { :; }
WG_PORT=51820; WG_SUBNET=10.99.0.0/22
mgmt_settings
echo "$MGMT_SUBNET $MGMT_NET $MGMT_SERVER_IP $MGMT_PORT"
`
	run := func(env ...string) string {
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(os.Environ(), env...)
		out, _ := cmd.CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	for _, c := range []struct{ env, want string }{
		{"", "10.98.0.0/24 10.98.0.0 10.98.0.1 51821"},
		{"MGMT_SUBNET=10.98.0.77/24", "10.98.0.0/24 10.98.0.0 10.98.0.1 51821"}, // normalised to the network
		{"MGMT_SUBNET=192.168.50.128/25", "192.168.50.128/25 192.168.50.128 192.168.50.129 51821"},
		{"MGMT_SUBNET=172.31.0.0/16", "172.31.0.0/16 172.31.0.0 172.31.0.1 51821"},
		{"MGMT_SUBNET=172.32.0.0/24", "DIE:"}, // just outside 172.16/12
		{"MGMT_SUBNET=100.64.0.0/24", "DIE:"},
		{"MGMT_SUBNET=10.99.3.0/24", "DIE:"}, // inside WG_SUBNET
		{"MGMT_SUBNET=10.96.0.0/14", "DIE:"}, // contains WG_SUBNET (and is too big)
		{"MGMT_SUBNET=10.98.0.0/24 ", "DIE:"},
	} {
		got := run(c.env)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("%q: got %q, want %q", c.env, got, c.want)
		}
	}
}

// fnSrcOne is fnSrc for functions whose body has no heredoc and may be a one-liner.
func fnSrcOne(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, name+"() {") {
			continue
		}
		if strings.HasSuffix(l, "}") && !strings.HasSuffix(l, "{") {
			return l
		}
		for j := i; j < len(lines); j++ {
			if lines[j] == "}" {
				return strings.Join(lines[i:j+1], "\n")
			}
		}
	}
	t.Fatalf("function %s not found in install.sh", name)
	return ""
}

// ---- the installer's firewall step, run with a fake ufw, iptables-restore and sysctl ----------

const fakeUFW = `#!/bin/bash
echo "ufw $*" >>"$FAKE/calls.log"
case "$1 ${2:-}" in
  "status ") [ -e "$FAKE/inactive" ] && echo "Status: inactive" || echo "Status: active" ;;
  "reload ") if [ -e "$FAKE/reload_fail" ]; then echo "ERROR: problem running ufw-init" >&2; exit 1; fi ;;
esac
exit 0
`

const fakeIPTRestore = `#!/bin/bash
echo "iptables-restore $*" >>"$FAKE/calls.log"
cat >"$FAKE/ir.in"
if [ -e "$FAKE/ir_fail" ]; then echo "iptables-restore: line 9 failed" >&2; exit 1; fi
`

const fakeSysctl = `#!/bin/bash
echo "sysctl $*" >>"$FAKE/calls.log"
`

type fwRig struct {
	t    *testing.T
	dir  string
	rule string // the fake /etc/ufw/before.rules
	sysf string // the fake /etc/sysctl.d file
}

const beforeRules = `*filter
:ufw-before-input - [0:0]
:ufw-before-output - [0:0]
:ufw-before-forward - [0:0]
:ufw-not-local - [0:0]
# End required lines

-A ufw-before-input -i lo -j ACCEPT
-A ufw-before-forward -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
COMMIT
`

func newFWRig(t *testing.T, before string) *fwRig {
	t.Helper()
	bashOrSkip(t)
	dir := t.TempDir()
	r := &fwRig{t: t, dir: dir, rule: filepath.Join(dir, "before.rules"), sysf: filepath.Join(dir, "91-ftnbilling-mgmt.conf")}
	os.Mkdir(filepath.Join(dir, "bin"), 0o755)
	for name, body := range map[string]string{"ufw": fakeUFW, "iptables-restore": fakeIPTRestore, "sysctl": fakeSysctl} {
		if err := os.WriteFile(filepath.Join(dir, "bin", name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(r.rule, []byte(before), 0o644)
	return r
}

func (r *fwRig) flag(name string) { os.WriteFile(filepath.Join(r.dir, name), nil, 0o644) }

func (r *fwRig) calls() string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "calls.log"))
	return string(b)
}

func (r *fwRig) file(p string) string { b, _ := os.ReadFile(p); return string(b) }

// run runs body (shell) with the installer's functions loaded; vars are extra assignments.
func (r *fwRig) run(vars, body string) (string, error) {
	r.t.Helper()
	script := "set -euo pipefail\n"
	for _, f := range []string{"have", "legacy_rule_numbers", "mgmt_before_rules", "mgmt_validate_rules", "mgmt_firewall", "mgmt_stale_ports",
		"mgmt_forwarding", "mgmt_forwarding_off", "wireguard_mgmt", "firewall"} {
		script += fnSrcOne(r.t, f) + "\n"
	}
	script += fnSrc(r.t, "mgmt_rules") + "\n"
	script += `MGMT_SYSCTL=$FTN_MGMT_SYSCTL
DRY=0; MGMT_IF=wg-mgmt; WG_IF=wg0; WG_SUBNET=10.99.0.0/22; MGMT_SUBNET=10.98.0.0/24; MGMT_PORT=51821; WG_PORT=51820
MGMT_MARK=ftnbilling-mgmt; MGMT_ON=1; ENVF=/nonexistent; WG_SERVER_IP=10.99.0.1
step() { :; }; note() { :; }; warn() { :; }; run() { "$@"; }; runq() { "$@" >/dev/null; }; try() { "$@" >/dev/null 2>&1 || true; }
die() { echo "DIE: $*" >&2; exit 3; }
writef() { cat >"$3"; }
` + vars + "\n" + body + "\n"
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(r.dir, "bin")+":"+os.Getenv("PATH"), "FAKE="+r.dir,
		"FTN_UFW_BEFORE_RULES="+r.rule, "FTN_MGMT_SYSCTL="+r.sysf)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallMgmtFirewallRefusesAndKeepsTheOriginal(t *testing.T) {
	// A BEGIN marker with no END: refused, the file untouched, no stray files.
	broken := beforeRules + "# BEGIN ftnbilling-mgmt (hand edited)\n-A ufw-before-forward -i wg-mgmt -j ACCEPT\n"
	r := newFWRig(t, broken)
	out, err := r.run("", "mgmt_firewall")
	if err == nil || !strings.Contains(out, "without its") || !strings.Contains(out, "# END ftnbilling-mgmt") {
		t.Errorf("unmatched marker: %v\n%s", err, out)
	}
	if got := r.file(r.rule); got != broken {
		t.Errorf("the file changed:\n%s", got)
	}
	if left, _ := filepath.Glob(r.rule + ".*"); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	if strings.Contains(r.calls(), "reload") {
		t.Error("ufw was reloaded")
	}
	// The same check at the function level: a clear error on stderr and a failing status.
	if out, err := exec.Command("bash", "-c", "set -e\n"+fnSrc(t, "mgmt_rules")+"\n"+fnSrcOne(t, "mgmt_before_rules")+"\nMGMT_MARK=ftnbilling-mgmt; MGMT_IF=a; WG_IF=b; WG_SUBNET=1; MGMT_SUBNET=2\nprintf '# BEGIN ftnbilling-mgmt\\nx\\n' | mgmt_before_rules >/dev/null").CombinedOutput(); err == nil || !strings.Contains(string(out), "no matching") {
		t.Errorf("mgmt_before_rules on an unmatched marker: %v %s", err, out)
	}

	// iptables-restore --test says no: nothing is written, nothing reloaded.
	r = newFWRig(t, beforeRules)
	r.flag("ir_fail")
	out, err = r.run("", "mgmt_firewall")
	if err == nil || !strings.Contains(out, "refused by iptables-restore --test") || !strings.Contains(out, "line 9 failed") {
		t.Errorf("failed validation: %v\n%s", err, out)
	}
	if r.file(r.rule) != beforeRules || strings.Contains(r.calls(), "reload") {
		t.Error("a rejected rule set changed the file or reloaded ufw")
	}

	// ufw reload fails: the old file is put back, the failure is not hidden.
	r = newFWRig(t, beforeRules)
	r.flag("reload_fail")
	out, err = r.run("", "mgmt_firewall")
	if err == nil || !strings.Contains(out, "ufw reload failed") || !strings.Contains(out, "previous") || !strings.Contains(out, "problem running ufw-init") {
		t.Errorf("failed reload: %v\n%s", err, out)
	}
	if r.file(r.rule) != beforeRules {
		t.Errorf("the original was not put back:\n%s", r.file(r.rule))
	}

	// What was validated is the block itself, in a ruleset of its own (--test: nothing is loaded).
	r = newFWRig(t, beforeRules)
	if out, err := r.run("", "mgmt_firewall"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	in := r.file(filepath.Join(r.dir, "ir.in"))
	for _, want := range []string{"*filter\n", ":ufw-before-forward - [0:0]", "-A ufw-before-forward -i wg-mgmt -j DROP", "-A ufw-before-forward -i wg0 -j DROP", "COMMIT\n"} {
		if !strings.Contains(in, want) {
			t.Errorf("the validated ruleset lacks %q:\n%s", want, in)
		}
	}
	if !strings.Contains(r.calls(), "iptables-restore --test") {
		t.Errorf("iptables-restore ran without --test:\n%s", r.calls())
	}
	if !strings.Contains(r.file(r.rule), "# BEGIN ftnbilling-mgmt") || strings.Count(r.calls(), "ufw reload") != 1 {
		t.Errorf("success: block missing or reload count wrong:\n%s\n%s", r.file(r.rule), r.calls())
	}
	// A second run changes nothing and reloads nothing; an inactive ufw is not reloaded (enable loads it).
	before := r.file(r.rule)
	if out, err := r.run("", "mgmt_firewall"); err != nil || r.file(r.rule) != before || strings.Count(r.calls(), "ufw reload") != 1 {
		t.Errorf("second run: %v %s\n%s", err, out, r.calls())
	}
	r = newFWRig(t, beforeRules)
	r.flag("inactive")
	if out, err := r.run("", "mgmt_firewall"); err != nil || strings.Contains(r.calls(), "ufw reload") || !strings.Contains(r.file(r.rule), "# BEGIN") {
		t.Errorf("inactive ufw: %v %s\n%s", err, out, r.calls())
	}
}

func TestInstallFirewallStepDeniesRoutedAndDropsOldMgmtPort(t *testing.T) {
	r := newFWRig(t, beforeRules)
	// MGMT_PORT moved from 51999 (the settings file) and 51821 (the interface config) to 51850.
	out, err := r.run("MGMT_PORT=51850; MGMT_PORT_ENVFILE=51999; MGMT_PORT_CONF=51821", "firewall; echo FW-DONE")
	if err != nil || !strings.Contains(out, "FW-DONE") {
		t.Fatalf("%v\n%s", err, out)
	}
	calls := r.calls()
	idx := func(sub string) int { return strings.Index(calls, sub) }
	if idx("ufw default deny incoming") < 0 || idx("ufw default deny routed") < idx("ufw default deny incoming") || idx("ufw --force enable") < idx("ufw default deny routed") {
		t.Errorf("'ufw default deny routed' must come with the defaults, before enable:\n%s", calls)
	}
	for _, want := range []string{"ufw allow 51850/udp", "ufw delete allow 51999/udp", "ufw delete allow 51821/udp", "ufw allow 51820/udp"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q:\n%s", want, calls)
		}
	}
	for _, bad := range []string{"delete allow 51850", "delete allow 51820"} {
		if strings.Contains(calls, bad) {
			t.Errorf("%q must never happen:\n%s", bad, calls)
		}
	}
	// forwarding is switched on only after ufw is enabled (and the block is in)
	if idx("sysctl -q -p") < idx("ufw --force enable") || !strings.Contains(r.file(r.sysf), "net.ipv4.ip_forward=1") || !strings.Contains(r.file(r.rule), "# BEGIN ftnbilling-mgmt") {
		t.Errorf("forwarding order or files wrong:\n%s\n%s", calls, r.file(r.sysf))
	}

	// helper cases
	for _, c := range []struct{ vars, want string }{
		{"MGMT_PORT=51821; MGMT_PORT_ENVFILE=51821; MGMT_PORT_CONF=51821", ""},      // unchanged: nothing to delete
		{"MGMT_PORT=51821; MGMT_PORT_ENVFILE=; MGMT_PORT_CONF=", ""},                // first install
		{"MGMT_PORT=51999; MGMT_PORT_ENVFILE=51821; MGMT_PORT_CONF=51821", "51821"}, // one old port, named twice
		{"MGMT_PORT=51999; MGMT_PORT_ENVFILE=51821; MGMT_PORT_CONF=51822", "51821 51822"},
		{"MGMT_PORT=51999; MGMT_PORT_ENVFILE=51820; MGMT_PORT_CONF=51820", ""}, // wg0's port is never deleted
		{"MGMT_PORT=51999; MGMT_PORT_ENVFILE='51821; reboot'; MGMT_PORT_CONF=", ""},
	} {
		out, err := r.run(c.vars, "mgmt_stale_ports | tr '\\n' ' '")
		if err != nil || strings.TrimSpace(out) != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.vars, out, err, c.want)
		}
	}
}

// Remote WinBox off (NO_FIREWALL=1): forwarding set by an earlier run goes off again, and only that.
func TestInstallMgmtForwardingOffWithoutFirewall(t *testing.T) {
	ours := "# Fast Tech Net remote WinBox: devices on wg-mgmt are routed.\nnet.ipv4.ip_forward=1\n"
	r := newFWRig(t, beforeRules)
	os.WriteFile(r.sysf, []byte(ours), 0o644)
	out, err := r.run("NO_FIREWALL=1", "wireguard_mgmt; echo \"ON=$MGMT_ON\"")
	if err != nil || !strings.Contains(out, "ON=0") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(r.sysf); err == nil {
		t.Error("our sysctl file is still there")
	}
	if !strings.Contains(r.calls(), "sysctl -q -w net.ipv4.ip_forward=0") {
		t.Errorf("forwarding was not switched off:\n%s", r.calls())
	}
	if strings.Contains(r.calls(), "ufw") || r.file(r.rule) != beforeRules {
		t.Error("NO_FIREWALL=1 must not touch ufw")
	}
	// A sysctl file that is not ours (or none) is left alone, and forwarding stays as the admin set it.
	r = newFWRig(t, beforeRules)
	os.WriteFile(r.sysf, []byte("net.ipv4.ip_forward=1\n"), 0o644)
	if out, err := r.run("NO_FIREWALL=1", "wireguard_mgmt"); err != nil || r.file(r.sysf) == "" || strings.Contains(r.calls(), "sysctl") {
		t.Errorf("foreign sysctl file: %v %s\n%s", err, out, r.calls())
	}
	r = newFWRig(t, beforeRules)
	if out, err := r.run("NO_FIREWALL=1", "wireguard_mgmt"); err != nil || strings.Contains(r.calls(), "sysctl") {
		t.Errorf("no sysctl file: %v %s\n%s", err, out, r.calls())
	}
}

// The Drive snapshot carries wg-mgmt.conf next to wg0.conf, mode 600, only when it exists, and says how to put it back.
func TestBackupIncludesMgmtConfig(t *testing.T) {
	b, err := os.ReadFile("ftnbilling-backup")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"WGMGMTCONF=$ROOT/etc/wireguard/wg-mgmt.conf",
		`if [ -f "$WGMGMTCONF" ]; then`,
		`cp -a "$WGMGMTCONF" "$top/wireguard/"`,
		`chmod 600 "$top/wireguard/wg-mgmt.conf"`,
		"sudo cp -a $dir/wireguard/wg-mgmt.conf /etc/wireguard/wg-mgmt.conf",
		"sudo chmod 600 /etc/wireguard/wg-mgmt.conf",
		"sudo systemctl restart wg-quick@wg-mgmt",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("ftnbilling-backup lacks %q", want)
		}
	}
	doc, _ := os.ReadFile("BACKUP.md")
	for _, want := range []string{"wg-mgmt.conf", "chmod 600 /etc/wireguard/wg-mgmt.conf", "systemctl restart wg-quick@wg-mgmt"} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("BACKUP.md lacks %q", want)
		}
	}
}
