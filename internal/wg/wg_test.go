package wg

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		os.WriteFile(p, got, 0o644)
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestPublicKeyDerivesFromPrivate(t *testing.T) {
	// RFC 7748 section 6.1, Alice's key pair.
	priv := hexKey("77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	want := hexKey("8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a")
	got, err := PublicKey(priv)
	if err != nil || got != want {
		t.Fatalf("got %q want %q (%v)", got, want, err)
	}
	p, pub, err := GenerateKeypair()
	if err != nil || !ValidKey(p) || !ValidKey(pub) || p == pub {
		t.Fatalf("bad keypair %q %q %v", p, pub, err)
	}
	if d, _ := PublicKey(p); d != pub {
		t.Fatal("generated public key does not derive from its private key")
	}
	p2, _, _ := GenerateKeypair()
	if p2 == p {
		t.Fatal("keys must be random")
	}
}

func hexKey(h string) string {
	b := make([]byte, 32)
	for i := range b {
		var v byte
		for _, c := range h[2*i : 2*i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			default:
				v |= byte(c-'a') + 10
			}
		}
		b[i] = v
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestValidKey(t *testing.T) {
	good := strings.Repeat("A", 43) + "="
	for s, want := range map[string]bool{good: true, "": false, "short": false, strings.Repeat("A", 44): false, strings.Repeat("!", 44): false, good + "A": false} {
		if ValidKey(s) != want {
			t.Errorf("ValidKey(%q)=%v", s, !want)
		}
	}
}

func k(n byte) string {
	b := bytes.Repeat([]byte{n}, 32)
	return base64.StdEncoding.EncodeToString(b)
}

func TestPeersGolden(t *testing.T) {
	peers := []Peer{
		{PublicKey: k(3), TunnelIP: "10.99.0.10"},
		{PublicKey: k(1), TunnelIP: "10.99.0.2"},
		{PublicKey: "bogus", TunnelIP: "10.99.0.5"},
		{PublicKey: k(2), TunnelIP: "not-an-ip"},
		{PublicKey: k(4), TunnelIP: "10.99.0.3"},
	}
	got := RenderPeers(peers)
	golden(t, "peers.conf", got)
	// Everything rendered must match the only line shapes the sync script allows.
	for _, l := range strings.Split(strings.TrimSpace(string(got)), "\n") {
		if l == "" || l == "[Peer]" || strings.HasPrefix(l, "PublicKey = ") || strings.HasPrefix(l, "AllowedIPs = 10.99.") && strings.HasSuffix(l, "/32") {
			continue
		}
		t.Fatalf("unexpected line %q", l)
	}
	if string(RenderPeers(nil)) != "" {
		t.Fatal("no peers -> empty file")
	}
}

func TestWritePeersTouchesApply(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wg")
	if err := WritePeers(dir, []Peer{{PublicKey: k(1), TunnelIP: "10.99.0.2"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "peers.conf"))
	if err != nil || !strings.Contains(string(b), "10.99.0.2/32") {
		t.Fatalf("%s %v", b, err)
	}
	a1, _ := os.ReadFile(filepath.Join(dir, "apply"))
	time.Sleep(2 * time.Millisecond)
	if err := WritePeers(dir, nil); err != nil {
		t.Fatal(err)
	}
	a2, _ := os.ReadFile(filepath.Join(dir, "apply"))
	if len(a1) == 0 || bytes.Equal(a1, a2) {
		t.Fatal("apply must change on every write so the path unit fires")
	}
}

func TestParseDump(t *testing.T) {
	in := k(1) + "\t203.0.113.7:51820\t10.99.0.2/32\t1700000000\t1234\t5678\n" +
		k(2) + "\t(none)\t10.99.0.3/32\t0\t0\t0\n" +
		"garbage line\n" +
		k(3) + "\tx\t10.99.0.4/32\n"
	m, err := ParseDump(strings.NewReader(in))
	if err != nil || len(m) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	a := m[k(1)]
	if a.Endpoint != "203.0.113.7:51820" || a.RxBytes != 1234 || a.TxBytes != 5678 || a.LastHandshake.Unix() != 1700000000 {
		t.Fatalf("%+v", a)
	}
	age, ok := a.HandshakeAge(time.Unix(1700000100, 0))
	if !ok || age != 100*time.Second {
		t.Fatal(age, ok)
	}
	b := m[k(2)]
	if b.Endpoint != "" || !b.LastHandshake.IsZero() {
		t.Fatalf("%+v", b)
	}
	if _, ok := b.HandshakeAge(time.Now()); ok {
		t.Fatal("never-handshaken peer has no age")
	}
}

func TestReadDumpMissingIsUnknown(t *testing.T) {
	_, _, err := ReadDump(filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, ErrStatusUnknown) {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "d")
	os.WriteFile(p, []byte(k(1)+"\t(none)\t10.99.0.2/32\t5\t1\t2\n"), 0o640)
	m, mt, err := ReadDump(p)
	if err != nil || len(m) != 1 || mt.IsZero() {
		t.Fatal(m, mt, err)
	}
}
