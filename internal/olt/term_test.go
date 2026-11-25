package olt

import (
	"ispbill/internal/store"
	"strings"
	"testing"
)

func TestRenderCursorMovement(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "a\r\nb\r\n", "a\nb\n"},
		{"forward pads", "GPON0/1:1\x1b[2Cenable\x1b[2Cenable\x1b[2Cworking\x1b[2CVSOL00127e0f\r\n",
			"GPON0/1:1  enable  enable  working  VSOL00127e0f\n"},
		{"forward default 1", "a\x1b[Cb\n", "a b\n"},
		{"back overwrites", "abcdef\x1b[3DXY\n", "abcXYf\n"},
		{"absolute G", "ab\x1b[10Gc\n", "ab       c\n"},
		{"absolute backtick", "abc\x1b[2`X\n", "aXc\n"},
		{"cr overwrite", "GPON0/\rVSOL\n", "VSOL0/\n"},
		{"cr then erase to end", "--More--\r\x1b[K", ""},
		{"cr blank cr", "--More--\r        \rrow\n", "row\n"},
		{"erase line", "junk\x1b[2Kok\n", "    ok\n"},
		{"erase start", "abcdef\x1b[3G\x1b[1K!\n", "  !def\n"},
		{"erase n", "abcdef\x1b[2G\x1b[2Xz\n", "az def\n"},
		{"delete n", "abcdef\x1b[2G\x1b[2P\n", "adef\n"},
		{"backspace overwrite", "ab\b\bxy\n", "xy\n"},
		{"bs space bs erases", "abc\b \b\n", "ab\n"},
		{"sgr ignored", "\x1b[1;32mok\x1b[0m\n", "ok\n"},
		{"other csi ignored", "\x1b[?25l\x1b[2Jok\n", "ok\n"},
		{"charset and keypad", "\x1b(Bok\x1b=\x1b>\n", "ok\n"},
		{"cut escape", "ok\x1b[1", "ok"},
		{"nul dropped", "a\x00b\n", "ab\n"},
		{"insert", "abcd\x1b[2G\x1b[2@X\n", "aX bcd\n"},
	}
	for _, c := range cases {
		if got := render([]byte(c.in)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRenderPromptKeepsTrailingBlank(t *testing.T) {
	if got := render([]byte("x\r\ngpon-olt# ")); got != "x\ngpon-olt# " {
		t.Errorf("%q", got)
	}
}

func TestRenderBounded(t *testing.T) {
	got := render([]byte("a\x1b[999999999Cb\x1b[999999999Cc\n" + strings.Repeat("z", 100000)))
	for _, l := range strings.Split(got, "\n") {
		if len(l) > maxLineCols {
			t.Fatalf("line of %d", len(l))
		}
	}
}

func TestVisible(t *testing.T) {
	got := visible([]byte("a\x1b[12C\rb\b\x01\r\nc"), 100)
	if want := "a\\x1b[12C\\rb\\b\\x01\\r\nc"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
	if v := visible([]byte(strings.Repeat("x", 50)), 10); !strings.HasSuffix(v, "...") || len(v) > 14 {
		t.Errorf("not capped: %q", v)
	}
}

func TestParseONUStateTolerant(t *testing.T) {
	out := "OnuIndex  Admin State  OMCC State  Phase State  Serial\n" +
		"GPON0/1:1   enable   enable   working   VSOL00127e0f\n" +
		"1:2   enable   enable   los   VSOL00127e10\n" +
		"1:3 enable enable DyingGasp\n" +
		"total: 3 working: 1\n"
	rows, bad := ParseONUState(0, out)
	if bad != "" {
		t.Fatalf("unparsed %q", bad)
	}
	if len(rows) != 3 {
		t.Fatalf("%+v", rows)
	}
	if r := rows[0]; r.PON != 1 || r.ID != 1 || r.State != "online" || !r.Online || r.Serial != "VSOL00127E0F" {
		t.Errorf("row 0: %+v", r)
	}
	if r := rows[1]; r.PON != 1 || r.ID != 2 || r.State != "los" || r.Online || r.Serial != "VSOL00127E10" {
		t.Errorf("row 1: %+v", r)
	}
	if r := rows[2]; r.ID != 3 || r.Online || r.State != "dying-gasp" {
		t.Errorf("row 2: %+v", r)
	}
	if _, bad := ParseONUState(1, "something odd here"); bad != "something odd here" {
		t.Errorf("odd line not kept: %q", bad)
	}
}

func TestMergeDistanceLayouts(t *testing.T) {
	for name, out := range map[string]string{
		"prefixed":  "GPON0/1:1   123 m\n",
		"bare":      "1:1  123\n",
		"unit glue": "1:1  123(m)\n",
		"meters":    "GPON0/1:1  123 meters\n",
		"kv":        "ONU ID  : 1\nDistance : 123 m\n",
		"kv index":  "ONU : GPON0/1:1\nRanging distance : 123(m)\n",
	} {
		rows := []store.OLTONU{{PON: 1, ID: 1}, {PON: 1, ID: 2}}
		MergeDistance(rows, 1, out)
		if rows[0].Distance != "123" || rows[1].Distance != "" {
			t.Errorf("%s: %+v", name, rows)
		}
	}
}

func TestAutoFindBareIndex(t *testing.T) {
	f := ParseAutoFindOn(0, "1:--   VSOL00127e0f   unknown\n")
	if len(f) != 1 || f[0].PON != 1 || f[0].Serial != "VSOL00127E0F" {
		t.Errorf("%+v", f)
	}
}

func TestRenderMalformedEscapeKeepsLineFeed(t *testing.T) {
	for in, want := range map[string]string{
		"a\x1b[\nb\n":                    "a\nb\n",
		"a\x1b[12\nb\n":                  "a\nb\n",
		"a\x1b\nb\n":                     "a\nb\n",
		"a\x1b(\nb\n":                    "a\nb\n",
		"a\x1b[-1Cb\n":                   "a1Cb\n", // "-" is no parameter byte: the sequence ends there, the rest is text
		"a\x1b[99999999999999999999Cb\n": "a b\n",
	} {
		if got := render([]byte(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestBlankMoreThenShortRedraw(t *testing.T) {
	re := VSOL().MoreRE
	raw := []byte("row1\r\n--More--")
	blankMore(raw, re)
	raw = append(raw, "\rab\r\nrow3\r\n"...)
	if got := render(raw); got != "row1\nab\nrow3\n" {
		t.Errorf("%q", got)
	}
	// Backspaces over the marker still line up (same length).
	raw = []byte("x\r\n -- More -- ")
	blankMore(raw, re)
	raw = append(raw, strings.Repeat("\b", 14)+"y\n"...)
	if got := render(raw); got != "x\ny\n" {
		t.Errorf("%q", got)
	}
}

func TestDebugCopyRedactsWhatTheRenderedLineHides(t *testing.T) {
	raw := []byte("ONU\x1b[2C1\r\n\x1b[4Cpassword\x1b[2C:\x1b[1Ctopsecret99\r\nok\x1b[3Cdone")
	got := debugCopy(raw, 4000)
	if strings.Contains(got, "topsecret99") {
		t.Fatalf("secret in the debug copy: %q", got)
	}
	if !strings.Contains(got, `ONU\x1b[2C1\r`) || !strings.Contains(got, `ok\x1b[3Cdone`) {
		t.Errorf("other lines not kept visible: %q", got)
	}
}

func TestParseONUStateStateNotLast(t *testing.T) {
	rows, bad := ParseONUState(1, "1:1  enable  enable  Dying Gasp  VSOL00127e0f\n1:2  enable  enable  working  VSOL00127e10  2025-01-01\n")
	if bad != "" || len(rows) != 2 {
		t.Fatalf("%q %+v", bad, rows)
	}
	if rows[0].State != "dying-gasp" || rows[0].Online {
		t.Errorf("row 0: %+v", rows[0])
	}
	if rows[1].State != "online" || !rows[1].Online {
		t.Errorf("row 1: %+v", rows[1])
	}
}
