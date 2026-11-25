package store

import (
	"sort"
	"strings"
	"time"
)

// ONU parameter tree: the TR-069 parameter names (and, where read, values) the ACS found on an ONU by walking its
// data model (GetParameterNames, then GetParameterValues in chunks), and the real vendor paths resolved from them
// (optical levels, WAN counters, hosts, Wi-Fi). One current tree per ONU, replaced on every walk (key: the ONU id).
//
// The tree is kept as one text blob, one line per parameter ("path<TAB>flags<TAB>value"), so that a few thousand
// parameters cost little memory in the in-memory store. A value is kept only for a parameter whose name is not
// secret (passphrases, passwords, keys): those are listed by path with the value withheld, and never read.
const (
	MaxTreeParams = 3000 // parameters kept per ONU (a pathological box cannot grow the row without limit)
	MaxTreeValue  = 160  // bytes of one stored value
	MaxTreeGaps   = 32
)

// TreeParam is one parameter of the tree.
type TreeParam struct {
	Path     string
	Writable bool
	Secret   bool   // the value is withheld (and was never read)
	Value    string // "" = not read (or empty)
}

// TreeResolved is what the resolver found in the tree: the real path of each thing the server wants to read.
// Paths is keyed by category ("optical.rx", "wan.rx_bytes", "hosts.table", "wifi.passphrase", ...); a category that
// has no matching parameter in the tree is not in Paths: it is listed in Gaps (a path is never made up).
type TreeResolved struct {
	Paths map[string][]string `json:"paths,omitempty"`
	Gaps  []string            `json:"gaps,omitempty"`
	Notes []string            `json:"notes,omitempty"`
}

// One returns the first path of a category ("" when there is none).
func (r TreeResolved) One(key string) string {
	if p := r.Paths[key]; len(p) > 0 {
		return p[0]
	}
	return ""
}

// ONUTree is the record of one ONU.
type ONUTree struct {
	ID        string       `json:"id"` // the ONU id
	WalkedAt  time.Time    `json:"walked_at"`
	Root      string       `json:"root,omitempty"`
	SWVersion string       `json:"sw_version,omitempty"` // what the ONU reported when it was walked
	HWVersion string       `json:"hw_version,omitempty"`
	Reason    string       `json:"reason,omitempty"` // why it was walked: first | version | reset | marked
	Count     int          `json:"count"`
	Truncated bool         `json:"truncated,omitempty"` // the walk stopped at a limit (count, requests or time)
	Text      string       `json:"text,omitempty"`
	Resolved  TreeResolved `json:"resolved"`
}

func (x ONUTree) norm() ONUTree {
	x.WalkedAt = nt(x.WalkedAt)
	if len(x.Resolved.Paths) == 0 {
		x.Resolved.Paths = nil
	}
	if len(x.Resolved.Gaps) == 0 {
		x.Resolved.Gaps = nil
	}
	if len(x.Resolved.Notes) == 0 {
		x.Resolved.Notes = nil
	}
	return x
}

// Norm is the form the store keeps (for tests).
func (x ONUTree) Norm() ONUTree { return x.norm() }

// Clone copies the maps and slices.
func (x ONUTree) Clone() ONUTree { return cloneONUTree(x) }

func cloneONUTree(x ONUTree) ONUTree {
	if x.Resolved.Paths != nil {
		m := make(map[string][]string, len(x.Resolved.Paths))
		for k, v := range x.Resolved.Paths {
			m[k] = append([]string(nil), v...)
		}
		x.Resolved.Paths = m
	}
	x.Resolved.Gaps = append([]string(nil), x.Resolved.Gaps...)
	x.Resolved.Notes = append([]string(nil), x.Resolved.Notes...)
	return x
}

// ONUTreeOf returns the tree of an ONU.
func (d *Data) ONUTreeOf(id string) (ONUTree, bool) {
	t, ok := d.ONUTrees[id]
	return t, ok
}

// cleanTreeText makes a path or value safe for the line format: no tabs, newlines or other control characters.
func cleanTreeText(s string, max int) string {
	if len(s) > max {
		s = s[:max]
		for len(s) > 0 && s[len(s)-1] >= 0x80 && s[len(s)-1] < 0xC0 { // do not cut a UTF-8 sequence in half
			s = s[:len(s)-1]
		}
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// EncodeTree turns parameters into the stored text, sorted by path, at most MaxTreeParams of them. It returns the
// text and how many parameters it holds.
func EncodeTree(ps []TreeParam) (string, int) {
	list := make([]TreeParam, 0, len(ps))
	seen := map[string]bool{}
	for _, p := range ps {
		path := cleanTreeText(p.Path, 256)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		p.Path = path
		list = append(list, p)
	}
	sort.SliceStable(list, func(i, j int) bool { return TreePathLess(list[i].Path, list[j].Path) })
	if len(list) > MaxTreeParams {
		list = list[:MaxTreeParams]
	}
	var b strings.Builder
	for _, p := range list {
		flags := ""
		if p.Writable {
			flags += "w"
		}
		if p.Secret {
			flags += "s"
		}
		if flags == "" {
			flags = "-"
		}
		v := ""
		if !p.Secret {
			v = cleanTreeText(p.Value, MaxTreeValue)
		}
		b.WriteString(p.Path + "\t" + flags + "\t" + v + "\n")
	}
	return b.String(), len(list)
}

// Params decodes the stored text.
func (x ONUTree) Params() []TreeParam {
	var out []TreeParam
	for _, line := range strings.Split(x.Text, "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 2 {
			continue
		}
		p := TreeParam{Path: f[0], Writable: strings.Contains(f[1], "w"), Secret: strings.Contains(f[1], "s")}
		if len(f) == 3 && !p.Secret {
			p.Value = f[2]
		}
		out = append(out, p)
	}
	return out
}

// TreePathLess orders parameter paths segment by segment, numbers (table instances) by value.
func TreePathLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		an, aok := atoiSeg(as[i])
		bn, bok := atoiSeg(bs[i])
		if aok && bok {
			return an < bn
		}
		return as[i] < bs[i]
	}
	return len(as) < len(bs)
}

func atoiSeg(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
