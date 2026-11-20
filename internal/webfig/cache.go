package webfig

import (
	"bytes"
	"container/list"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Static-file cache. WebFig loads the same engine script and package files on
// every open; they only change with the RouterOS version, so the server keeps
// them and answers repeat loads without touching the router (and without a
// proxy slot). Entries are per router (site ID) and RouterOS version, bounded
// by size (LRU) and age, and only responses that are safe to share are kept:
// 200, no Set-Cookie, no Vary beyond Accept-Encoding, a static file extension
// under /webfig/. The session check happens before any lookup.
const (
	DefaultCacheBytes = 64 << 20
	CacheTTL          = 24 * time.Hour
	maxCacheEntry     = 8 << 20
	// StaticCacheControl is what the browser is told for a static file: private because the files are
	// behind the WebFig session.
	StaticCacheControl = "private, max-age=86400"
)

var staticExt = map[string]bool{
	".js": true, ".jg": true, ".css": true, ".png": true, ".gif": true, ".jpg": true, ".jpeg": true,
	".svg": true, ".ico": true, ".woff": true, ".woff2": true, ".ttf": true, ".eot": true, ".otf": true,
}

// staticRequest: a plain GET of a static asset under /webfig/. /jsproxy, the
// index and /webfig/list (no extension) never qualify.
func staticRequest(r *http.Request) bool {
	if r.Method != http.MethodGet || r.Header.Get("Range") != "" || r.Header.Get("Upgrade") != "" {
		return false
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, "/webfig/") || len(r.URL.RawQuery) > 256 {
		return false
	}
	return staticExt[strings.ToLower(path.Ext(p))]
}

type cacheEntry struct {
	key, site, ver string
	body           []byte
	ctype, enc     string
	etag, lastMod  string
	stored         time.Time
}

type fileCache struct {
	mu      sync.Mutex
	max     int64
	ttl     time.Duration
	now     func() time.Time
	bytes   int64
	ll      *list.List
	items   map[string]*list.Element
	lastVer map[string]string // site -> version the cached files belong to
}

func newFileCache(max int64, ttl time.Duration, now func() time.Time) *fileCache {
	if now == nil {
		now = time.Now
	}
	return &fileCache{max: max, ttl: ttl, now: now, ll: list.New(), items: map[string]*list.Element{}, lastVer: map[string]string{}}
}

func cacheKey(site, ver string, r *http.Request) string {
	return site + "\x00" + ver + "\x00" + r.URL.Path + "?" + r.URL.RawQuery
}

// versionLocked drops a router's files when its version is not the one they
// were cached under.
func (c *fileCache) versionLocked(site, ver string) {
	if prev, ok := c.lastVer[site]; ok && prev != ver {
		c.purgeLocked(site)
	}
	c.lastVer[site] = ver
}

func (c *fileCache) purgeLocked(site string) {
	for k, el := range c.items {
		if el.Value.(*cacheEntry).site == site {
			c.removeLocked(k, el)
		}
	}
}

func (c *fileCache) removeLocked(k string, el *list.Element) {
	c.bytes -= int64(len(el.Value.(*cacheEntry).body))
	c.ll.Remove(el)
	delete(c.items, k)
}

func (c *fileCache) purgeSite(site string) {
	c.mu.Lock()
	c.purgeLocked(site)
	delete(c.lastVer, site)
	c.mu.Unlock()
}

func (c *fileCache) stats() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.bytes
}

// get returns a fresh entry the client can use (it must accept the stored encoding).
func (c *fileCache) get(site, ver string, r *http.Request) *cacheEntry {
	k := cacheKey(site, ver, r)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.versionLocked(site, ver)
	el, ok := c.items[k]
	if !ok {
		return nil
	}
	e := el.Value.(*cacheEntry)
	if c.now().Sub(e.stored) >= c.ttl {
		c.removeLocked(k, el)
		return nil
	}
	if e.enc != "" && !acceptsEncoding(r.Header.Get("Accept-Encoding"), e.enc) {
		return nil
	}
	c.ll.MoveToFront(el)
	return e
}

func (c *fileCache) put(e *cacheEntry) {
	if int64(len(e.body)) > c.max || len(e.body) > maxCacheEntry {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.versionLocked(e.site, e.ver)
	if el, ok := c.items[e.key]; ok {
		c.removeLocked(e.key, el)
	}
	e.stored = c.now()
	c.items[e.key] = c.ll.PushFront(e)
	c.bytes += int64(len(e.body))
	for c.bytes > c.max {
		last := c.ll.Back()
		if last == nil {
			break
		}
		c.removeLocked(last.Value.(*cacheEntry).key, last)
	}
}

func acceptsEncoding(header, enc string) bool {
	for _, part := range strings.Split(header, ",") {
		f := strings.Split(strings.ReplaceAll(part, " ", ""), ";")
		if !strings.EqualFold(f[0], enc) && f[0] != "*" {
			continue
		}
		for _, q := range f[1:] {
			if v, ok := strings.CutPrefix(strings.ToLower(q), "q="); ok {
				if x, err := strconv.ParseFloat(v, 64); err == nil && x <= 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

// staticPolicy looks at the router's own response headers (before ours are
// applied) and decides: the Cache-Control to send the browser, and whether the
// body may be kept on the server.
func staticPolicy(resp *http.Response) (cc string, store bool) {
	if resp.StatusCode != http.StatusOK || len(resp.Header.Values("Set-Cookie")) > 0 ||
		resp.Header.Get("Content-Range") != "" || len(resp.Header.Values("Www-Authenticate")) > 0 {
		return "", false
	}
	if v := strings.ToLower(strings.ReplaceAll(strings.Join(resp.Header.Values("Vary"), ","), " ", "")); v != "" && v != "accept-encoding" {
		return "", false
	}
	orig := strings.ToLower(resp.Header.Get("Cache-Control"))
	if strings.Contains(orig, "no-store") || strings.Contains(orig, "no-cache") {
		return "no-store", false
	}
	if i := strings.Index(orig, "max-age="); i >= 0 {
		rest := orig[i+len("max-age="):]
		if j := strings.IndexAny(rest, ", "); j >= 0 {
			rest = rest[:j]
		}
		if n, err := strconv.Atoi(rest); err == nil && n < 86400 {
			// The router asked for less than a day: honour it, and keep nothing here.
			return "private, max-age=" + strconv.Itoa(n), false
		}
	}
	return StaticCacheControl, true
}

// captureBody copies what the proxy streams to the browser and hands the whole
// body to done when it ended cleanly and completely.
type captureBody struct {
	rc   io.ReadCloser
	buf  bytes.Buffer
	over bool
	want int64
	done func([]byte)
	sent bool
}

func (c *captureBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 && !c.over {
		if c.buf.Len()+n > maxCacheEntry {
			c.over = true
			c.buf = bytes.Buffer{}
		} else {
			c.buf.Write(p[:n])
		}
	}
	if err == io.EOF && !c.over && !c.sent && (c.want < 0 || int64(c.buf.Len()) == c.want) {
		c.sent = true
		c.done(append([]byte(nil), c.buf.Bytes()...))
	}
	return n, err
}

func (c *captureBody) Close() error { return c.rc.Close() }
