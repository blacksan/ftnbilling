package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The design tokens in static/app.css must stay readable: WCAG 2.x contrast
// of 4.5 for text and 3 for interface parts and charts, in light and dark.

func hexLum(t *testing.T, h string) float64 {
	t.Helper()
	h = strings.TrimPrefix(h, "#")
	if len(h) != 6 {
		t.Fatalf("bad colour %q", h)
	}
	ch := func(s string) float64 {
		v, err := strconv.ParseUint(s, 16, 8)
		if err != nil {
			t.Fatalf("bad colour %q", h)
		}
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(h[0:2]) + 0.7152*ch(h[2:4]) + 0.0722*ch(h[4:6])
}

func ratio(t *testing.T, a, b string) float64 {
	la, lb := hexLum(t, a), hexLum(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

var tokRe = regexp.MustCompile(`--([a-z0-9-]+):(#[0-9A-Fa-f]{6})\b`)

// block returns the text from the first occurrence of start to its closing brace.
func block(t *testing.T, css, start string) string {
	t.Helper()
	i := strings.Index(css, start)
	if i < 0 {
		t.Fatalf("css has no %q block", start)
	}
	depth, j := 0, i+len(start)
	for ; j < len(css); j++ {
		switch css[j] {
		case '{':
			depth++
		case '}':
			if depth == 0 {
				return css[i:j]
			}
			depth--
		}
	}
	t.Fatalf("unterminated %q block", start)
	return ""
}

func tokens(t *testing.T, blk string) map[string]string {
	m := map[string]string{}
	for _, x := range tokRe.FindAllStringSubmatch(blk, -1) {
		m[x[1]] = strings.ToUpper(x[2])
	}
	return m
}

func loadThemes(t *testing.T) (light, dark, darkSys map[string]string) {
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	light = tokens(t, block(t, css, ":root{\n  color-scheme:light;"))
	darkSys = tokens(t, block(t, css, "  :root:not([data-theme=light]){"))
	dark = tokens(t, block(t, css, ":root[data-theme=dark]{"))
	return
}

func TestDarkTokensAgreeBetweenSystemAndChosen(t *testing.T) {
	_, dark, sys := loadThemes(t)
	if len(dark) < 30 {
		t.Fatalf("dark theme has only %d tokens", len(dark))
	}
	for k, v := range dark {
		if sys[k] != v {
			t.Errorf("--%s: chosen dark %s, system dark %s", k, v, sys[k])
		}
	}
	for k := range sys {
		if _, ok := dark[k]; !ok {
			t.Errorf("--%s only in the system dark block", k)
		}
	}
}

func TestDesignTokensContrast(t *testing.T) {
	light, dark, _ := loadThemes(t)
	for _, th := range []struct {
		name string
		tok  map[string]string
	}{{"light", light}, {"dark", dark}} {
		tok := th.tok
		for _, k := range []string{"bg", "surface", "surface-2", "surface-3", "ink", "ink-2", "muted", "brand", "on-brand", "brand-ink", "brand-soft",
			"side", "on-side", "side-mut", "ok", "ok-soft", "warn", "warn-soft", "warn-fill", "bad", "bad-soft", "info", "info-soft",
			"field", "focus", "line", "line-strong", "chart-1", "chart-2", "chart-3", "chart-4", "chart-grid",
			"brand-50", "brand-100", "brand-200", "brand-300", "brand-400", "brand-500", "brand-600", "brand-700", "brand-800", "brand-900"} {
			if tok[k] == "" {
				t.Fatalf("%s: token --%s missing", th.name, k)
			}
		}
		for _, gone := range []string{"teal", "plum", "feat-bg"} {
			if tok[gone] != "" {
				t.Errorf("%s: --%s was dropped from the design and must not come back", th.name, gone)
			}
		}
		text := func(fg, bg string) {
			t.Helper()
			if r := ratio(t, tok[fg], tok[bg]); r < 4.5 {
				t.Errorf("%s: text --%s on --%s is %.2f:1, need 4.5", th.name, fg, bg, r)
			}
		}
		ui := func(fg, bg string) {
			t.Helper()
			if r := ratio(t, tok[fg], tok[bg]); r < 3 {
				t.Errorf("%s: --%s against --%s is %.2f:1, need 3", th.name, fg, bg, r)
			}
		}
		for _, bg := range []string{"bg", "surface", "surface-2", "surface-3"} {
			text("ink", bg)
			text("ink-2", bg)
			text("muted", bg)
			text("brand-ink", bg)
		}
		text("on-brand", "brand")
		text("brand-ink", "brand-soft")
		text("ink", "brand-soft")
		text("on-side", "side")
		text("side-mut", "side")
		text("bg", "ink") // inverted: toasts, selected tab (light)
		for _, tone := range []string{"ok", "warn", "bad", "info"} {
			text(tone, tone+"-soft")
			text(tone, "surface")
			text(tone, "bg")
			text("ink", tone+"-soft")
			text("ink-2", tone+"-soft")
		}
		// warning is a yellow fill with dark text and an icon, never orange
		if r := ratio(t, "#0F172A", tok["warn-fill"]); r < 4.5 {
			t.Errorf("%s: dark text on --warn-fill is %.2f:1, need 4.5", th.name, r)
		}
		text("bg", "bad") // danger buttons
		// interface parts: input borders, focus ring, chart marks, feature accents
		for _, bg := range []string{"bg", "surface", "surface-2"} {
			ui("field", bg)
			ui("line-strong", bg) // interactive borders: buttons, inputs, tabs
			ui("focus", bg)
			ui("brand", bg)
		}
		// gridlines are decoration and exempt; marks are not
		for _, c := range []string{"chart-1", "chart-2", "chart-3", "chart-4"} {
			ui(c, "surface")
		}
		ui("brand", "side")
		ui("on-brand", "brand")
	}
}

func TestCSSHasNoExternalURLs(t *testing.T) {
	b, _ := staticFS.ReadFile("static/app.css")
	for _, m := range regexp.MustCompile(`url\(\s*['"]?([^)'"]+)`).FindAllStringSubmatch(string(b), -1) {
		u := m[1]
		if strings.Contains(u, "://") || strings.HasPrefix(u, "//") {
			t.Errorf("app.css loads %q", u)
		}
	}
}
