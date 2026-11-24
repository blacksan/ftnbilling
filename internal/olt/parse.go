package olt

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ispbill/internal/store"
)

// Everything here reads what the OLT printed, defensively: the real table layouts are not yet known (see the dialect
// file), so a row that cannot be understood is kept as raw text for the owner instead of being guessed.

var (
	serialRE = regexp.MustCompile(`\b([A-Za-z]{4}[0-9A-Fa-f]{8})\b`)
	ponRE    = regexp.MustCompile(`(?i)\b(?:gpon|pon)?\s*0/(\d+)(?::(\d+|--|-))?`)
	numRE    = regexp.MustCompile(`-?\d+(?:\.\d+)?`)
)

// NormSerial is a GPON serial in the form it is compared in.
func NormSerial(s string) string { return store.NormSerial(s) }

// Found is an ONU the OLT's auto-find list shows: a serial on a PON port that is not authorised.
type Found struct {
	PON    int
	Serial string
}

// ParseAutoFind reads "show onu auto-find". A line with a GPON serial gives one entry; the PON port is read from a
// "0/<n>" in the same line (0 when there is none). Entries are unique per PON and serial.
func ParseAutoFind(out string) []Found {
	var res []Found
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		m := serialRE.FindStringSubmatch(l)
		if m == nil || strings.HasPrefix(strings.TrimSpace(l), "-") {
			continue
		}
		pon := 0
		if p := ponRE.FindStringSubmatch(l); p != nil {
			pon, _ = strconv.Atoi(p[1])
		}
		f := Found{PON: pon, Serial: NormSerial(m[1])}
		if k := strconv.Itoa(f.PON) + "|" + f.Serial; !seen[k] {
			seen[k] = true
			res = append(res, f)
		}
	}
	return res
}

// ParseVersion is the first few lines of "show version", trimmed.
func ParseVersion(out string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "---") {
			continue
		}
		keep = append(keep, l)
		if len(keep) == 3 {
			break
		}
	}
	return clip(strings.Join(keep, " | "), 200)
}

var stateWords = []struct {
	re     *regexp.Regexp
	name   string
	online bool
}{
	{regexp.MustCompile(`(?i)\bdy(ing)?[ -]?gasp\b`), "dying-gasp", false},
	{regexp.MustCompile(`(?i)\bworking\b`), "online", true},
	{regexp.MustCompile(`(?i)\bonline\b`), "online", true},
	{regexp.MustCompile(`(?i)\boff-?line\b`), "offline", false},
	{regexp.MustCompile(`(?i)\blos\b`), "los", false},
	{regexp.MustCompile(`(?i)\bauth(entication)?[ -]?fail`), "auth-failed", false},
	{regexp.MustCompile(`(?i)\bunknown\b`), "unknown", false},
}

// ParseONUState reads an ONU state table of one PON port into rows. Lines that look like a row but cannot be
// understood go to unparsed.
func ParseONUState(pon int, out string) (rows []store.OLTONU, unparsed string) {
	var bad []string
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "---") {
			continue
		}
		p := ponRE.FindStringSubmatch(t)
		if p == nil || p[2] == "" || p[2] == "-" || p[2] == "--" {
			if !headerLine(t) {
				bad = append(bad, t)
			}
			continue
		}
		port, _ := strconv.Atoi(p[1])
		id, _ := strconv.Atoi(p[2])
		if port == 0 {
			port = pon
		}
		r := store.OLTONU{PON: port, ID: id}
		if m := serialRE.FindStringSubmatch(t); m != nil {
			r.Serial = NormSerial(m[1])
		}
		for _, w := range stateWords {
			if w.re.MatchString(t) {
				r.State, r.Online = w.name, w.online
				break
			}
		}
		if r.State == "" {
			f := strings.Fields(t)
			r.State = strings.ToLower(f[len(f)-1])
		}
		rows = append(rows, r)
	}
	return rows, strings.Join(bad, "\n")
}

func headerLine(t string) bool {
	l := strings.ToLower(t)
	return strings.Contains(l, "onuindex") || strings.Contains(l, "admin state") || strings.Contains(l, "phase") || strings.Contains(l, "onu id") || strings.HasPrefix(l, "onu ")
}

var (
	rxRE   = regexp.MustCompile(`(?i)\brx[^-\d\n]{0,20}(-?\d+(?:\.\d+)?)`)
	txRE   = regexp.MustCompile(`(?i)\btx[^-\d\n]{0,20}(-?\d+(?:\.\d+)?)`)
	distRE = regexp.MustCompile(`(?i)(?:distance|rtt)[^\d\n]{0,20}(\d+)`)
	downRE = regexp.MustCompile(`(?i)(?:last[ -]?down|down[ -]?cause|offline[ -]?reason)\s*[:=]?\s*([A-Za-z][\w -]{1,30})`)
)

