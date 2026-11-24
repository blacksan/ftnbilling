package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
)

// fakeVendorAPI is a stand-in for api.macvendors.com that records every request.
type fakeVendorAPI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
	uas   []string
	code  int               // answer for any prefix not in names
	names map[string]string // OUI path -> 200 body
	retry string            // Retry-After for a 429
}

func newFakeVendorAPI(t *testing.T) *fakeVendorAPI {
	t.Helper()
	f := &fakeVendorAPI{names: map[string]string{}, code: http.StatusNotFound}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, strings.TrimPrefix(r.URL.Path, "/"))
		f.uas = append(f.uas, r.Header.Get("User-Agent"))
		name, ok := f.names[strings.TrimPrefix(r.URL.Path, "/")]
		code, retry := f.code, f.retry
		f.mu.Unlock()
		if ok {
			_, _ = w.Write([]byte(name))
			return
		}
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"errors":{"detail":"Not Found"}}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVendorAPI) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.paths) }

// vendorRig is a lookup with a fake clock and a sleep that only moves the clock.
type vendorRig struct {
	l      *VendorLookup
	api    *fakeVendorAPI
	st     *store.Store
	now    time.Time
	slept  []time.Duration
	handle *storetest.Handle
}

func newVendorRig(t *testing.T, daily int) *vendorRig {
	t.Helper()
	r := &vendorRig{api: newFakeVendorAPI(t), now: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC), handle: storetest.New(t)}
	r.st = r.handle.Store
	r.l = r.build(daily)
	t.Cleanup(func() { SetVendorLookup(nil) })
	SetVendorLookup(r.l)
	return r
}

func (r *vendorRig) build(daily int) *VendorLookup {
	return NewVendorLookup(r.st, nil, VendorOptions{BaseURL: r.api.srv.URL, Daily: daily,
		Now:   func() time.Time { return r.now },
		Sleep: func(_ context.Context, d time.Duration) { r.slept = append(r.slept, d); r.now = r.now.Add(d) }})
}

// drain runs the worker's step for everything wanted so far.
func (r *vendorRig) drain() {
	for {
		select {
		case p := <-r.l.queue:
			r.l.process(context.Background(), p)
		default:
			return
		}
	}
}

// These prefixes are in no bundled table (checked by needUnknown).
const (
	unkA = "10:77:01"
	unkB = "10:77:02"
	unkC = "10:77:03"
)

func needUnknown(t *testing.T, prefixes ...string) {
	t.Helper()
	for _, p := range prefixes {
		if ouiVendors[p] != "" || ieeeOUI[p] != "" {
			t.Skipf("%s is in a bundled table", p)
		}
	}
}

func TestVendorLookupAsksOnlyForTheOUI(t *testing.T) {
	needUnknown(t, unkA)
	r := newVendorRig(t, 0)
	r.api.names[unkA] = "Acme Optical Co., Ltd."
	full := unkA + ":DE:AD:BE"
	if got := Vendor(full); got != "" {
		t.Fatalf("first sight answers nothing yet, got %q", got)
	}
	r.drain()
	if r.api.count() != 1 {
		t.Fatalf("requests = %d", r.api.count())
	}
	p := r.api.paths[0]
	if p != unkA || len(p) != 8 || strings.Contains(strings.ToUpper(p), "DE:AD") {
		t.Fatalf("the request carried more than the OUI: %q", p)
	}
	if r.api.uas[0] != "ftnbilling" {
		t.Fatalf("User-Agent %q", r.api.uas[0])
	}
	if got := Vendor(full); got != "Acme Optical Co., Ltd." {
		t.Fatalf("the next load shows the name, got %q", got)
	}
}

func TestVendorLookupCacheHitAvoidsTheNetworkAndSurvivesARestart(t *testing.T) {
	needUnknown(t, unkA)
	r := newVendorRig(t, 0)
	r.api.names[unkA] = "Acme Optical"
	Vendor(unkA + ":00:00:01")
	r.drain()
	for i := 0; i < 5; i++ {
		if Vendor(unkA+":00:00:0"+string(rune('1'+i))) != "Acme Optical" {
			t.Fatal("cache miss")
		}
	}
	r.drain()
	if r.api.count() != 1 {
		t.Fatalf("a cache hit went to the network: %d requests", r.api.count())
	}
	// a restart: a new lookup on the same store knows the answer without asking
	l2 := r.build(0)
	if v, ok := l2.cached(unkA); !ok || v != "Acme Optical" {
		t.Fatalf("not persisted: %q %v", v, ok)
	}
	var e store.OUIEntry
	r.st.View(func(d *store.Data) { e = d.OUICache[unkA] })
	if !e.Found || e.Vendor != "Acme Optical" {
		t.Fatalf("store row %+v", e)
	}
}

