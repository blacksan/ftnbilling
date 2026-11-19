package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
)

// Everything under static/ is served at /static/{path...} (D10). Files are
// immutable for a year; templates add ?v=<build hash> (the "static" template
// function) so a changed file gets a new address.

//go:embed static
var staticFS embed.FS

var staticTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".woff2": "font/woff2",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".txt":   "text/plain; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".ico":   "image/x-icon",
}

var (
	staticHashOnce sync.Once
	staticHashVal  string
)

// staticHash identifies this build's static files: a digest of their names
// and bytes, so ?v= changes exactly when an asset does.
func staticHash() string {
	staticHashOnce.Do(func() {
		var names []string
		_ = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				names = append(names, p)
			}
			return nil
		})
		sort.Strings(names)
		h := sha256.New()
		for _, n := range names {
			b, _ := staticFS.ReadFile(n)
			h.Write([]byte(n))
			h.Write([]byte{0})
			h.Write(b)
		}
		staticHashVal = hex.EncodeToString(h.Sum(nil))[:10]
	})
	return staticHashVal
}

// staticURL is the template helper: {{static "app.css"}} gives /static/app.css?v=<hash>.
func staticURL(name string) string {
	return "/static/" + strings.TrimPrefix(name, "/") + "?v=" + staticHash()
}

func (s *Server) staticRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /static/{path...}", serveStatic)
}

func serveStatic(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if p == "" || strings.Contains(p, "..") || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/" + p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := staticTypes[strings.ToLower(path.Ext(p))]
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(p))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+staticHash()+`"`)
	if r.Header.Get("If-None-Match") == h.Get("ETag") {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(b)
}
