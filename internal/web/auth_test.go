package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// get runs a Basic auth GET /admin as client ip (through X-Forwarded-For).
func (r *setRig) getAs(ip, user, pass string) int {
	req, _ := http.NewRequest("GET", r.ts.URL+"/admin", nil)
	req.SetBasicAuth(user, pass)
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}
	resp, err := r.cl.Do(req)
	if err != nil {
		panic(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Twenty parallel wrong passwords from one address: at most five reach the
// pbkdf2 check, the rest are told to wait.
func TestLoginParallelAttemptsAreLimited(t *testing.T) {
	r := newSetRig(t)
	if err := r.m.SetAdminPassword(longPW, "test"); err != nil {
		t.Fatal(err)
	}
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
			switch r.getAs("", "admin", "wrong-password-x") {
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
	// And the address stays blocked afterwards, even for the right password.
	if c := r.getAs("", "admin", longPW); c != 429 {
		t.Fatalf("blocked address: %d", c)
	}
}

// While the single pbkdf2 slot is busy and the waiting room is full, further
// checks get 429 at once.
func TestLoginBusySlotAnswers429(t *testing.T) {
	r := newSetRig(t)
	r.m.SetAdminPassword(longPW, "test")
	release := make(chan struct{})
	var started atomic.Int32
	r.srv.SetHashVerifier(func(string, string) bool { started.Add(1); <-release; return false })
	var wg sync.WaitGroup
	codes := make(chan int, 6)
	// Five different addresses so the per-address limit is not what stops them.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- r.getAs("127.0.0."+string(rune('1'+i)), "admin", "wrong-password-x")
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
		if c := r.getAs("", "admin", ""); c != 401 {
			t.Fatalf("empty password: %d", c)
		}
	}
	if c := r.getAs("", "admin", pw); c != 200 {
		t.Fatalf("owner locked out by empty passwords: %d", c)
	}
}

// A mistyped "current password" on an action has its own counter: it never
// blocks the admin's page loads.
func TestReauthFailuresDoNotBlockLogin(t *testing.T) {
	r := newSetRig(t)
	for i := 0; i < 8; i++ {
		code, msg, _ := r.post("/admin/settings/password", url.Values{"current_password": {"nope"}, "new_password": {longPW}, "confirm_password": {longPW}})
		if code != 303 || !strings.HasPrefix(msg, "Error") {
			t.Fatalf("attempt %d: %d %q", i, code, msg)
		}
	}
	if c := r.getAs("", "admin", pw); c != 200 {
		t.Fatalf("page loads blocked by mistyped current passwords: %d", c)
	}
	// The re-auth field itself is limited, so it cannot be used to guess.
	code, msg, _ := r.post("/admin/settings/password", url.Values{"current_password": {pw}, "new_password": {longPW}, "confirm_password": {longPW}})
	if code != 303 || !strings.HasPrefix(msg, "Error") || r.m.HasAdminPassword() {
		t.Fatalf("re-auth not limited: %d %q", code, msg)
	}
}

// The global limit must not lock the owner out: a remembered login, and an
// address that logged in before, are exempt from it.
func TestGlobalLimitDoesNotLockOwnerOut(t *testing.T) {
	r := newSetRig(t)
	r.m.SetAdminPassword(longPW, "test")
	if c := r.getAs("127.0.0.7", "admin", longPW); c != 200 {
		t.Fatalf("first login: %d", c)
	}
	r.srv.AuthInjectFailures("127.0.0.9", 40)
	if c := r.getAs("127.0.0.7", "admin", longPW); c != 200 {
		t.Fatalf("remembered login blocked by the global limit: %d", c)
	}
	r.srv.AuthFlush()
	if c := r.getAs("127.0.0.7", "admin", longPW); c != 200 {
		t.Fatalf("known address blocked by the global limit: %d", c)
	}
	if c := r.getAs("127.0.0.50", "admin", "wrong-password-x"); c != 429 {
		t.Fatalf("a stranger guessing should wait under the global limit: %d", c)
	}
}

// A remembered login is bound to the hash it was verified against: after the
// password changes, neither an old entry nor a slow check that finishes late
// can bring the old password back.
func TestAuthCacheTiedToHash(t *testing.T) {
	r := newSetRig(t)
	r.m.SetAdminPassword(longPW, "test")
	oldHash := r.m.AdminHash()
	started, release := make(chan struct{}), make(chan struct{})
	r.srv.SetHashVerifier(func(string, string) bool { close(started); <-release; return true })
	done := make(chan int)
	go func() { done <- r.getAs("", "admin", longPW) }()
	<-started
	const newPW = "another-long-password-77"
	if err := r.m.SetAdminPassword(newPW, "test"); err != nil {
		t.Fatal(err)
	}
	r.srv.AuthFlush()
	close(release) // the slow check of the old password finishes after the flush
	<-done
	r.srv.SetHashVerifier(func(h, p string) bool { return p == newPW })
	if c := r.getAs("", "admin", longPW); c != 401 {
		t.Fatalf("old password accepted after the change: %d", c)
	}
	r.srv.AuthRemember("admin", longPW, oldHash)
	if c := r.getAs("", "admin", longPW); c != 401 {
		t.Fatalf("an entry bound to the old hash was honoured: %d", c)
	}
	if c := r.getAs("", "admin", newPW); c != 200 {
		t.Fatalf("new password: %d", c)
	}
}