func TestVendorLookupIsOneRequestASecond(t *testing.T) {
	needUnknown(t, unkA, unkB, unkC)
	r := newVendorRig(t, 0)
	for _, p := range []string{unkA, unkB, unkC} {
		r.api.names[p] = "Maker " + p
		Vendor(p + ":00:00:01")
	}
	r.drain()
	if r.api.count() != 3 {
		t.Fatalf("requests = %d", r.api.count())
	}
	var waited time.Duration
	for _, d := range r.slept {
		waited += d
	}
	if waited < 2*time.Second {
		t.Fatalf("three requests were not spaced a second apart: slept %v", r.slept)
	}
}

func TestVendorLookupDailyBudget(t *testing.T) {
	needUnknown(t, unkA, unkB, unkC)
	r := newVendorRig(t, 2)
	for _, p := range []string{unkA, unkB, unkC} {
		Vendor(p + ":00:00:01")
	}
	r.drain()
	if r.api.count() != 2 {
		t.Fatalf("budget of 2 allowed %d requests", r.api.count())
	}
	r.now = r.now.Add(25 * time.Hour) // the next day the budget is back
	Vendor(unkC + ":00:00:01")
	r.drain()
	if r.api.count() != 3 {
		t.Fatalf("next day: %d requests", r.api.count())
	}
}

func TestVendorLookupMissIsCachedThirtyDaysAndNeverAskedTwiceADay(t *testing.T) {
	needUnknown(t, unkA)
	r := newVendorRig(t, 0)
	Vendor(unkA + ":00:00:01") // the fake answers 404
	r.drain()
	if r.api.count() != 1 {
		t.Fatal("no first request")
	}
	var e store.OUIEntry
	r.st.View(func(d *store.Data) { e = d.OUICache[unkA] })
	if e.Found || e.CheckedAt.IsZero() {
		t.Fatalf("the 404 is not stored: %+v", e)
	}
	for _, later := range []time.Duration{time.Hour, 23 * time.Hour, 28 * 24 * time.Hour} { // 1 h, 1 day, 29 days in all
		r.now = r.now.Add(later)
		Vendor(unkA + ":00:00:01")
		r.drain()
	}
	if r.api.count() != 1 {
		t.Fatalf("a missing prefix was asked again inside 30 days: %d", r.api.count())
	}
	r.now = r.now.Add(5 * 24 * time.Hour)
	Vendor(unkA + ":00:00:01")
	r.drain()
	if r.api.count() != 2 {
		t.Fatalf("after 30 days it is asked again: %d", r.api.count())
	}
}

func TestVendorLookupHonoursTooManyRequests(t *testing.T) {
	needUnknown(t, unkA, unkB)
	r := newVendorRig(t, 0)
	r.api.code, r.api.retry = http.StatusTooManyRequests, "120"
	Vendor(unkA + ":00:00:01")
	r.drain()
	var e store.OUIEntry
	var has bool
	r.st.View(func(d *store.Data) { e, has = d.OUICache[unkA] })
	if has {
		t.Fatalf("a 429 must not be cached as an answer: %+v", e)
	}
	// the same prefix is not retried within a day, whatever happened
	r.now = r.now.Add(3 * time.Hour)
	Vendor(unkA + ":00:00:01")
	r.drain()
	if r.api.count() != 1 {
		t.Fatalf("retried after a 429 inside a day: %d", r.api.count())
	}
	// another prefix waits out the Retry-After first
	r.api.code, r.api.retry = http.StatusNotFound, ""
	r.api.mu.Lock()
	r.api.names[unkB] = "Maker B"
	r.api.mu.Unlock()
	r.slept = nil
	Vendor(unkB + ":00:00:01")
	r.l.mu.Lock()
	r.l.blocked = r.now.Add(90 * time.Second)
	r.l.mu.Unlock()
	r.drain()
	if len(r.slept) == 0 || r.slept[0] < 90*time.Second {
		t.Fatalf("did not wait out the 429: slept %v", r.slept)
	}
}

func TestVendorLookupBackoffGrowsAndIsCapped(t *testing.T) {
	r := newVendorRig(t, 0)
	r.l.limit("")
	if d := r.l.blocked.Sub(r.now); d != time.Minute {
		t.Fatalf("first back-off %v", d)
	}
	r.l.limit("")
	if d := r.l.blocked.Sub(r.now); d != 2*time.Minute {
		t.Fatalf("second back-off %v", d)
	}
	for i := 0; i < 10; i++ {
		r.l.limit("999999")
	}
	if d := r.l.blocked.Sub(r.now); d != time.Hour {
		t.Fatalf("cap %v", d)
	}
}

