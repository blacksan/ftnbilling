package billing

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"ispbill/internal/store"
)

// The online MAC-vendor lookup. A prefix (OUI) that the bundled table does not know is asked of
// https://api.macvendors.com/<OUI> by ONE background worker, never from a request: Vendor() only notes the prefix
// and answers "" until the worker has the answer, and the page fills in on the next load.
//
// What leaves the server: the first three bytes of a MAC ("AA:BB:CC"), never the whole address, plus the
// User-Agent "ftnbilling". Limits: one request a second, MAC_VENDOR_DAILY a day (default 500), a 429 is honoured
// (Retry-After, then doubling back-off), 5 s timeout. A 200 is cached for good, a 404 for 30 days, and a prefix is
// never asked twice within a day whatever the answer. The answer of the web service is untrusted text: trimmed,
// stripped of control characters, capped, and escaped by the templates. Failures are silent (a rate-limited debug log).
const (
	vendorAPI          = "https://api.macvendors.com/"
	vendorUserAgent    = "ftnbilling"
	vendorTimeout      = 5 * time.Second
	vendorMinGap       = time.Second
	vendorMissTTL      = 30 * 24 * time.Hour
	vendorRetryAfter   = 24 * time.Hour
	vendorMaxBackoff   = time.Hour
	vendorDefaultDaily = 500
	vendorQueue        = 256
	vendorLogEvery     = time.Minute
)

// PrivateAddress is shown for a randomised (locally administered) MAC, which phones use per network: it is not
// an ONT's WAN address and no registry knows it, so it is never looked up.
const PrivateAddress = "Private (randomised) address"

// vendorAliases shortens the long registry names of makers an ISP meets to the name the operator uses. The first
// row whose Match is contained in the lower-cased registry name wins; the registry name stays as the tooltip.
var vendorAliases = []struct{ Match, Short string }{
	{"v-sol", "V-SOL"},
	{"routerboard", "MikroTik"},
	{"mikrotik", "MikroTik"},
	{"huawei", "Huawei"},
	{"zte corp", "ZTE"},
	{"tp-link", "TP-Link"},
	{"tenda", "Tenda"},
	{"ubiquiti", "Ubiquiti"},
	{"hangzhou h3c", "H3C"},
	{"fiberhome", "FiberHome"},
	{"nokia", "Nokia"},
	{"cambium", "Cambium"},
	{"netgear", "Netgear"},
	{"d-link", "D-Link"},
	{"xiaomi", "Xiaomi"},
	{"samsung", "Samsung"},
	{"apple", "Apple"},
}

// VendorAlias is the short name of a registry name ("" when no alias row matches).
func VendorAlias(full string) string {
	l := strings.ToLower(full)
	for _, a := range vendorAliases {
		if strings.Contains(l, a.Match) {
			return a.Short
		}
	}
	return ""
}

// cleanVendor makes the answer of the web service safe to keep and show: one line, no control characters, at most
// store.MaxOUIVendor characters. "" means "not a vendor name" (empty, or an error document).
func cleanVendor(b []byte) string {
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' || r == '�' {
			return ' '
		}
		return r
	}, string(b))
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || strings.HasPrefix(s, "{") || strings.HasPrefix(s, "<") {
		return ""
	}
	if r := []rune(s); len(r) > store.MaxOUIVendor {
		s = strings.TrimSpace(string(r[:store.MaxOUIVendor]))
	}
	return s
}

// ouiPrefix is "AA:BB:CC" of a canonical MAC.
func ouiPrefix(canon string) string { return canon[:8] }

// randomisedMAC is true when the locally administered bit of the first byte is set: the second hex digit is 2, 6,
// A or E (3, 7, B and F are multicast and never a device).
func randomisedMAC(canon string) bool { return hexNibble(canon[1])&2 != 0 }

// VendorOptions configures NewVendorLookup. Zero values are the production ones.
type VendorOptions struct {
	BaseURL string       // default https://api.macvendors.com/ (tests: a fake server)
	Client  *http.Client // default: 5 s timeout, no redirects
	Daily   int          // requests a day; default 500
	Now     func() time.Time
	// Sleep waits d or until ctx ends; tests replace it.
	Sleep func(ctx context.Context, d time.Duration)
}

