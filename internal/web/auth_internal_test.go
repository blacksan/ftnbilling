package web

import (
	"fmt"
	"testing"
	"time"
)

// A reservation that ends without failing leaves nothing behind, and a full
// table drops the quietest keys, never everybody (which would give an attacker
// a fresh allowance by flooding the table).
func TestTrackerNoEntriesForCleanAttemptsAndLRUEviction(t *testing.T) {
	tr := newTracker(5, 0)
	now := time.Now()
	for i := 0; i < 100; i++ {
		k := fmt.Sprintf("clean-%d", i)
		if ok, _ := tr.reserve(k, now); !ok {
			t.Fatal("refused")
		}
		tr.release(k, now, false, false)
	}
	if len(tr.perIP) != 0 || len(tr.inflight) != 0 {
		t.Fatalf("attempts that never failed left %d/%d entries", len(tr.perIP), len(tr.inflight))
	}
	// Five failures of a hot key, then a flood of one-off keys past the cap.
	for i := 0; i < loginLimiterCap+500; i++ {
		k := fmt.Sprintf("flood-%d", i)
		tr.reserve(k, now)
		tr.release(k, now, true, false)
	}
	for i := 0; i < 5; i++ { // the key under attack is the most recently active
		tr.reserve("hot", now.Add(time.Second))
		tr.release("hot", now.Add(time.Second), true, false)
	}
	if len(tr.perIP) > loginLimiterCap {
		t.Fatalf("the table grew to %d", len(tr.perIP))
	}
	if ok, _ := tr.reserve("hot", now.Add(2*time.Second)); ok {
		t.Fatal("flooding the table wiped a blocked key's failures")
	}
	if len(tr.perIP) < loginLimiterCap/2 {
		t.Fatalf("eviction dropped nearly everything: %d left", len(tr.perIP))
	}
}

func TestLimiterEvictsQuietestKeysOnly(t *testing.T) {
	l := newLimiter(2, time.Hour)
	l.allow("busy")
	l.allow("busy")
	for i := 0; i < limiterCap+300; i++ {
		l.allow(fmt.Sprintf("k%d", i))
	}
	if len(l.hits) > limiterCap {
		t.Fatalf("grew to %d", len(l.hits))
	}
	// "busy" was the oldest, so it may go; but the table must not have been wiped.
	if len(l.hits) < limiterCap/2 {
		t.Fatalf("wiped: %d", len(l.hits))
	}
	l.allow("late")
	l.allow("late")
	if l.allow("late") {
		t.Fatal("a recent key lost its count")
	}
}

func TestAddrKeyGroupsIPv6By64(t *testing.T) {
	for in, want := range map[string]string{
		"2001:db8:1:2::1":      "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::9": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "2001:db8:1:3::/64",
		"::ffff:203.0.113.5":   "203.0.113.5",
		"203.0.113.5":          "203.0.113.5",
		"not-an-ip":            "not-an-ip",
	} {
		if got := addrKey(in); got != want {
			t.Errorf("addrKey(%q) = %q, want %q", in, got, want)
		}
	}
}
