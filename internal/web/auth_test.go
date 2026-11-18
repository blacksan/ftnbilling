package web_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// attempt posts the login form as client ip (through X-Forwarded-For) and
// returns the status and body. The login CSRF cookie is fetched once per rig.
func (r *setRig) attempt(ip, user, pass string) (int, string, http.Header) {
	return r.attemptWith(ip, user, pass)
}

// attemptWith is attempt that also sends cookies (a known-browser cookie).
func (r *setRig) attemptWith(ip, user, pass string, extra ...*http.Cookie) (int, string, http.Header) {
	lc := r.loginCookie()
	form := url.Values{"username": {user}, "password": {pass}, "_csrf": {lc.Value}}
	req, _ := http.NewRequest("POST", r.ts.URL+"/admin/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(lc)
	for _, c := range extra {
		req.AddCookie(c)
	}
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}
	resp, err := r.cl.Do(req)
	if err != nil {
		panic(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b), resp.Header
}

var lcOnce sync.Map

func (r *setRig) loginCookie() *http.Cookie {
	if v, ok := lcOnce.Load(r); ok {
		return v.(*http.Cookie)
	}
	resp, err := r.cl.Get(r.ts.URL + "/admin/login")
	if err != nil {
		panic(err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if strings.HasSuffix(c.Name, "ftn_lc") {
			lcOnce.Store(r, c)
			return c
		}
	}
	panic("no login cookie")
}

// Twenty parallel wrong passwords from one address: at most five reach the
// pbkdf2 check, the rest are told to wait.
func TestLoginParallelAttemptsAreLimited(t *testing.T) {
	r := newSetRig(t)
	var evals atomic.Int32
	r.srv.SetHashVerifier(func(string, string) bool {
		evals.Add(1)
		time.Sleep(30 * time.Millisecond)
		return false
	})
	var wg sync.WaitGroup
	var c401, c429, other atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch code, _, _ := r.attempt("", "admin", "wrong-password-x"); code {
			case 401:
				c401.Add(1)
			case 429:
				c429.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	wg.Wait()
	if evals.Load() > 5 || other.Load() != 0 || c429.Load() < 15 || c401.Load() > 5 {
		t.Fatalf("evaluations=%d 401=%d 429=%d other=%d", evals.Load(), c401.Load(), c429.Load(), other.Load())
	}
	// The address stays blocked, even for the right password, and is told for how long.
	code, _, h := r.attempt("", "admin", pw)
	if code != 429 || h.Get("Retry-After") == "" {
		t.Fatalf("blocked address: %d retry-after=%q", code, h.Get("Retry-After"))
	}
}

// While the single pbkdf2 slot is busy and the waiting room is full, further
// checks get 429 at once.
func TestLoginBusySlotAnswers429(t *testing.T) {
	r := newSetRig(t)
	release := make(chan struct{})
	var started atomic.Int32
	r.srv.SetHashVerifier(func(string, string) bool { started.Add(1); <-release; return false })
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	// Different addresses, so the per-address limit is not what stops them.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _, _ := r.attempt("127.0.0."+string(rune('1'+i)), "admin", "wrong-password-x")
			codes <- c
		}(i)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(codes) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c := <-codes; c != 429 {
		t.Fatalf("a request beyond the slot and the waiting room got %d", c)
	}
	close(release)
	wg.Wait()
	if started.Load() > 5 {
		t.Fatalf("started %d", started.Load())
	}
}

func TestEmptyPasswordIsNotCounted(t *testing.T) {
	r := newSetRig(t)
	for i := 0; i < 12; i++ {
		if c, _, _ := r.attempt("", "admin", ""); c != 401 {
			t.Fatalf("empty password: %d", c)
		}
	}
	if c, _, _ := r.attempt("", "admin", pw); c != 303 {
		t.Fatalf("owner locked out by empty passwords: %d", c)
	}
}

// Five wrong passwords from one address block it; a different address is not
// affected; the first address stays blocked even with the right password.
func TestPerAddressLockout(t *testing.T) {
	r := newSetRig(t)
	for i := 0; i < 5; i++ {
		if c, _, _ := r.attempt("203.0.113.5", "admin", "wrong-password-x"); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	code, body, h := r.attempt("203.0.113.5", "admin", pw)
	if code != 429 || h.Get("Retry-After") == "" || !strings.Contains(body, "Too many") {
		t.Fatalf("sixth attempt: %d %q", code, h.Get("Retry-After"))
	}
	if c, _, _ := r.attempt("203.0.113.6", "admin", pw); c != 303 {
		t.Fatalf("another address: %d", c)
	}
}

// An unknown user and a wrong password look the same, and cost the same checks.
func TestNoUserEnumeration(t *testing.T) {
	r := newSetRig(t)
	var evals atomic.Int32
	r.srv.SetHashVerifier(func(string, string) bool { evals.Add(1); return false })
	c1, b1, _ := r.attempt("203.0.113.1", "admin", "wrong-password-x")
	c2, b2, _ := r.attempt("203.0.113.2", "nobody", "wrong-password-x")
	if c1 != c2 || c1 != 401 {
		t.Fatalf("status differs: %d vs %d", c1, c2)
	}
	strip := func(s string) string {
		i := strings.Index(s, `name="_csrf"`)
		if i < 0 {
			return s
		}
		return s[:i] + s[strings.Index(s[i:], ">")+i:]
	}
	norm := func(s string) string { return strings.ReplaceAll(strip(s), "nobody", "admin") }
	if norm(b1) != norm(b2) {
		t.Fatal("the pages differ between a wrong password and an unknown user")
	}
	if evals.Load() != 2 {
		t.Fatalf("an unknown user must still cost a password check: %d checks", evals.Load())
	}
}