// VendorLookup is the cache and the single background worker. Create it with NewVendorLookup, give it to
// SetVendorLookup, and run Run in one goroutine.
type VendorLookup struct {
	st      *store.Store
	log     *slog.Logger
	base    string
	client  *http.Client
	daily   int
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration)
	queue   chan string
	requeue atomic.Int64 // prefixes dropped because the queue was full (tests)

	mu       sync.Mutex
	cache    map[string]store.OUIEntry // mirror of the store table
	queued   map[string]bool
	tried    map[string]time.Time // last attempt of any kind, per prefix (in memory: a restart may ask again)
	day      string
	used     int
	blocked  time.Time // no request before this (429)
	backoff  time.Duration
	lastReq  time.Time
	lastLog  time.Time
	requests atomic.Int64 // requests made (tests)
}

// NewVendorLookup loads the cache from the store.
func NewVendorLookup(st *store.Store, log *slog.Logger, o VendorOptions) *VendorLookup {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	l := &VendorLookup{st: st, log: log, base: o.BaseURL, client: o.Client, daily: o.Daily, now: o.Now, sleep: o.Sleep,
		queue: make(chan string, vendorQueue), cache: map[string]store.OUIEntry{}, queued: map[string]bool{}, tried: map[string]time.Time{}}
	if l.base == "" {
		l.base = vendorAPI
	}
	if !strings.HasSuffix(l.base, "/") {
		l.base += "/"
	}
	if l.client == nil {
		l.client = &http.Client{Timeout: vendorTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if l.daily <= 0 {
		l.daily = vendorDefaultDaily
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.sleep == nil {
		l.sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
	if st != nil {
		st.View(func(d *store.Data) {
			for k, e := range d.OUICache {
				l.cache[k] = e
			}
		})
	}
	return l
}

// cached is the stored answer for a prefix: (vendor, true) for a found name; ("", false) when unknown or when
// the "no such prefix" answer is older than 30 days.
func (l *VendorLookup) cached(prefix string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.cache[prefix]
	if !ok || !e.Found {
		return "", false
	}
	return e.Vendor, true
}

// want notes a prefix for the worker. It never blocks and never touches the network.
func (l *VendorLookup) want(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.queued[prefix] || !l.eligibleLocked(prefix) {
		return
	}
	select {
	case l.queue <- prefix:
		l.queued[prefix] = true
	default:
		l.requeue.Add(1)
	}
}

// eligibleLocked: not answered yet, not asked within a day, and budget left.
func (l *VendorLookup) eligibleLocked(prefix string) bool {
	now := l.now()
	if e, ok := l.cache[prefix]; ok && (e.Found || now.Sub(e.CheckedAt) < vendorMissTTL) {
		return false
	}
	if t, ok := l.tried[prefix]; ok && now.Sub(t) < vendorRetryAfter {
		return false
	}
	l.rollDayLocked(now)
	return l.used < l.daily
}

func (l *VendorLookup) rollDayLocked(now time.Time) {
	if d := now.UTC().Format("2006-01-02"); d != l.day {
		l.day, l.used = d, 0
	}
}

// Run is the single worker: it takes the wanted prefixes one at a time until ctx ends.
func (l *VendorLookup) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-l.queue:
			l.process(ctx, p)
		}
	}
}

// process asks the service about one prefix (the worker's step; tests call it directly).
func (l *VendorLookup) process(ctx context.Context, prefix string) {
	l.mu.Lock()
	delete(l.queued, prefix)
	ok := l.eligibleLocked(prefix)
	wait := time.Duration(0)
	if ok {
		now := l.now()
		if l.blocked.After(now) {
			wait = l.blocked.Sub(now)
		}
		if gap := l.lastReq.Add(vendorMinGap).Sub(now); gap > wait {
			wait = gap
		}
	}
	l.mu.Unlock()
	if !ok {
		return
	}
	if wait > 0 {
		l.sleep(ctx, wait)
		if ctx.Err() != nil {
			return
		}
	}
	l.mu.Lock()
	now := l.now()
	l.rollDayLocked(now)
	l.used++
	l.tried[prefix] = now
	l.lastReq = now
	l.mu.Unlock()

	l.requests.Add(1)
	entry, status := l.fetch(ctx, prefix)
	switch status {
	case fetched:
		entry.CheckedAt = l.now()
		l.mu.Lock()
		l.cache[prefix] = entry
		l.backoff = 0
		l.mu.Unlock()
		if l.st != nil {
			if err := l.st.Update(func(d *store.Data) error {
				d.OUICache[prefix] = entry
				return nil
			}); err != nil {
				l.debug("vendor cache not saved", "err", err)
			}
		}
	case limited, failed:
		// the prefix is asked again after a day at the earliest (tried above)
	}
}

type fetchStatus int

const (
	fetched fetchStatus = iota
	limited
	failed
)

// fetch makes the one request. Only the OUI is in the URL.
func (l *VendorLookup) fetch(ctx context.Context, prefix string) (store.OUIEntry, fetchStatus) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.base+url.PathEscape(prefix), nil)
	if err != nil {
		return store.OUIEntry{}, failed
	}
	req.Header.Set("User-Agent", vendorUserAgent)
	req.Header.Set("Accept", "text/plain")
	resp, err := l.client.Do(req)
	if err != nil {
		l.debug("vendor lookup failed", "err", err.Error())
		return store.OUIEntry{}, failed
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch {
	case resp.StatusCode == http.StatusOK:
		v := cleanVendor(body)
		if v == "" {
			l.debug("vendor lookup: unusable answer")
			return store.OUIEntry{}, failed
		}
		return store.OUIEntry{Prefix: prefix, Vendor: v, Found: true}, fetched
	case resp.StatusCode == http.StatusNotFound:
		return store.OUIEntry{Prefix: prefix}, fetched
	case resp.StatusCode == http.StatusTooManyRequests:
		l.limit(resp.Header.Get("Retry-After"))
		return store.OUIEntry{}, limited
	default:
		l.debug("vendor lookup refused", "status", resp.StatusCode)
		return store.OUIEntry{}, failed
	}
}

