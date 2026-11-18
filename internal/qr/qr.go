// Package qr draws QR codes. It is a small encoder for short texts (byte mode,
// error correction level M, versions 1 to 10, so up to 213 bytes), written with
// the standard library only, for the two-factor enrolment page: the otpauth://
// address of an authenticator is about 120 bytes.
package qr

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
)

// ErrTooLong means the text does not fit in a version 10 symbol.
var ErrTooLong = errors.New("qr: text too long")

type vinfo struct {
	ec     int // error correction codewords per block
	g1, d1 int // blocks in group 1 and data codewords in each
	g2, d2 int // blocks in group 2 and data codewords in each
}

// Level M, versions 1..10 (index = version).
var vtab = [...]vinfo{
	{}, {10, 1, 16, 0, 0}, {16, 1, 28, 0, 0}, {26, 1, 44, 0, 0}, {18, 2, 32, 0, 0}, {24, 2, 43, 0, 0},
	{16, 4, 27, 0, 0}, {18, 4, 31, 0, 0}, {22, 2, 38, 2, 39}, {22, 3, 36, 2, 37}, {26, 4, 43, 1, 44},
}

var alignPos = [...][]int{nil, nil, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50}}

func (v vinfo) dataCodewords() int { return v.g1*v.d1 + v.g2*v.d2 }

// Code is an encoded symbol. Modules[y][x] is true for a dark module.
type Code struct {
	Size    int
	Version int
	Modules [][]bool
}

// Encode makes the symbol for text.
func Encode(text string) (*Code, error) {
	data := []byte(text)
	ver := 0
	for v := 1; v <= 10; v++ {
		cc := 8
		if v >= 10 {
			cc = 16
		}
		if 4+cc+8*len(data) <= 8*vtab[v].dataCodewords() {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, ErrTooLong
	}
	vi := vtab[ver]
	// Bit stream: mode 0100, count, bytes, terminator, padding.
	var bb bitBuf
	bb.put(0b0100, 4)
	if ver >= 10 {
		bb.put(uint(len(data)), 16)
	} else {
		bb.put(uint(len(data)), 8)
	}
	for _, b := range data {
		bb.put(uint(b), 8)
	}
	capBits := 8 * vi.dataCodewords()
	term := capBits - bb.n
	if term > 4 {
		term = 4
	}
	bb.put(0, term)
	for bb.n%8 != 0 {
		bb.put(0, 1)
	}
	cw := bb.bytes()
	for pad := byte(0xEC); len(cw) < vi.dataCodewords(); {
		cw = append(cw, pad)
		if pad == 0xEC {
			pad = 0x11
		} else {
			pad = 0xEC
		}
	}
	all := interleave(cw, vi)

	size := 17 + 4*ver
	c := &Code{Size: size, Version: ver, Modules: make([][]bool, size)}
	fn := make([][]bool, size)
	for i := range c.Modules {
		c.Modules[i] = make([]bool, size)
		fn[i] = make([]bool, size)
	}
	set := func(x, y int, dark bool) { c.Modules[y][x], fn[y][x] = dark, true }
	for i := 0; i < size; i++ { // timing
		set(6, i, i%2 == 0)
		set(i, 6, i%2 == 0)
	}
	finder := func(cx, cy int) {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= size || y >= size {
					continue
				}
				d := max(abs(dx), abs(dy))
				set(x, y, d != 2 && d != 4)
			}
		}
	}
	finder(3, 3)
	finder(size-4, 3)
	finder(3, size-4)
	ap := alignPos[ver]
	for i, py := range ap {
		for j, px := range ap {
			if (i == 0 && j == 0) || (i == 0 && j == len(ap)-1) || (i == len(ap)-1 && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					set(px+dx, py+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	// Reserve the format and version areas (real bits are drawn below).
	c.drawFormat(fn, 0)
	if ver >= 7 {
		rem := ver
		for i := 0; i < 12; i++ {
			rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
		}
		bits := ver<<12 | rem
		for i := 0; i < 18; i++ {
			bit := bits>>i&1 == 1
			a, b := size-11+i%3, i/3
			set(a, b, bit)
			set(b, a, bit)
		}
	}
	// Codewords, zigzag from the bottom right.
	i := 0
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = size - 1 - vert
				}
				if !fn[y][x] && i < len(all)*8 {
					c.Modules[y][x] = all[i>>3]>>(7-uint(i&7))&1 == 1
					i++
				}
			}
		}
	}
	// Pick the mask with the lowest penalty.
	best, bestPen := 0, -1
	for m := 0; m < 8; m++ {
		c.applyMask(fn, m)
		c.drawFormat(fn, m)
		if p := c.penalty(); bestPen < 0 || p < bestPen {
			best, bestPen = m, p
		}
		c.applyMask(fn, m) // undo
	}
	c.applyMask(fn, best)
	c.drawFormat(fn, best)
	return c, nil
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

