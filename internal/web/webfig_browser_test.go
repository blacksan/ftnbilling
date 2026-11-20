package web_test

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestWebFigInABrowser drives a real browser (Playwright) through the WebFig
// host handler against the fake WebFig-like router page. It needs Node,
// Playwright and Chrome, so it runs only with WEBFIG_PLAYWRIGHT=1 (NODE_PATH
// must reach the playwright package; CHROME_BIN names the browser).
func TestWebFigInABrowser(t *testing.T) {
	if os.Getenv("WEBFIG_PLAYWRIGHT") != "1" {
		t.Skip("set WEBFIG_PLAYWRIGHT=1 to run the browser check")
	}
	r := newWF(t)
	tls := httptest.NewTLSServer(r.srv.WebFigHandler())
	defer tls.Close()
	r.clientIP = "127.0.0.1" // the browser connects straight to the test listener
	tok := ticketOf(t, r.stepUp(t, r.br, r.owner, r.site))
	hostPort := "tech.example.co.ke:" + tls.Listener.Addr().String()[strings.LastIndex(tls.Listener.Addr().String(), ":")+1:]
	cmd := exec.Command("node", "testdata/webfig_check.js", "https://"+hostPort+"/_ftn/open?t="+tok, hostPort)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("browser check failed: %v", err)
	}
}
