package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"ispbill/internal/sites"
)

func (m *multi) joinPost(token, body string) (*http.Response, string) {
	req, _ := http.NewRequest("POST", m.ts.URL+"/j/"+token+"/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	r, err := m.cl.Do(req)
	if err != nil {
		panic(err)
	}
	return r, body_(r)
}

func body_(r *http.Response) string {
	defer r.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}

func TestJoinReportEndpoint(t *testing.T) {
	m := newMulti(t)
	site, err := m.mgr.CreateSite(sites.SiteInput{Name: "Zeta"})
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, m.admin("POST", "/admin/sites/join", url.Values{"id": {site.ID}}))
	i := strings.Index(page, "/j/")
	rest := page[i+3:]
	token := rest[:strings.IndexAny(rest, "\"' <&?")]

	// Before the script was fetched the token is not used yet: same 404 as a wrong one.
	_, wrongBody := m.joinPost("nope-nope-nope-nope-nope-nope-nope-nope-nope", "result ok\n")
	r0, b0 := m.joinPost(token, "result ok\n")
	if r0.StatusCode != 404 || b0 != wrongBody {
		t.Fatalf("report before the script was fetched: %d %q", r0.StatusCode, b0)
	}
	if r, _ := m.joinGet(token, "RouterOS 7.25.1"); r.StatusCode != 200 {
		t.Fatal("join fetch failed")
	}

	// A wrong token looks exactly like a bad join link.
	rw, bw := m.joinPost("nope-nope-nope-nope-nope-nope-nope-nope-nope", "result ok\n")
	_, bj := m.joinGet("nope-nope-nope-nope-nope-nope-nope-nope-nope", "RouterOS 7.20")
	if rw.StatusCode != 404 || bw != bj {
		t.Fatalf("wrong token: %d %q vs GET %q", rw.StatusCode, bw, bj)
	}
	// Too large.
	if r, _ := m.joinPost(token, "result fail\n"+strings.Repeat("x", 2100)); r.StatusCode != 413 {
		t.Fatalf("oversize report: %d", r.StatusCode)
	}
	if _, ok := m.mgr.JoinReport(site.ID); ok {
		t.Fatal("a refused report was stored")
	}
	// A good one: secrets masked, control bytes replaced.
	r, _ := m.joinPost(token, "result fail ros 7.25.1\n7 cutover kit: not allowed by device-mode\n1 WireGuard: bad private-key=ABCSECRET123 here\x01\n")
	if r.StatusCode != 204 {
		t.Fatalf("report: %d", r.StatusCode)
	}
	rep, ok := m.mgr.JoinReport(site.ID)
	if !ok || rep.OK || rep.ROS != "7.25.1" || len(rep.Lines) != 2 {
		t.Fatalf("stored report %+v %v", rep, ok)
	}
	all := strings.Join(rep.Lines, "\n")
	if strings.Contains(all, "ABCSECRET123") || !strings.Contains(all, "[hidden]") || !strings.Contains(all, "device-mode") || !strings.Contains(all, "here?") {
		t.Fatalf("report not redacted or sanitized: %q", all)
	}
	// The token never reaches the log.
	if strings.Contains(m.logs.String(), token) {
		t.Fatal("token in the log")
	}
}

func TestRefusedUserAgentShowsInStatus(t *testing.T) {
	m := newMulti(t)
	site, _ := m.mgr.CreateSite(sites.SiteInput{Name: "Eta"})
	page := body(t, m.admin("POST", "/admin/sites/join", url.Values{"id": {site.ID}}))
	i := strings.Index(page, "/j/")
	rest := page[i+3:]
	token := rest[:strings.IndexAny(rest, "\"' <&?")]
	if r, _ := m.joinGet(token, "curl/8.5.0"); r.StatusCode != 404 {
		t.Fatal("curl must be refused")
	}
	st, _ := m.mgr.Status(t.Context(), site.ID)
	if st.RefusedUA != "curl/8.5.0" || st.RefusedAt.IsZero() || st.Token != sites.TokenPending {
		t.Fatalf("status %+v", st)
	}
	// An unknown token leaves no trace on any building.
	m.joinGet("nope-nope-nope-nope-nope-nope-nope-nope-nope", "Wget/1")
	m.joinGet(token, "RouterOS 7.25.1")
	st, _ = m.mgr.Status(t.Context(), site.ID)
	if st.RefusedUA != "" {
		t.Fatalf("a used token still shows a refusal: %+v", st)
	}
}
