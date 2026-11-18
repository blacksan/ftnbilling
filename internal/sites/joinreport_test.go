package sites

import (
	"strings"
	"testing"
	"time"
)

func TestReportWindow(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	tok, _, _ := f.m.MintJoin(s.ID)
	if err := f.m.SubmitJoinReport(tok, []byte("result ok\n")); err != ErrJoinInvalid {
		t.Fatalf("before the fetch: %v", err)
	}
	if _, _, err := f.m.ConsumeJoinStage(tok, "RouterOS 7.25.1", "1.2.3.4", false, StageJoin); err != nil {
		t.Fatal(err)
	}
	f.advance(29 * time.Minute)
	if err := f.m.SubmitJoinReport(tok, []byte("result ok ros 7.25.1\n")); err != nil {
		t.Fatalf("inside the window: %v", err)
	}
	if r, ok := f.m.JoinReport(s.ID); !ok || !r.OK || r.ROS != "7.25.1" || len(r.Lines) != 0 {
		t.Fatalf("report %+v", r)
	}
	f.advance(2 * time.Minute)
	if err := f.m.SubmitJoinReport(tok, []byte("result fail\n")); err != ErrJoinInvalid {
		t.Fatalf("after the window: %v", err)
	}
	if r, _ := f.m.JoinReport(s.ID); !r.OK {
		t.Fatal("a late report replaced the stored one")
	}
}

func TestReportSizeCapAndBadToken(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	tok, _, _ := f.m.MintJoin(s.ID)
	f.m.ConsumeJoinStage(tok, mt, "1.2.3.4", false, StageJoin)
	if err := f.m.SubmitJoinReport(tok, make([]byte, MaxReportBytes+1)); err != ErrReportTooLarge {
		t.Fatalf("%v", err)
	}
	if err := f.m.SubmitJoinReport(tok, []byte(strings.Repeat("a", MaxReportBytes))); err != nil {
		t.Fatalf("exactly the cap: %v", err)
	}
	// An oversize body with a wrong token must not reveal that the size was the problem.
	if err := f.m.SubmitJoinReport("wrong", make([]byte, MaxReportBytes+1)); err != ErrJoinInvalid {
		t.Fatalf("%v", err)
	}
	if err := f.m.SubmitJoinReport("", nil); err != ErrJoinInvalid {
		t.Fatal(err)
	}
}

func TestSanitizeReport(t *testing.T) {
	got := sanitizeReport([]byte("a\tb\r\nc\x00d\xffe password=hunter2 f\n"))
	if got != "a?b\nc?d?e password=[hidden] f\n" {
		t.Fatalf("%q", got)
	}
}

func TestROSVersionFromUA(t *testing.T) {
	for ua, want := range map[string]string{
		"RouterOS 7.25.1": "7.25.1", "RouterOS 7.13": "7.13", "RouterOS 7.20.1 (stable)": "7.20.1",
		"Mikrotik/6.49 Fetch": "", "curl/8": "", "": "", "routeros 7.9": "7.9",
	} {
		if got := ROSVersionFromUA(ua); got != want {
			t.Errorf("%q: %q want %q", ua, got, want)
		}
	}
	for v, want := range map[string]bool{"7.13": true, "7.25.1": true, "7.12.2": false, "7.9": false, "8.0": true, "6.49": false, "": false, "x.y": false} {
		if got := rosAtLeast(v, 7, 13); got != want {
			t.Errorf("rosAtLeast(%q) = %v", v, got)
		}
	}
}

