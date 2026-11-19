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
	for _, want := range []string{"remote WinBox: WireGuard wg-mgmt (10.98.0.0/24, udp/51821)", "ufw allow 51821/udp", "forwarding block", "systemctl start wg-quick@wg-mgmt", "91-ftnbilling-mgmt.conf"} {
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
