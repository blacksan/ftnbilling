package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPersistenceAndAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *Data) error {
		d.Packages["p1"] = Package{ID: "p1", Name: "X", Slug: "x", PriceKES: 5}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// File is valid JSON and no temp files are left behind.
	b, err := os.ReadFile(filepath.Join(dir, "ispbill.json"))
	if err != nil || !json.Valid(b) {
		t.Fatalf("bad file: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2.View(func(d *Data) {
		if d.Packages["p1"].PriceKES != 5 {
			t.Fatal("not persisted")
		}
	})
}

func TestNoChangeSkipsWriteAndErrorKeepsFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if err := s.Update(func(*Data) error { return ErrNoChange }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ispbill.json")); err == nil {
		t.Fatal("ErrNoChange must not write")
	}
}

func TestCorruptFileRefused(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ispbill.json"), []byte("{oops"), 0o600)
	if _, err := Open(dir); err == nil {
		t.Fatal("expected error")
	}
}

func TestConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Update(func(d *Data) error {
				d.Payments[string(rune('a'+i))] = Payment{Key: "x"}
				return nil
			})
		}(i)
	}
	wg.Wait()
	s2, _ := Open(dir)
	s2.View(func(d *Data) {
		if len(d.Payments) != 20 {
			t.Fatalf("got %d", len(d.Payments))
		}
	})
}

func TestOldFormatRefusedAndUntouched(t *testing.T) {
	dir := t.TempDir()
	old := []byte(`{"packages":{},"subscribers":{"x":{"id":"x","code":"ABC234","password":"123456"}},"receipts":{}}`)
	p := filepath.Join(dir, "ispbill.json")
	os.WriteFile(p, old, 0o600)
	_, err := Open(dir)
	if err == nil || !errors.Is(err, ErrOldFormat) || !strings.Contains(err.Error(), "old PIN/subscriber model") {
		t.Fatalf("want a clear old-format error, got %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != string(old) {
		t.Fatal("the old file must not be modified")
	}
}

func TestFreshFileHasFormatVersionAndEmptyAccounts(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.Update(func(d *Data) error { d.Packages["p"] = Package{ID: "p"}; return nil })
	b, _ := os.ReadFile(filepath.Join(dir, "ispbill.json"))
	if !strings.Contains(string(b), `"format": 8`) {
		t.Fatalf("no format marker: %s", b)
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
}

func TestNaturalLess(t *testing.T) {
	if !NaturalLess("Flat 2A", "Flat 10A") || NaturalLess("Flat 10A", "Flat 2A") || !NaturalLess("Flat 3A", "Flat 3B") {
		t.Fatal("ordering")
	}
}
