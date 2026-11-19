package sites

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Remote WebFig goes through the server over the tunnel: the bootstrap leaves
// the router's www service on the REST port, answering the admin LAN and the
// server's tunnel address only, and the firewall lets only the server's tunnel
// address reach that port. The server never writes /ip/service over REST (the
// policy refuses it), so this is the whole router side of the feature.
func TestBootstrapKeepsWebFigForTheAdminLANAndTheServerOnly(t *testing.T) {
	files, _ := filepath.Glob("testdata/bootstrap_*.rsc")
	if len(files) == 0 {
		t.Fatal("no golden files")
	}
	www := regexp.MustCompile(`/ip/service set www port=(\d+) address=(\S+) disabled=no`)
	wwwSSL := regexp.MustCompile(`/ip/service set www-ssl address=(\S+) disabled=yes`)
	rest := regexp.MustCompile(`chain=input action=accept protocol=tcp in-interface=wg-ftn src-address=(\S+) dst-port=(\d+) comment="ftn: wg rest"`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		w := www.FindStringSubmatch(src)
		ss := wwwSSL.FindStringSubmatch(src)
		rr := rest.FindStringSubmatch(src)
		if w == nil || ss == nil || rr == nil {
			t.Errorf("%s: www %v, www-ssl %v, rest rule %v", f, w != nil, ss != nil, rr != nil)
			continue
		}
		port, addr := w[1], w[2]
		if rr[2] != port {
			t.Errorf("%s: www listens on %s but the firewall opens %s to the server", f, port, rr[2])
		}
		parts := strings.Split(addr, ",")
		if len(parts) != 2 || parts[1] != rr[1]+"/32" || strings.HasPrefix(parts[0], "0.0.0.0") || !strings.HasSuffix(parts[0], "/24") {
			t.Errorf("%s: www address %q is not the admin LAN plus the server /32", f, addr)
		}
		if ss[1] != addr {
			t.Errorf("%s: www-ssl address %q differs from www %q", f, ss[1], addr)
		}
		// the www port is accepted on input from the tunnel's server address only; flats get it dropped
		for _, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "chain=input") && strings.Contains(line, "action=accept") && strings.Contains(line, "dst-port="+port) && !strings.Contains(line, "ftn: wg rest") {
				t.Errorf("%s: another input rule accepts the www port: %s", f, strings.TrimSpace(line))
			}
		}
		if !strings.Contains(src, "action=drop protocol=tcp in-interface=br-customers dst-port="+port) {
			t.Errorf("%s: flats are not blocked from the www port", f)
		}
	}
}
