package web_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ispbill/internal/sites"
	"ispbill/internal/store"
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
	if r2, _ := m.joinPost(token, "result ok\n"); r2.StatusCode != 409 {
		t.Fatalf("second report: %d, want 409", r2.StatusCode)
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

// The router page and the API say whether a router was built with remote
// WinBox: only when its join report named the management subnet the server uses now.
func TestMgmtWinboxFollowsTheJoinReport(t *testing.T) {
	r := newAPIRig(t)
	// newSite makes a built router (it has a tunnel address). posted: the report goes through the real
	// endpoint (the join fetch and report are rate limited, so only one router does that); otherwise the
	// stored report text is written directly ("" for none).
	nth := 10 // every router has its own tunnel address (the database insists)
	newSite := func(name string) string {
		nth++
		site, err := r.mgr.CreateSite(sites.SiteInput{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.st.Update(func(d *store.Data) error {
			x := d.Sites[site.ID]
			x.TunnelIP = fmt.Sprintf("10.99.0.%d", nth)
			d.Sites[site.ID] = x
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return site.ID
	}
	seed := func(id, stored string) {
		if err := r.st.Update(func(d *store.Data) error {
			k := sites.JoinReportKey(id)
			d.Settings[k] = store.Setting{Key: k, Value: stored, UpdatedAt: time.Now()}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	apiFlag := func(id string) any {
		return r.call(r.view.Access, "GET", "/sites/"+id, nil, nil).JSON(t)["mgmt_winbox"]
	}
	const built = "Remote WinBox</dt><dd>Connect your management WireGuard profile, then open WinBox to"
	const notBuilt = "Built without remote WinBox: reset the router and paste a new line to add it."

	with := newSite("With")
	page := body(t, r.admin("POST", "/admin/sites/join", url.Values{"id": {with}}))
	rest := page[strings.Index(page, "/j/")+3:]
	token := rest[:strings.IndexAny(rest, "\"' <&?")]
	if x, _ := r.joinGet(token, "RouterOS 7.25.1"); x.StatusCode != 200 {
		t.Fatal("join fetch failed")
	}
	if x, _ := r.joinPost(token, "result ok ros 7.25.1 mgmt 10.98.0.0/24\n"); x.StatusCode != 204 {
		t.Fatalf("report: %d", x.StatusCode)
	}
	page = r.adminGet(t, "/admin/routers/view?id="+with+"&tab=overview")
	if !strings.Contains(page, built) || strings.Contains(page, notBuilt) || apiFlag(with) != true {
		t.Errorf("a router built with the current management subnet: api %v\n%s", apiFlag(with), page)
	}
	if rep, _ := r.mgr.JoinReport(with); rep.Mgmt != "10.98.0.0/24" || !rep.OK {
		t.Errorf("stored report: %+v", rep)
	}
	at := time.Now().UTC().Format(time.RFC3339)
	for name, stored := range map[string]string{
		"other subnet":   "at=" + at + " ok=true ros=7.25.1 mgmt=172.20.9.0/24\n", // built for a subnet that is not the current MGMT_SUBNET
		"before the fix": "at=" + at + " ok=true ros=7.25.1\n",                    // no mgmt field (or the script stopped before section 9)
		"no report":      "",
	} {
		id := newSite("Without " + name)
		if stored != "" {
			seed(id, stored)
		}
		page := r.adminGet(t, "/admin/routers/view?id="+id+"&tab=overview")
		if strings.Contains(page, built) || !strings.Contains(page, notBuilt) || apiFlag(id) != false {
			t.Errorf("%s: api %v\n%s", name, apiFlag(id), page)
		}
	}
	// a router that is not built yet (no tunnel address) shows neither line
	site, _ := r.mgr.CreateSite(sites.SiteInput{Name: "Blank"})
	if page := r.adminGet(t, "/admin/routers/view?id="+site.ID+"&tab=overview"); strings.Contains(page, "Remote WinBox") {
		t.Error("a router with no tunnel address shows a Remote WinBox line")
	}
	// the list carries the flag too
	items := r.call(r.view.Access, "GET", "/sites", nil, nil).JSON(t)["items"].([]any)
	for _, it := range items {
		if _, ok := it.(map[string]any)["mgmt_winbox"].(bool); !ok {
			t.Errorf("the list lacks a boolean mgmt_winbox: %v", it)
		}
	}
}
