package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"ispbill/internal/olt"
	"ispbill/internal/store"
)

// Apply and Check now are background jobs sharing one slot per OLT with Secure: the request checks the password and the plan
// hash, answers at once (202 / a redirect with a flash), and the page and the owner-only status endpoint show the progress.
func TestOLTApplyJobPagesAPIAndSharedSlot(t *testing.T) {
	o := newOLTRig(t)
	o.f.SetRunning("")
	o.f.WriteBuilding, o.f.WriteDelay = true, 1500*time.Millisecond // a slow save keeps the job running for a while
	p := "/olts/" + o.id
	if g := o.call(o.owner.Access, "PUT", p+"/provisioning", map[string]any{"enabled": true, "dialect_checked": true, "password": o.ownerW.Pass}, nil); g.Status != 200 {
		t.Fatalf("provisioning: %d %s", g.Status, g.Raw)
	}
	pl := o.call(o.owner.Access, "POST", p+"/plan", nil, nil)
	hash, _ := pl.JSON(t)["hash"].(string)
	if pl.Status != 200 || hash == "" {
		t.Fatalf("plan: %d %s", pl.Status, pl.Raw)
	}
	cl := &http.Client{CheckRedirect: noRedirect}
	ob := signIn(t, o.ts, o.srv, o.st, o.ownerW)
	pb := signIn(t, o.ts, o.srv, o.st, o.operW)
	page := func(b *browser, path string) string {
		t.Helper()
		resp := b.Do(cl, o.ts.URL, "GET", path, nil)
		bs, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		return string(bs)
	}
	manage := "/admin/olts/manage?id=" + url.QueryEscape(o.id)

	// The web form: a wrong password starts nothing; the right one redirects at once with a "started" flash.
	form := func(pass string) *http.Response {
		resp := ob.Do(cl, o.ts.URL, "POST", "/admin/olts/manage/apply", url.Values{"id": {o.id}, "hash": {hash}, "current_password": {pass}})
		resp.Body.Close()
		return resp
	}
	form("wrong-password-xx")
	if v := o.svc2.JobStatus(o.id); v.Known {
		t.Fatal("a wrong password started a job")
	}
	t0 := time.Now()
	resp := form(o.ownerW.Pass)
	if d := time.Since(t0); d > time.Second || resp.StatusCode != 303 || !strings.Contains(resp.Header.Get("Location"), "Applying+started") {
		t.Fatalf("apply form: %d %s after %v", resp.StatusCode, resp.Header.Get("Location"), d)
	}

	// While it runs: the owner's page polls (no inline script), the operator's page reloads by itself, neither shows a secret.
	op := page(ob, manage)
	if !strings.Contains(op, "<b>Applying&hellip;</b>") || !strings.Contains(op, `data-job-poll="/admin/olts/manage/job-status?id=`+o.id) || !strings.Contains(op, `data-running="1"`) {
		t.Errorf("owner page while applying lacks the progress panel")
	}
	if hasInlineScript(op) || inlineHandler.MatchString(op) {
		t.Error("inline script on the page")
	}
	pp := page(pb, manage)
	if !strings.Contains(pp, "<b>Applying&hellip;</b>") || strings.Contains(pp, "data-job-poll") || !strings.Contains(pp, `data-reload="5"`) {
		t.Errorf("operator page while applying: no panel, or it polls the owner-only endpoint")
	}
	for _, pg := range []string{op, pp} {
		for _, secret := range []string{oltACSPass, oltACSUser, o.f.Pass} {
			if strings.Contains(pg, secret) {
				t.Errorf("the page shows %q", secret)
			}
		}
	}

	// The status: owner only, never cached, kind and stage, no secret.
	st := o.call(o.owner.Access, "GET", p+"/job", nil, nil)
	j := st.JSON(t)
	if st.Status != 200 || j["running"] != true || j["kind"] != "apply" || j["label"] != "Applying" || j["stage"] == "" || !strings.Contains(st.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("status while running: %d %s", st.Status, st.Raw)
	}
	if strings.Contains(string(st.Raw), oltACSPass) || strings.Contains(string(st.Raw), oltACSUser) {
		t.Fatalf("secret in the status: %s", st.Raw)
	}
	for _, tok := range []string{o.oper.Access, o.view.Access} {
		if g := o.call(tok, "GET", p+"/job", nil, nil); g.Status != 403 {
			t.Errorf("job status by a non-owner: %d", g.Status)
		}
	}
	sp := "/admin/olts/manage/job-status?id=" + url.QueryEscape(o.id)
	resp = pb.Do(cl, o.ts.URL, "GET", sp, nil)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Error("an operator reads the owner's status page")
	}
	resp = ob.Do(cl, o.ts.URL, "GET", sp, nil)
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(bs), `"kind":"apply"`) || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Errorf("owner status page: %d %s", resp.StatusCode, bs)
	}

	// One job per OLT: every other start is refused, politely (a flash), and by the API with 409.
	if g := o.call(o.owner.Access, "POST", p+"/apply", map[string]any{"hash": hash, "password": o.ownerW.Pass}, nil); g.Status != 409 || !strings.Contains(string(g.Raw), "Applying is already running") {
		t.Errorf("second apply: %d %s", g.Status, g.Raw)
	}
	if g := o.call(o.oper.Access, "POST", p+"/adoption/check", nil, nil); g.Status != 409 || !strings.Contains(string(g.Raw), "Applying is already running") {
		t.Errorf("check during apply: %d %s", g.Status, g.Raw)
	}
	if g := o.call(o.owner.Access, "POST", p+"/secure", map[string]any{"password": o.ownerW.Pass}, nil); g.Status != 409 {
		t.Errorf("secure during apply: %d %s", g.Status, g.Raw)
	}
	resp = pb.Do(cl, o.ts.URL, "POST", "/admin/olts/manage/adopt-check", url.Values{"id": {o.id}})
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != 303 || !strings.Contains(loc, "Applying+is+already+running") {
		t.Errorf("web check during apply: %d %s", resp.StatusCode, loc)
	}

	// It ends: the status says so, the page loses the polling and shows the outcome.
	var done reply
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		if done = o.call(o.owner.Access, "GET", p+"/job", nil, nil); done.JSON(t)["done"] == true {
			break
		}
	}
	if d := done.JSON(t); d["ok"] != true || d["verified"] != true || d["running"] != false || !strings.Contains(d["message"].(string), "Applied ") {
		t.Fatalf("finished: %s", done.Raw)
	}
	fin := page(ob, manage)
	if strings.Contains(fin, "data-running") || strings.Contains(fin, "data-job-poll") || !strings.Contains(fin, "Applied ") {
		t.Errorf("finished page keeps polling or lacks the outcome")
	}
	for _, secret := range []string{oltACSPass, oltACSUser, o.f.Pass} {
		if strings.Contains(fin+string(done.Raw)+o.auditText()+o.alertText(), secret) {
			t.Errorf("%q leaked after the apply", secret)
		}
	}
	// The slot is free: a second apply with the old hash now says the plan changed (it was applied), not "running".
	if g := o.call(o.owner.Access, "POST", p+"/apply", map[string]any{"hash": hash, "password": o.ownerW.Pass}, nil); g.Status != 409 || strings.Contains(string(g.Raw), "already running") {
		t.Errorf("apply of a finished plan: %d %s", g.Status, g.Raw)
	}
}

