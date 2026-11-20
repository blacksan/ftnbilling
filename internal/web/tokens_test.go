package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The design tokens in static/tokens.css must stay readable: WCAG 2.x contrast
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
	b, err := staticFS.ReadFile("static/tokens.css")
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
		for _, k := range []string{"bg", "surface", "surface-2", "surface-3", "ink", "ink-2", "muted", "brand", "on-brand", "brand-ink", "brand-soft", "brand-hi",
			"side", "on-side", "side-mut", "side-accent", "ok", "ok-soft", "warn", "warn-soft", "warn-fill", "on-warn-fill", "bad", "bad-soft", "info", "info-soft",
			"field", "focus", "line", "line-strong", "chart-1", "chart-2", "chart-3", "chart-4", "chart-grid"} {
			if tok[k] == "" {
				t.Fatalf("%s: token --%s missing", th.name, k)
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
		text("on-brand", "brand") // primary buttons, avatar
		text("brand-ink", "brand-soft")
		text("ink", "brand-soft")
		text("muted", "brand-soft")
		text("on-side", "side")
		text("side-mut", "side")
		text("on-warn-fill", "warn-fill") // warning icon chip
		text("bg", "bad")                 // danger buttons
		text("bg", "ink")                 // inverted: toasts
		// status colours read on every surface and on their soft chip
		for _, tone := range []string{"ok", "warn", "bad", "info"} {
			text(tone, tone+"-soft")
			text(tone, "surface")
			text(tone, "surface-2")
			text("ink", tone+"-soft")
			text("ink-2", tone+"-soft")
		}
		// interface parts: input borders, focus ring, button outlines, chart marks, sidebar marker
		for _, bg := range []string{"bg", "surface"} {
			ui("field", bg)
			ui("focus", bg)
			ui("line-strong", bg)
			ui("brand", bg)
		}
		ui("field", "surface-2")
		ui("side-accent", "side")
		// --warn-fill (yellow) is never used alone: it is a dot beside a text label, or a chip with dark text on it
		// gridlines and hairlines are decoration and exempt; marks are not
		for _, c := range []string{"chart-1", "chart-2", "chart-3", "chart-4"} {
			ui(c, "surface")
		}
		// the semantic green must stay clearly apart from the brand teal
		if d := hueDistance(t, tok["ok"], tok["brand"]); d < 25 {
			t.Errorf("%s: success and brand hues are only %.0f degrees apart", th.name, d)
		}
	}
}

func hue(t *testing.T, h string) float64 {
	t.Helper()
	h = strings.TrimPrefix(h, "#")
	v := func(i int) float64 {
		x, _ := strconv.ParseUint(h[i:i+2], 16, 8)
		return float64(x) / 255
	}
	r, g, b := v(0), v(2), v(4)
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	if mx == mn {
		return 0
	}
	d := mx - mn
	var hh float64
	switch mx {
	case r:
		hh = math.Mod((g-b)/d, 6)
	case g:
		hh = (b-r)/d + 2
	default:
		hh = (r-g)/d + 4
	}
	hh *= 60
	if hh < 0 {
		hh += 360
	}
	return hh
}

func hueDistance(t *testing.T, a, b string) float64 {
	d := math.Abs(hue(t, a) - hue(t, b))
	if d > 180 {
		d = 360 - d
	}
	return d
}

// No orange anywhere: the palette has no hue between red-orange and amber
// except the yellow warning fill, which sits outside that band.
func TestNoOrangeTokens(t *testing.T) {
	light, dark, _ := loadThemes(t)
	for name, tok := range map[string]map[string]string{"light": light, "dark": dark} {
		for k, v := range tok {
			h := hue(t, v)
			if h > 18 && h < 44 && saturation(v) > 0.45 && brightness(v) > 0.6 {
				t.Errorf("%s: --%s %s is orange (hue %.0f)", name, k, v, h)
			}
		}
	}
}

func brightness(h string) float64 {
	h = strings.TrimPrefix(h, "#")
	m := 0.0
	for i := 0; i < 6; i += 2 {
		x, _ := strconv.ParseUint(h[i:i+2], 16, 8)
		m = math.Max(m, float64(x)/255)
	}
	return m
}

func saturation(h string) float64 {
	h = strings.TrimPrefix(h, "#")
	v := func(i int) float64 {
		x, _ := strconv.ParseUint(h[i:i+2], 16, 8)
		return float64(x) / 255
	}
	r, g, b := v(0), v(2), v(4)
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	if mx == 0 {
		return 0
	}
	return (mx - mn) / mx
}

// Login and the customer portal use the shared tokens, not their own palettes.
func TestSignInAndPortalUseSharedTokens(t *testing.T) {
	for _, f := range []string{"templates/admin_login.html", "templates/layout.html"} {
		b, err := tplFS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`#[0-9a-fA-F]{6}\b`).Match(b) {
			t.Errorf("%s still carries hex colours; use var(--token)", f)
		}
		if !strings.Contains(string(b), "tokens.css") {
			t.Errorf("%s does not load tokens.css", f)
		}
	}
	b, _ := staticFS.ReadFile("static/portal.css")
	if regexp.MustCompile(`#[0-9a-fA-F]{3,6}\b`).Match(b) {
		t.Error("portal.css has hard-coded colours")
	}
}

func TestCSSHasNoExternalURLs(t *testing.T) {
	for _, f := range []string{"static/app.css", "static/tokens.css", "static/portal.css"} {
		b, _ := staticFS.ReadFile(f)
		checkNoExternal(t, f, b)
	}
}

func checkNoExternal(t *testing.T, f string, b []byte) {
	for _, m := range regexp.MustCompile(`url\(\s*['"]?([^)'"]+)`).FindAllStringSubmatch(string(b), -1) {
		u := m[1]
		if strings.Contains(u, "://") || strings.HasPrefix(u, "//") {
			t.Errorf("%s loads %q", f, u)
		}
	}
}
