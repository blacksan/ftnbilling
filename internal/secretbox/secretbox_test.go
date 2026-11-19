package secretbox

import (
	"bytes"
	"testing"
)

func TestRoundTripAndLabels(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	a, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSettings(key)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := a.Seal("hello")
	if p, err := a.Open(ct); err != nil || p != "hello" {
		t.Fatalf("round trip: %q %v", p, err)
	}
	if _, err := s.Open(ct); err == nil {
		t.Fatal("a value sealed by the national-ID box must not open in the settings box")
	}
	if a.Index("x") == s.Index("x") {
		t.Fatal("index keys must differ by label")
	}
	if _, err := New(make([]byte, 5)); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestAADBinding(t *testing.T) {
	b, _ := NewSettings(bytes.Repeat([]byte{1}, 32))
	ct, err := b.SealAAD("secret", []byte("wa.token"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := b.OpenAAD(ct, []byte("wa.token")); err != nil || p != "secret" {
		t.Fatalf("%q %v", p, err)
	}
	if _, err := b.OpenAAD(ct, []byte("mpesa.secret")); err == nil {
		t.Fatal("ciphertext opened under a different key name")
	}
	if _, err := b.Open(ct); err == nil {
		t.Fatal("ciphertext opened without its AAD")
	}
	if _, err := b.OpenAAD("!!notbase64", nil); err == nil {
		t.Fatal("damaged value accepted")
	}
}