// An apply the server did not finish (it stopped) is reported as unfinished on the page and in the status.
func TestOLTInterruptedApplyIsReported(t *testing.T) {
	o := newOLTRig(t)
	o.f.SetRunning("")
	if err := o.svc2.SetDialectChecked(o.id, true, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := o.svc2.SetProvisioning(o.id, true, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := o.st.Update(func(d *store.Data) error {
		s := d.OLTStateOf(o.id)
		s.Settings.Job.Kind, s.Settings.Job.By, s.Settings.Job.At = "apply", "olga", time.Now()
		d.OLTStates[o.id] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	g := o.call(o.owner.Access, "GET", "/olts/"+o.id+"/job", nil, nil)
	j := g.JSON(t)
	if j["interrupted"] != true || j["interrupted_kind"] != "apply" || !strings.Contains(j["interrupted_note"].(string), "nothing is applied twice") {
		t.Fatalf("%s", g.Raw)
	}
	ob := signIn(t, o.ts, o.srv, o.st, o.ownerW)
	resp := ob.Do(&http.Client{CheckRedirect: noRedirect}, o.ts.URL, "GET", "/admin/olts/manage?id="+url.QueryEscape(o.id), nil)
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(bs), "unfinished") || !strings.Contains(string(bs), "nothing is applied twice") || strings.Contains(string(bs), "data-running") {
		t.Errorf("page does not report the unfinished apply")
	}
	// Reading the plan again clears it.
	if pl := o.call(o.owner.Access, "POST", "/olts/"+o.id+"/plan", nil, nil); pl.Status != 200 {
		t.Fatalf("%d %s", pl.Status, pl.Raw)
	}
	if j := o.call(o.owner.Access, "GET", "/olts/"+o.id+"/job", nil, nil).JSON(t); j["interrupted"] == true {
		t.Fatal("still reported after the plan was read")
	}
}

// The script polls only while a job runs, stops on any 4xx and when the job ends, and uses no inline code.
func TestOLTJobPollScript(t *testing.T) {
	b, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	i := strings.Index(js, `$("[data-job-poll]")`)
	if i < 0 {
		t.Fatal("the job poll is not in app.js")
	}
	blk := js[i:min(len(js), i+1400)]
	for _, want := range []string{`getAttribute("data-running")`, `j.__status >= 400 && j.__status < 500`, "clearInterval(spTimer); return", "if (j.running)", "clearInterval(spTimer); location.reload()", "doc.hidden"} {
		if !strings.Contains(blk, want) {
			t.Errorf("poll script lacks %q", want)
		}
	}
	if strings.Contains(js, "data-secure-poll") {
		t.Error("the old secure-only poll attribute is still in app.js")
	}
}

// Removing an ONU and "Save and check" of a typed login no longer hold the request: the removal is an Apply job, the check
// after the save is a Check job. A login cannot be typed over a running job. The API answers 202 with Location.
func TestOLTRemoveAndCredentialsUseTheJobSlot(t *testing.T) {
	o := newOLTRig(t)
	o.f.SetRunning("vlan 100\n" + "interface gpon 0/1\n  onu add 3 profile default sn VSOL00000001\n")
	o.f.WriteBuilding, o.f.WriteDelay = true, 1500*time.Millisecond
	p := "/olts/" + o.id
	if g := o.call(o.owner.Access, "PUT", p+"/provisioning", map[string]any{"enabled": true, "dialect_checked": true, "password": o.ownerW.Pass}, nil); g.Status != 200 {
		t.Fatalf("provisioning: %d %s", g.Status, g.Raw)
	}
	_, hash, err := o.svc2.PlanRemove(context.Background(), o.id, "VSOL00000001")
	if err != nil {
		t.Fatal(err)
	}
	cl := &http.Client{CheckRedirect: noRedirect}
	ob := signIn(t, o.ts, o.srv, o.st, o.ownerW)
	post := func(path string, v url.Values) string {
		t.Helper()
		v.Set("id", o.id)
		resp := ob.Do(cl, o.ts.URL, "POST", path, v)
		resp.Body.Close()
		if resp.StatusCode != 303 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		loc, _ := url.QueryUnescape(resp.Header.Get("Location"))
		return loc
	}
	t0 := time.Now()
	if loc := post("/admin/olts/manage/remove-apply", url.Values{"serial": {"VSOL00000001"}, "hash": {hash}, "current_password": {o.ownerW.Pass}}); !strings.Contains(loc, "Removing started") || time.Since(t0) > time.Second {
		t.Fatalf("remove: %s after %v", loc, time.Since(t0))
	}
	if v := o.svc2.JobStatus(o.id); !v.Running || v.Kind != olt.JobApply {
		t.Fatalf("%+v", v)
	}
	// While it runs: a typed login is refused (web and API), and the API apply answers 409.
	var before string
	o.st.View(func(d *store.Data) { before = d.Equipment[o.id].MgmtPass })
	if loc := post("/admin/olts/manage/credentials", url.Values{"user": {"admin"}, "pass": {"typed-over-a-job"}, "current_password": {o.ownerW.Pass}}); !strings.Contains(loc, "not saved") {
		t.Errorf("credentials during a job: %s", loc)
	}
	if g := o.call(o.owner.Access, "PUT", p+"/credentials", map[string]any{"password": o.ownerW.Pass, "user": "admin", "pass": "typed-over-a-job"}, nil); g.Status != 409 {
		t.Errorf("API credentials during a job: %d %s", g.Status, g.Raw)
	}
	var after string
	o.st.View(func(d *store.Data) { after = d.Equipment[o.id].MgmtPass })
	if after != before {
		t.Fatal("the login was changed while a job ran")
	}
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end) && o.svc2.JobStatus(o.id).Running; time.Sleep(25 * time.Millisecond) {
	}
	if v := o.svc2.JobStatus(o.id); !v.OK || !v.Apply.Verified {
		t.Fatalf("removal: %+v", v)
	}
	if !strings.Contains(strings.Join(o.f.Lines(), "\n"), "no onu 3") {
		t.Fatal("not removed")
	}
	// The API apply: 202, the status URL in Location and in the body.
	pl := o.call(o.owner.Access, "POST", p+"/plan", nil, nil)
	h2, _ := pl.JSON(t)["hash"].(string)
	g := o.call(o.owner.Access, "POST", p+"/apply", map[string]any{"hash": h2, "password": o.ownerW.Pass}, nil)
	if g.Status != 202 || g.Header.Get("Location") != "/api/v1"+p+"/job" || g.JSON(t)["status_url"] != "/api/v1"+p+"/job" {
		t.Fatalf("API apply: %d %v %s", g.Status, g.Header, g.Raw)
	}
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end) && o.svc2.JobStatus(o.id).Running; time.Sleep(25 * time.Millisecond) {
	}
	// After the job: the typed login is saved (the OLT's own, so the rig keeps working) and the check is not run inline.
	if loc := post("/admin/olts/manage/credentials", url.Values{"user": {o.f.User}, "pass": {o.f.Pass}, "current_password": {o.ownerW.Pass}}); !strings.Contains(loc, "msg=Login saved") {
		t.Errorf("credentials: %s", loc)
	}
}