// MergeOptical adds optical power, distance and last-down cause from a table to rows. A row is matched by its ONU id
// ("0/1:3" or a leading number); text no row took is returned.
func MergeOptical(rows []store.OLTONU, pon int, out string) (unparsed string) {
	byID := map[int]int{}
	for i, r := range rows {
		if r.PON == pon {
			byID[r.ID] = i
		}
	}
	var bad []string
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "---") || headerLine(t) {
			continue
		}
		id := -1
		if p := ponRE.FindStringSubmatch(t); p != nil && p[2] != "" && p[2] != "-" && p[2] != "--" {
			id, _ = strconv.Atoi(p[2])
		} else if f := strings.Fields(t); len(f) > 0 {
			if n, err := strconv.Atoi(f[0]); err == nil {
				id = n
			}
		}
		i, ok := byID[id]
		if !ok {
			bad = append(bad, t)
			continue
		}
		if m := rxRE.FindStringSubmatch(t); m != nil {
			rows[i].RxPower = m[1]
		}
		if m := txRE.FindStringSubmatch(t); m != nil {
			rows[i].TxPower = m[1]
		}
		if m := distRE.FindStringSubmatch(t); m != nil {
			rows[i].Distance = m[1]
		}
		if m := downRE.FindStringSubmatch(t); m != nil {
			rows[i].LastDown = strings.TrimSpace(m[1])
		}
		if rows[i].RxPower == "" && rows[i].TxPower == "" {
			// A bare table row: the first negative decimal number is the received power.
			for _, n := range numRE.FindAllString(t, -1) {
				if strings.HasPrefix(n, "-") && strings.Contains(n, ".") {
					rows[i].RxPower = n
					break
				}
			}
		}
	}
	return strings.Join(bad, "\n")
}

// ---- running configuration -----------------------------------------------------------------------------------------

// RunONU is an authorised ONU as the running configuration lists it.
type RunONU struct {
	ID      int
	Serial  string
	Profile string
}

// Running is the parts of "show running-config" the planner needs. Lines are in the Norm form.
type Running struct {
	Global []string            // lines outside any block
	Blocks map[string][]string // header -> member lines (the header is in Norm form)
	VLANs  map[int]bool
	ONUs   map[int][]RunONU // PON port -> authorised ONUs
	Text   string
}

var (
	headerRE = regexp.MustCompile(`^(interface|vlan|profile)\b`)
	onuRE    = regexp.MustCompile(`^onu (?:add )?(\d+)\b`)
	profRE   = regexp.MustCompile(`\bprofile (\S+)`)
	gponRE   = regexp.MustCompile(`^interface gpon 0/(\d+)$`)
)

// ParseRunning reads a running configuration. A block starts at an "interface", "vlan" or "profile" line and ends at
// "exit", "!" or the next such line; indentation is ignored.
func ParseRunning(text string) *Running {
	r := &Running{Blocks: map[string][]string{}, VLANs: map[int]bool{}, ONUs: map[int][]RunONU{}, Text: text}
	cur := ""
	for _, raw := range strings.Split(text, "\n") {
		l := Norm(raw)
		switch {
		case l == "" || l == "!" || strings.HasPrefix(l, "#"):
			continue
		case l == "exit" || l == "end":
			cur = ""
			continue
		case headerRE.MatchString(l):
			cur = l
			if _, ok := r.Blocks[cur]; !ok {
				r.Blocks[cur] = nil
			}
			if f := strings.Fields(l); f[0] == "vlan" && len(f) > 1 {
				for _, v := range vlanList(f[1]) {
					r.VLANs[v] = true
				}
			}
			continue
		}
		if cur == "" {
			r.Global = append(r.Global, l)
			continue
		}
		r.Blocks[cur] = append(r.Blocks[cur], l)
		if m := gponRE.FindStringSubmatch(cur); m != nil {
			if o := onuRE.FindStringSubmatch(l); o != nil {
				pon, _ := strconv.Atoi(m[1])
				id, _ := strconv.Atoi(o[1])
				ro := RunONU{ID: id}
				if strings.Contains(l, " sn ") || strings.Contains(l, "serial") {
					if s := serialRE.FindStringSubmatch(raw); s != nil {
						ro.Serial = NormSerial(s[1])
					}
				}
				if p := profRE.FindStringSubmatch(l); p != nil {
					ro.Profile = p[1]
				}
				r.addONU(pon, ro)
			}
		}
	}
	return r
}

// addONU merges lines about one ONU: "onu add 1 ... sn X" gives the serial, "onu 1 tcont ..." only the id.
func (r *Running) addONU(pon int, o RunONU) {
	for i, e := range r.ONUs[pon] {
		if e.ID == o.ID {
			if o.Serial != "" {
				r.ONUs[pon][i].Serial = o.Serial
			}
			if o.Profile != "" {
				r.ONUs[pon][i].Profile = o.Profile
			}
			return
		}
	}
	r.ONUs[pon] = append(r.ONUs[pon], o)
}

func vlanList(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		if a, b, ok := strings.Cut(part, "-"); ok {
			lo, e1 := strconv.Atoi(a)
			hi, e2 := strconv.Atoi(b)
			if e1 == nil && e2 == nil && hi >= lo && hi-lo < 4096 {
				for v := lo; v <= hi; v++ {
					out = append(out, v)
				}
			}
			continue
		}
		if v, err := strconv.Atoi(part); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// Has reports whether a line is in a block (header "" = outside any block).
func (r *Running) Has(header, line string) bool {
	h, l := Norm(header), Norm(line)
	var set []string
	if h == "" {
		set = r.Global
	} else {
		set = r.Blocks[h]
	}
	for _, x := range set {
		if x == l {
			return true
		}
	}
	return false
}

// HasBlock reports whether a header exists.
func (r *Running) HasBlock(header string) bool { _, ok := r.Blocks[Norm(header)]; return ok }

// ONUBySerial finds an authorised ONU by serial.
func (r *Running) ONUBySerial(serial string) (pon int, o RunONU, ok bool) {
	n := NormSerial(serial)
	for p, l := range r.ONUs {
		for _, e := range l {
			if e.Serial != "" && e.Serial == n {
				return p, e, true
			}
		}
	}
	return 0, RunONU{}, false
}

// UsedIDs lists the ONU ids in use on a PON port.
func (r *Running) UsedIDs(pon int) []int {
	var ids []int
	for _, o := range r.ONUs[pon] {
		ids = append(ids, o.ID)
	}
	sort.Ints(ids)
	return ids
}
