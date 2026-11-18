package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// D8 of docs/release-plan-routers.md: the product says "router", the code keeps
// "site". Every /api/v1/sites… route is also served at /api/v1/routers…, the
// query parameter ?router= means ?site=, a request body's router_id means
// site_id, and every answer that carries site_id / site_name also carries
// router_id / router_name. It is all done here, once, so handlers never see it.

const (
	apiSitesPrefix   = "/api/v1/sites"
	apiRoutersPrefix = "/api/v1/routers"
)

// aliasPatterns returns the patterns to register for one route: the pattern
// itself and, for a /sites route, its /routers twin.
func aliasPatterns(pattern string) []string {
	if i := strings.Index(pattern, apiSitesPrefix); i >= 0 {
		rest := pattern[i+len(apiSitesPrefix):]
		if rest == "" || rest[0] == '/' {
			return []string{pattern, pattern[:i] + apiRoutersPrefix + rest}
		}
	}
	return []string{pattern}
}

// aliasRequest maps ?router= to ?site= and a JSON body's router_id to site_id.
func aliasRequest(r *http.Request) {
	if q := r.URL.Query(); q.Has("router") && !q.Has("site") {
		q.Set("site", q.Get("router"))
		q.Del("router")
		r.URL.RawQuery = q.Encode()
	}
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(b))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	if !bytes.Contains(b, []byte(`"router_id"`)) {
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return
	}
	v, ok := m["router_id"]
	if !ok {
		return
	}
	delete(m, "router_id")
	if _, has := m["site_id"]; !has {
		m["site_id"] = v
	}
	if nb, err := json.Marshal(m); err == nil {
		r.Body = io.NopCloser(bytes.NewReader(nb))
		r.ContentLength = int64(len(nb))
	}
}

// addRouterAliases returns b with router_id / router_name beside every
// site_id / site_name, at any depth. b is returned unchanged when it has none.
func addRouterAliases(b []byte) []byte {
	if !bytes.Contains(b, []byte(`"site_id"`)) && !bytes.Contains(b, []byte(`"site_name"`)) {
		return b
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return b
	}
	aliasWalk(v)
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

func aliasWalk(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			aliasWalk(x)
			switch k {
			case "site_id":
				if _, has := t["router_id"]; !has {
					t["router_id"] = x
				}
			case "site_name":
				if _, has := t["router_name"]; !has {
					t["router_name"] = x
				}
			}
		}
	case []any:
		for _, x := range t {
			aliasWalk(x)
		}
	}
}