// limit honours a 429: Retry-After seconds, else a back-off that doubles from a minute up to an hour.
func (l *VendorLookup) limit(retryAfter string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := time.Duration(0)
	if n, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && n > 0 {
		d = time.Duration(n) * time.Second
	}
	if l.backoff == 0 {
		l.backoff = time.Minute
	} else {
		l.backoff *= 2
	}
	if l.backoff > vendorMaxBackoff {
		l.backoff = vendorMaxBackoff
	}
	if d < l.backoff {
		d = l.backoff
	}
	if d > vendorMaxBackoff {
		d = vendorMaxBackoff
	}
	l.blocked = l.now().Add(d)
	l.debugLocked("vendor lookup rate limited", "wait", d.String())
}

func (l *VendorLookup) debug(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debugLocked(msg, args...)
}

// debugLocked logs at debug level, at most once a minute: a flaky network must not fill the log.
func (l *VendorLookup) debugLocked(msg string, args ...any) {
	now := l.now()
	if now.Sub(l.lastLog) < vendorLogEvery {
		return
	}
	l.lastLog = now
	l.log.Debug(msg, args...)
}

// vendorHook is the running lookup, if the operator has it on. Vendor() consults it; nothing else does.
var vendorHook atomic.Pointer[VendorLookup]

// SetVendorLookup turns the online lookup on for Vendor() (nil turns it off).
func SetVendorLookup(l *VendorLookup) { vendorHook.Store(l) }

// VendorFull is the registry name behind a shortened one, for a tooltip: "" when the short name is all there is.
func VendorFull(mac string) string {
	m, ok := store.CanonMAC(mac)
	if !ok || randomisedMAC(m) {
		return ""
	}
	if h := vendorHook.Load(); h != nil {
		if full, ok := h.cached(ouiPrefix(m)); ok && VendorAlias(full) != "" && VendorAlias(full) != full {
			return full
		}
	}
	return ""
}
