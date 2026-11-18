package qr

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

// The worked example in the QR tutorials (version 1, level M, HELLO WORLD in
// alphanumeric mode): its 16 data codewords have these 10 error codewords.
func TestReedSolomonKnownVector(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := rsRemainder(data, 10); !bytes.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestEncodeSizesAndFinders(t *testing.T) {
	for _, n := range []int{1, 14, 15, 60, 120, 150, 213} {
		c, err := Encode(strings.Repeat("a", n))
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		if c.Size != 17+4*c.Version {
			t.Fatalf("size %d for version %d", c.Size, c.Version)
		}
		// The top-left finder: dark ring, light ring, 3x3 dark centre.
		for i := 0; i < 7; i++ {
			if !c.Modules[0][i] || !c.Modules[6][i] || !c.Modules[i][0] || !c.Modules[i][6] {
				t.Fatalf("%d bytes: finder ring broken", n)
			}
		}
		if c.Modules[1][1] || !c.Modules[3][3] {
			t.Fatalf("%d bytes: finder interior wrong", n)
		}
		if !c.Modules[c.Size-8][8] {
			t.Fatal("dark module missing")
		}
	}
	if _, err := Encode(strings.Repeat("a", 214)); err != ErrTooLong {
		t.Fatalf("214 bytes: %v", err)
	}
}

func TestPNGDecodes(t *testing.T) {
	c, err := Encode("otpauth://totp/Fast%20Tech%20Net:admin?secret=JBSWY3DPEHPK3PXP&issuer=Fast%20Tech%20Net")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(c.PNG(4)))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != (c.Size+8)*4 || b.Dy() != b.Dx() {
		t.Fatalf("bounds %v", b)
	}
}