func (c *Code) drawFormat(fn [][]bool, mask int) {
	size := c.Size
	set := func(x, y int, dark bool) { c.Modules[y][x], fn[y][x] = dark, true }
	data := 0<<3 | mask // level M = 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>i&1 == 1 }
	for i := 0; i <= 5; i++ {
		set(8, i, bit(i))
	}
	set(8, 7, bit(6))
	set(8, 8, bit(7))
	set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		set(size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		set(8, size-15+i, bit(i))
	}
	set(8, size-8, true)
}

func (c *Code) applyMask(fn [][]bool, m int) {
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if fn[y][x] {
				continue
			}
			var inv bool
			switch m {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			default:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv {
				c.Modules[y][x] = !c.Modules[y][x]
			}
		}
	}
}

// penalty scores a masked symbol with the four rules of the standard.
func (c *Code) penalty() int {
	n := c.Size
	m := c.Modules
	p := 0
	// Rule 1: runs of five or more of one colour, in rows and columns.
	for dir := 0; dir < 2; dir++ {
		for a := 0; a < n; a++ {
			run := 1
			for b := 1; b < n; b++ {
				cur, prev := m[a][b], m[a][b-1]
				if dir == 1 {
					cur, prev = m[b][a], m[b-1][a]
				}
				if cur == prev {
					run++
					if run == 5 {
						p += 3
					} else if run > 5 {
						p++
					}
				} else {
					run = 1
				}
			}
		}
	}
	// Rule 2: 2x2 blocks of one colour.
	for y := 0; y+1 < n; y++ {
		for x := 0; x+1 < n; x++ {
			v := m[y][x]
			if v == m[y][x+1] && v == m[y+1][x] && v == m[y+1][x+1] {
				p += 3
			}
		}
	}
	// Rule 3: 1:1:3:1:1 finder-like patterns with four light modules on a side.
	pa := []bool{true, false, true, true, true, false, true, false, false, false, false}
	pb := []bool{false, false, false, false, true, false, true, true, true, false, true}
	match := func(get func(i int) bool, pat []bool) bool {
		for i, w := range pat {
			if get(i) != w {
				return false
			}
		}
		return true
	}
	for a := 0; a < n; a++ {
		for b := 0; b+11 <= n; b++ {
			for dir := 0; dir < 2; dir++ {
				get := func(i int) bool {
					if dir == 0 {
						return m[a][b+i]
					}
					return m[b+i][a]
				}
				if match(get, pa) {
					p += 40
				}
				if match(get, pb) {
					p += 40
				}
			}
		}
	}
	// Rule 4: balance of dark and light.
	dark := 0
	for y := range m {
		for _, v := range m[y] {
			if v {
				dark++
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10) + total - 1) / total
	p += 10 * (k - 1)
	return p
}

// ---- bits and Reed-Solomon ------------------------------------------------------

type bitBuf struct {
	b []byte
	n int
}

func (w *bitBuf) put(v uint, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.b[w.n/8] |= 0x80 >> uint(w.n%8)
		}
		w.n++
	}
}

func (w *bitBuf) bytes() []byte { return append([]byte(nil), w.b...) }

var gfExp [512]byte
var gfLog [256]int

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = i
		x <<= 1
		if x >= 256 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[gfLog[a]+gfLog[b]]
}

// rsRemainder returns the n error correction codewords of data.
func rsRemainder(data []byte, n int) []byte {
	gen := []byte{1} // coefficients, highest degree first, monic
	for i := 0; i < n; i++ {
		next := make([]byte, len(gen)+1)
		for j, g := range gen {
			next[j] ^= g
			next[j+1] ^= gfMul(g, gfExp[i])
		}
		gen = next
	}
	rem := make([]byte, n)
	for _, d := range data {
		f := d ^ rem[0]
		copy(rem, rem[1:])
		rem[n-1] = 0
		if f != 0 {
			for j := 0; j < n; j++ {
				rem[j] ^= gfMul(gen[j+1], f)
			}
		}
	}
	return rem
}

func interleave(data []byte, v vinfo) []byte {
	type blk struct{ d, e []byte }
	var blocks []blk
	pos := 0
	add := func(count, size int) {
		for i := 0; i < count; i++ {
			d := data[pos : pos+size]
			pos += size
			blocks = append(blocks, blk{d, rsRemainder(d, v.ec)})
		}
	}
	add(v.g1, v.d1)
	add(v.g2, v.d2)
	var out []byte
	maxD := v.d1
	if v.d2 > maxD {
		maxD = v.d2
	}
	for i := 0; i < maxD; i++ {
		for _, b := range blocks {
			if i < len(b.d) {
				out = append(out, b.d[i])
			}
		}
	}
	for i := 0; i < v.ec; i++ {
		for _, b := range blocks {
			out = append(out, b.e[i])
		}
	}
	return out
}

// PNG draws the symbol with a four module quiet zone; scale is pixels per module.
func (c *Code) PNG(scale int) []byte {
	if scale < 1 {
		scale = 1
	}
	const quiet = 4
	px := (c.Size + 2*quiet) * scale
	img := image.NewGray(image.Rect(0, 0, px, px))
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if !c.Modules[y][x] {
				continue
			}
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					img.SetGray((x+quiet)*scale+dx, (y+quiet)*scale+dy, color.Gray{0})
				}
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
