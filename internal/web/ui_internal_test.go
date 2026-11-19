package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// W7 guards on the source of the admin UI: the product word, no external URL,
// no inline script or handler.

var actionRE = regexp.MustCompile(`(?s)\{\{.*?\}\}`)

func adminTemplateNames(t *testing.T) []string {
	t.Helper()
	ents, err := fs.ReadDir(tplFS, "templates")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		switch n {
		case "portal.html", "wait.html", "pay.html", "success.html", "layout.html":
			continue // the tenant side is not part of the admin redesign
		}
		out = append(out, n)
	}
	return out
}

func TestNoBuildingWordInAdminTemplates(t *testing.T) {
	names := adminTemplateNames(t)
	if len(names) < 10 {
		t.Fatalf("looked at only %d templates", len(names))
	}
	for _, n := range names {
		b, _ := fs.ReadFile(tplFS, "templates/"+n)
		// Template actions may name Go fields such as .Building; only text counts.
		text := actionRE.ReplaceAllString(string(b), "")
		if i := strings.Index(strings.ToLower(text), "uilding"); i >= 0 {
			t.Errorf("%s: user-facing \"building\" near %q", n, text[max(0, i-30):min(len(text), i+30)])
		}
	}
}

var extURL = regexp.MustCompile(`(?i)(https?:)?//[a-z0-9][a-z0-9-]*\.[a-z]{2,}`)

func TestNoExternalURLsInAdminUI(t *testing.T) {
	var files []string
	for _, n := range adminTemplateNames(t) {
		files = append(files, "templates/"+n)
	}
	files = append(files, "static/app.css", "static/app.js", "static/theme.js")
	for _, f := range files {
		b, err := fs.ReadFile(tplFS, f)
		if err != nil {
			b, err = fs.ReadFile(staticFS, f)
		}
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		s := strings.ReplaceAll(string(b), "http://www.w3.org/2000/svg", "")
		s = strings.ReplaceAll(s, "http://www.w3.org/1999/xlink", "")
		if m := extURL.FindString(s); m != "" {
			t.Errorf("%s: external URL %q (the CSP keeps the admin on this origin)", f, m)
		}
	}
}

var inlineHandler = regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*["']`)

func TestAdminTemplatesHaveNoInlineScript(t *testing.T) {
	for _, n := range adminTemplateNames(t) {
		b, _ := fs.ReadFile(tplFS, "templates/"+n)
		s := string(b)
		if m := inlineHandler.FindString(s); m != "" {
			t.Errorf("%s: inline handler %q", n, m)
		}
		for _, m := range regexp.MustCompile(`(?i)<script[^>]*>`).FindAllString(s, -1) {
			if !strings.Contains(m, "src=") {
				t.Errorf("%s: inline script tag %q", n, m)
			}
		}
		if strings.Contains(strings.ToLower(s), "javascript:") {
			t.Errorf("%s: javascript: URL", n)
		}
	}
}

func TestAdminCSPHasNoInlineScript(t *testing.T) {
	for _, c := range []string{cspAdmin, cspStrict} {
		for _, d := range strings.Split(c, ";") {
			d = strings.TrimSpace(d)
			if strings.HasPrefix(d, "script-src") && strings.Contains(d, "unsafe") {
				t.Errorf("script-src allows inline or eval: %q", d)
			}
		}
		for _, bad := range []string{"http:", "https:", "*"} {
			if strings.Contains(c, " "+bad) {
				t.Errorf("csp opens %s: %s", bad, c)
			}
		}
	}
}
