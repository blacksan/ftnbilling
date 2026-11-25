package olt

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxLineCols bounds one rendered line: a hostile or broken stream cannot make the renderer allocate without limit.
const maxLineCols = 1024

// render is a small line-oriented terminal: it replays what an SSH/Telnet CLI printed and returns the text a screen
// would show. The OLT lines its tables up with cursor movement (ESC[<n>C) instead of spaces and redraws prompts with
// carriage returns, so stripping the escapes would glue columns together or keep text that was overwritten.
//
// Handled per line (the cursor column is kept per line, lines never scroll back): printable runes (overwriting what is
// under the cursor), CR (column 0), BS and DEL (one column left), ESC[nC / ESC[nD (right / left), ESC[nG and ESC[n`
// (absolute column), ESC[K / ESC[0K / ESC[1K / ESC[2K (erase), ESC[nX (erase n), ESC[nP (delete n) and ESC[n@ (insert n
// blanks). Colour (m) and every other CSI sequence, and every other escape, is dropped.
func render(b []byte) string {
	var out strings.Builder
	out.Grow(len(b))
	line := make([]rune, 0, 128)
	col := 0
	flush := func() {
		out.WriteString(strings.TrimRight(string(line), " "))
		out.WriteByte('\n')
		line, col = line[:0], 0
	}
	put := func(r rune) {
		if col >= maxLineCols {
			return
		}
		for len(line) < col {
			line = append(line, ' ')
		}
		if col < len(line) {
			line[col] = r
		} else {
			line = append(line, r)
		}
		col++
	}
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '\n':
			flush()
			i++
		case c == '\r':
			col = 0
			i++
		case c == 8 || c == 127:
			if col > 0 {
				col--
			}
			i++
		case c == 0x1b:
			i = escape(b, i, &line, &col)
		case c == '\t':
			put('\t')
			i++
		case c < 0x20:
			i++
		default:
			r, n := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && n <= 1 {
				n = 1
				r = rune(c) // a stray byte: keep it as Latin-1 rather than lose it
			}
			put(r)
			i += n
		}
	}
	// The last, unfinished line (the prompt) keeps its trailing blanks.
	out.WriteString(string(line))
	return out.String()
}

// escape handles the escape sequence at b[i] and returns the index after it.
func escape(b []byte, i int, line *[]rune, col *int) int {
	if i+1 >= len(b) {
		return len(b) // cut off: the stream is rendered again as more of it arrives
	}
	switch b[i+1] {
	case '[':
		j := i + 2
		for j < len(b) && b[j] >= 0x30 && b[j] <= 0x3f {
			j++
		}
		ps := string(b[i+2 : j])
		for j < len(b) && b[j] >= 0x20 && b[j] <= 0x2f {
			j++
		}
		if j >= len(b) {
			return len(b)
		}
		csi(b[j], ps, line, col)
		return j + 1
	case '(', ')', '*', '+', '#', '%':
		if i+2 >= len(b) {
			return len(b)
		}
		return i + 3
	default:
		return i + 2 // ESC = , ESC > , ESC M ...
	}
}

// param is the first numeric parameter of a CSI, or def when it is absent.
func param(ps string, def int) int {
	if strings.HasPrefix(ps, "?") {
		return def
	}
	if k := strings.IndexByte(ps, ';'); k >= 0 {
		ps = ps[:k]
	}
	n, err := strconv.Atoi(ps)
	if err != nil || n < 0 {
		return def
	}
	if n > maxLineCols {
		n = maxLineCols
	}
	return n
}

func csi(final byte, ps string, line *[]rune, col *int) {
	l := *line
	switch final {
	case 'C': // cursor forward
		*col += max(param(ps, 1), 1)
		if *col > maxLineCols {
			*col = maxLineCols
		}
	case 'D': // cursor back
		*col -= max(param(ps, 1), 1)
		if *col < 0 {
			*col = 0
		}
	case 'G', '`': // absolute column, 1-based
		*col = min(max(param(ps, 1), 1)-1, maxLineCols)
	case 'K':
		switch param(ps, 0) {
		case 0:
			if *col < len(l) {
				l = l[:*col]
			}
		case 1:
			for k := 0; k < len(l) && k <= *col; k++ {
				l[k] = ' '
			}
		case 2:
			l = l[:0]
		}
	case 'X': // erase n characters from the cursor
		n := max(param(ps, 1), 1)
		for k := *col; k < len(l) && k < *col+n; k++ {
			l[k] = ' '
		}
	case 'P': // delete n characters
		n := max(param(ps, 1), 1)
		if *col < len(l) {
			if *col+n >= len(l) {
				l = l[:*col]
			} else {
				l = append(l[:*col], l[*col+n:]...)
			}
		}
	case '@': // insert n blanks
		n := min(max(param(ps, 1), 1), maxLineCols)
		if *col < len(l) && len(l)+n <= maxLineCols {
			ins := make([]rune, n)
			for k := range ins {
				ins[k] = ' '
			}
			l = append(l[:*col], append(ins, l[*col:]...)...)
		}
	}
	*line = l
}

// visible shows the control characters of raw terminal output so that a layout problem can be diagnosed from text:
// ESC becomes \x1b, CR \r, BS \b, other control bytes \xNN; line feeds stay line breaks. The result is capped.
func visible(b []byte, limit int) string {
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c == '\n':
			sb.WriteByte('\n')
		case c == '\r':
			sb.WriteString(`\r`)
		case c == 8:
			sb.WriteString(`\b`)
		case c == '\t':
			sb.WriteString(`\t`)
		case c < 0x20 || c == 0x7f:
			sb.WriteString(`\x`)
			sb.WriteString(strconv.FormatUint(uint64(c)+0x100, 16)[1:])
		default:
			sb.WriteByte(c)
		}
		if sb.Len() >= limit {
			sb.WriteString("...")
			break
		}
	}
	return sb.String()
}