func TestVendorLookupSkipsRandomisedAddresses(t *testing.T) {
	r := newVendorRig(t, 0)
	for _, m := range []string{"02:11:22:33:44:55", "06:11:22:33:44:55", "0A:11:22:33:44:55", "0e:11:22:33:44:55", "DA:A1:19:00:00:01"} {
		if got := Vendor(m); got != PrivateAddress {
			t.Errorf("Vendor(%s) = %q", m, got)
		}
	}
	r.drain()
	if r.api.count() != 0 || len(r.l.queue) != 0 {
		t.Fatalf("a randomised MAC was looked up (%d requests)", r.api.count())
	}
	if PrivateAddress != "Private (randomised) address" {
		t.Fatalf("wording changed: %q", PrivateAddress)
	}
}

func TestVendorAliasKeepsTheRegistryNameForATooltip(t *testing.T) {
	needUnknown(t, unkA, unkB)
	r := newVendorRig(t, 0)
	r.api.names[unkA] = "Shenzhen V-SOL Technologies Co., Ltd."
	r.api.names[unkB] = "Routerboard.com"
	Vendor(unkA + ":00:00:01")
	Vendor(unkB + ":00:00:01")
	r.drain()
	if got := Vendor(unkA + ":00:00:01"); got != "V-SOL" {
		t.Fatalf("alias: %q", got)
	}
	if got := VendorFull(unkA + ":00:00:01"); got != "Shenzhen V-SOL Technologies Co., Ltd." {
		t.Fatalf("tooltip: %q", got)
	}
	if got := Vendor(unkB + ":00:00:01"); got != "MikroTik" {
		t.Fatalf("alias: %q", got)
	}
	if VendorFull("AA:BB:CC:00:00:01") != "" {
		t.Fatal("a name with no alias has no tooltip")
	}
	if VendorAlias("Some Unlisted Maker") != "" {
		t.Fatal("unlisted maker got an alias")
	}
}

func TestVendorAnswersAreCleanedAndCapped(t *testing.T) {
	for in, want := range map[string]string{
		"  Acme\tOptical \n Ltd \x00\x07": "Acme Optical Ltd",
		`{"errors":{"detail":"x"}}`:       "",
		"<html>no</html>":                 "",
		"   ":                             "",
	} {
		if got := cleanVendor([]byte(in)); got != want {
			t.Errorf("cleanVendor(%q) = %q, want %q", in, got, want)
		}
	}
	long := cleanVendor([]byte(strings.Repeat("X", 500)))
	if len([]rune(long)) != store.MaxOUIVendor {
		t.Fatalf("not capped: %d", len([]rune(long)))
	}
}

// Vendor never waits for the network: a server that never answers cannot slow a page.
func TestVendorNeverTouchesTheNetworkOnTheRequestPath(t *testing.T) {
	needUnknown(t, unkA)
	release := make(chan struct{})
	hits := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits <- struct{}{}; <-release }))
	defer srv.Close()
	defer close(release)
	h := storetest.New(t)
	l := NewVendorLookup(h.Store, nil, VendorOptions{BaseURL: srv.URL})
	SetVendorLookup(l)
	t.Cleanup(func() { SetVendorLookup(nil) })
	done := make(chan string, 1)
	go func() { done <- Vendor(unkA + ":00:00:01") }()
	select {
	case v := <-done:
		if v != "" {
			t.Fatalf("got %q", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Vendor blocked")
	}
	select {
	case <-hits:
		t.Fatal("Vendor made a request itself")
	case <-time.After(100 * time.Millisecond):
	}
	if len(l.queue) != 1 {
		t.Fatalf("the prefix was not queued for the worker: %d", len(l.queue))
	}
	// asking again does not queue it twice
	Vendor(unkA + ":00:00:02")
	if len(l.queue) != 1 {
		t.Fatalf("queued twice: %d", len(l.queue))
	}
}

func TestVendorLookupOffDoesNothing(t *testing.T) {
	needUnknown(t, unkA)
	SetVendorLookup(nil)
	api := newFakeVendorAPI(t)
	if got := Vendor(unkA + ":00:00:01"); got != "" || api.count() != 0 {
		t.Fatalf("lookup off: %q, %d requests", got, api.count())
	}
}

func TestVendorBundledTableStillBeatsTheNetwork(t *testing.T) {
	r := newVendorRig(t, 0)
	if got := Vendor("38:32:7A:00:00:01"); got != "MikroTik" {
		t.Fatalf("got %q", got)
	}
	r.drain()
	if r.api.count() != 0 {
		t.Fatal("a bundled prefix was looked up")
	}
}

func TestVendorLookupFailureIsSilentAndCachesNothing(t *testing.T) {
	needUnknown(t, unkA)
	r := newVendorRig(t, 0)
	r.api.code = http.StatusInternalServerError
	Vendor(unkA + ":00:00:01")
	r.drain()
	var has bool
	r.st.View(func(d *store.Data) { _, has = d.OUICache[unkA] })
	if has {
		t.Fatal("a failure was cached")
	}
	if got := Vendor(unkA + ":00:00:01"); got != "" {
		t.Fatalf("got %q", got)
	}
}