// The script form follows the fetch User-Agent's version: 7.13+ gets :onerror
// with the router's error text, anything else the old form.
func TestSectionWrapperForms(t *testing.T) {
	cases := []struct {
		ua      string
		modern  bool
		stage   string
		cutover bool
	}{
		{"RouterOS 7.25.1", true, StageJoin, false},
		{"RouterOS 7.12", false, StageJoin, false},
		{"Mikrotik/6.49 Fetch", false, StageJoin, false},
		{"RouterOS 7.25.1", true, "", false},
		{"RouterOS 7.25.1", true, StageCutover, true},
		{"RouterOS 7.12", false, StageCutover, true},
	}
	for _, c := range cases {
		in := ipoeInput(c.stage)
		in.ROSVersion = ROSVersionFromUA(c.ua)
		in.Site.Provision = true
		out, err := RenderJoinScript(in)
		if err != nil {
			t.Fatal(c.ua, c.stage, err)
		}
		s := string(out)
		has := strings.Contains(s, ":onerror e in={")
		if has != c.modern {
			t.Errorf("%s %q: :onerror present=%v", c.ua, c.stage, has)
		}
		if c.modern {
			if strings.Contains(s, "no error text") || !strings.Contains(s, `$e`) || !strings.Contains(s, `:set report ($report . "`) {
				t.Errorf("%s %q: modern form lacks the error text", c.ua, c.stage)
			}
		} else if !strings.Contains(s, "no error text before RouterOS 7.13") {
			t.Errorf("%s %q: old form lacks its report line", c.ua, c.stage)
		}
		// Whatever the form, every section failure feeds $report, and a fatal one stops the script.
		if !strings.Contains(s, ":error $abort") || !strings.Contains(s, `:local report ""`) {
			t.Errorf("%s %q: no report or no abort", c.ua, c.stage)
		}
	}
}

func TestSectionHelpersExactText(t *testing.T) {
	m, o := sectionFuncs("7.25.1"), sectionFuncs("7.12")
	open := m["secOpen"].(func(int, string, bool) string)
	cl := m["secClose"].(func(int, string, bool) string)
	oopen := o["secOpen"].(func(int, string, bool) string)
	ocl := o["secClose"].(func(int, string, bool) string)
	if open(1, "x", true) != ":onerror e in={" || oopen(1, "x", true) != ":do {" {
		t.Fatal("open")
	}
	if got := cl(3, "REST API", true); got != `} do={ :set report ($report . "3 REST API: " . $e . "\n"); :set abort ("ftn: section 3 REST API failed: " . $e); :error $abort }` {
		t.Fatal(got)
	}
	if got := cl(4, "hotspot", false); got != `} do={ :set report ($report . "4 hotspot: " . $e . "\n"); :set fails ($fails + 1); :put ("ftn: section 4 hotspot failed, continuing: " . $e) }` {
		t.Fatal(got)
	}
	if got := ocl(3, "REST API", true); got != `} on-error={ :set report ($report . "3 REST API: failed (no error text before RouterOS 7.13)\n"); :set abort "ftn: section 3 REST API failed"; :error $abort }` {
		t.Fatal(got)
	}
}

// The pasted cutover fails fast, with the exact command, when device-mode
// blocks the scheduler.
func TestCutoverPrechecksDeviceMode(t *testing.T) {
	out, err := RenderJoinScript(ipoeInput(StageCutover))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	pre := strings.Index(s, `[/system/device-mode/get scheduler]`)
	first := strings.Index(s, "/system/script add")
	if pre < 0 || first < 0 || pre > first || !strings.Contains(s, "/system/device-mode/update scheduler=yes") {
		t.Fatal("the cutover must check the scheduler before it creates anything")
	}
}

func TestJoinReportAtBothExits(t *testing.T) {
	for _, stage := range []string{"", StageJoin} {
		in := ipoeInput(stage)
		out, err := RenderJoinScript(in)
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		if n := strings.Count(s, `/j/TOKEN-abc_123/report`); n != 2 {
			t.Errorf("stage %q: report posts = %d, want 2 (end and error handler)", stage, n)
		}
		h := strings.LastIndex(s, "} on-error={\n")
		if h < 0 || !strings.Contains(s[h:], "/report") {
			t.Errorf("stage %q: no report in the outer handler", stage)
		}
		if strings.Contains(s, "traffic-flow") {
			t.Errorf("stage %q: traffic flow must not be in the join script", stage)
		}
	}
}
