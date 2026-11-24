package olt

import (
	"fmt"
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
func ParseAutoFind(out string) []Found { return ParseAutoFindOn(0, out) }

// ParseAutoFindOn is ParseAutoFind for the output of "show onu auto-find" run inside a PON interface: a line that does
// not name its PON port belongs to pon.
func ParseAutoFindOn(pon0 int, out string) []Found {
	var res []Found
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		m := serialRE.FindStringSubmatch(l)
		if m == nil || strings.HasPrefix(strings.TrimSpace(l), "-") {
			continue
		}
		pon := pon0
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

// VersionInfo is what "show version" printed:
//
//	Olt Serial Number:           V2609170360
//	Olt Device Model:            V1600GS-R
//	Hardware Version:            V7.1.1
//	Software Version:            V1.0.5
type VersionInfo struct {
	Model, Serial, Hardware, Software, Built string
	Raw                                      string // the first lines, when no field was recognised
}

// Summary is one line for the status text.
func (v VersionInfo) Summary() string {
	if v.Model == "" && v.Software == "" && v.Serial == "" {
		return clip(v.Raw, 200)
	}
	var p []string
	if v.Model != "" {
		p = append(p, v.Model)
	}
	if v.Hardware != "" {
		p = append(p, "hw "+v.Hardware)
	}
	if v.Software != "" {
		p = append(p, "sw "+v.Software)
	}
	if v.Serial != "" {
		p = append(p, "serial "+v.Serial)
	}
	return strings.Join(p, ", ")
}

// ParseVersion reads "show version". Unknown layouts keep their first lines in Raw.
func ParseVersion(out string) VersionInfo {
	var v VersionInfo
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "---") {
			continue
		}
		if len(keep) < 3 {
			keep = append(keep, l)
		}
		k, val, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.Join(strings.Fields(k), " ")) {
		case "olt serial number", "serial number":
			v.Serial = val
		case "olt device model", "device model", "model":
			v.Model = val
		case "hardware version":
			v.Hardware = val
		case "software version":
			v.Software = val
		case "software created time":
			v.Built = val
		}
	}
	v.Raw = strings.Join(keep, " | ")
	return v
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
	return strings.Contains(l, "onuindex") || strings.Contains(l, "admin state") || strings.Contains(l, "phase") || strings.Contains(l, "onu id") || strings.HasPrefix(l, "onu ") ||
		strings.Contains(l, "equipment") || strings.Contains(l, "auth mode") || strings.Contains(l, "authmode")
}

// ParseONUInfo reads "show onu info" of one PON port:
//
//	GPON0/1:1  V414   default   sn   VSOL00127e0f
//
// (ONU index, equipment id, profile, auth mode, serial). The serial is returned in the inventory's form (upper case).
// Lines that look like rows but cannot be understood go to unparsed.
func ParseONUInfo(pon int, out string) (rows []store.OLTONU, unparsed string) {
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
		rows = append(rows, r)
	}
	return rows, strings.Join(bad, "\n")
}

// MergeState adds the states of a state table to rows (matched by ONU id); a state row with no info row becomes a row.
// The state table's layout is not known well, so it is read tolerantly (ParseONUState) and what cannot be read is
// returned as unparsed.
func MergeState(rows []store.OLTONU, pon int, out string) ([]store.OLTONU, string) {
	st, bad := ParseONUState(pon, out)
	for _, s := range st {
		found := false
		for i := range rows {
			if rows[i].PON == s.PON && rows[i].ID == s.ID {
				rows[i].State, rows[i].Online = s.State, s.Online
				if rows[i].Serial == "" {
					rows[i].Serial = s.Serial
				}
				found = true
			}
		}
		if !found {
			rows = append(rows, s)
		}
	}
	return rows, bad
}

var kvRE = regexp.MustCompile(`^\s*([^:]+?)\s*:\s*(.*?)\s*$`)

// MergeOpticalBlocks reads the per-ONU key : value blocks of "show onu optical-info":
//
//	ONU ID                     : 1
//	Rx optical level(ONU)      : -8.94
//	Tx optical level           : 2.55
//	Power feed voltage         : 3.30(V)
//	Laser bias current         : 8.160(mA)
//	Temperature                : 24.465(C)
//
// It returns false when the text has no such block. Threshold lines and the like are ignored; an ONU id with no row
// is returned in unparsed.
func MergeOpticalBlocks(rows []store.OLTONU, pon int, out string) (ok bool, unparsed string) {
	idx := -1
	var bad []string
	for _, l := range strings.Split(out, "\n") {
		m := kvRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		key := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
		val := m[2]
		num := numRE.FindString(val)
		if key == "onu id" {
			ok = true
			idx = -1
			id, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil {
				bad = append(bad, strings.TrimSpace(l))
				continue
			}
			for i := range rows {
				if rows[i].PON == pon && rows[i].ID == id {
					idx = i
				}
			}
			if idx < 0 {
				bad = append(bad, fmt.Sprintf("optical info of ONU %d, which the ONU table does not list", id))
			}
			continue
		}
		if idx < 0 || num == "" {
			continue
		}
		switch {
		case strings.HasPrefix(key, "rx optical level"):
			rows[idx].RxPower = num
		case strings.HasPrefix(key, "tx optical level"):
			rows[idx].TxPower = num
		case key == "power feed voltage":
			rows[idx].Voltage = num
		case key == "laser bias current":
			rows[idx].Bias = num
		case key == "temperature":
			rows[idx].Temp = num
		}
	}
	return ok, strings.Join(bad, "\n")
}

// MergeDistance adds the ranging distance of "show onu distance" to rows. Its layout is not known (TODO(bench)): a
// line with an ONU index and a whole number gives that ONU's distance; nothing else is reported, because the table is
// optional.
func MergeDistance(rows []store.OLTONU, pon int, out string) {
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		p := ponRE.FindStringSubmatch(t)
		if p == nil || p[2] == "" || p[2] == "-" || p[2] == "--" {
			continue
		}
		id, _ := strconv.Atoi(p[2])
		rest := t[strings.Index(t, p[0])+len(p[0]):]
		var dist string
		for _, f := range strings.Fields(rest) {
			if n := strings.TrimSuffix(strings.ToLower(f), "m"); n != "" && strings.Trim(n, "0123456789") == "" {
				dist = n
			}
		}
		if dist == "" {
			continue
		}
		for i := range rows {
			if rows[i].PON == pon && rows[i].ID == id {
				rows[i].Distance = dist
			}
		}
	}
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
	if ok, bad := MergeOpticalBlocks(rows, pon, out); ok {
		return bad
	}
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
	Lines   []string // every line about this ONU in the PON interface (Norm form)
}

// Running is the parts of "show running-config" the planner needs. Lines are in the Norm form.
type Running struct {
	Global []string            // lines outside any block
	Blocks map[string][]string // header -> member lines (the header is in Norm form)
	VLANs  map[int]bool
	ONUs   map[int][]RunONU // PON port -> authorised ONUs
	// AutoLearn and PlugPlay: PON ports on which the OLT authorises ONUs by itself ("onu auto-learn",
	// "plug_and_play enable ...").
	AutoLearn, PlugPlay map[int]bool
	Text                string
}

var (
	headerRE = regexp.MustCompile(`^(interface|vlan|profile)\b`)
	// "profile dba id 10 name dba_1g" is the block "profile dba name dba_1g" whatever the id.
	profHdrRE = regexp.MustCompile(`^profile (\S+)\b.*?\bname (\S+)`)
	onuRE     = regexp.MustCompile(`^onu (?:add )?(\d+)\b`)
	profRE    = regexp.MustCompile(`^onu add \d+ profile (\S+)`)
	gponRE    = regexp.MustCompile(`^interface gpon 0/(\d+)$`)
)

// ParseRunning reads a running configuration. A block starts at an "interface", "vlan" or "profile" line and ends at
// "exit", "!" or the next such line; indentation is ignored.
func ParseRunning(text string) *Running {
	r := &Running{Blocks: map[string][]string{}, VLANs: map[int]bool{}, ONUs: map[int][]RunONU{}, AutoLearn: map[int]bool{}, PlugPlay: map[int]bool{}, Text: text}
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
			if m := profHdrRE.FindStringSubmatch(l); m != nil {
				cur = "profile " + m[1] + " name " + m[2]
			}
			if _, ok := r.Blocks[cur]; !ok {
				r.Blocks[cur] = nil
			}
			if f := strings.Fields(l); f[0] == "vlan" && len(f) > 1 {
				arg := f[1]
				if len(f) == 4 && f[2] == "to" { // "vlan 99 to 100"
					arg = f[1] + "-" + f[3]
				}
				for _, v := range vlanList(arg) {
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
			pon, _ := strconv.Atoi(m[1])
			switch {
			case l == "onu auto-learn":
				r.AutoLearn[pon] = true
			case strings.HasPrefix(l, "plug_and_play enable"):
				r.PlugPlay[pon] = true
			}
			if o := onuRE.FindStringSubmatch(l); o != nil {
				id, _ := strconv.Atoi(o[1])
				ro := RunONU{ID: id, Lines: []string{l}}
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
			r.ONUs[pon][i].Lines = append(r.ONUs[pon][i].Lines, o.Lines...)
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

// Findings are plain statements about the OLT's own settings that an owner should know, read from its running
// configuration. They are never secrets and nothing is changed because of them.
func (r *Running) Findings() []string {
	var out []string
	var pons []int
	for p := range r.AutoLearn {
		pons = append(pons, p)
	}
	for p := range r.PlugPlay {
		if !r.AutoLearn[p] {
			pons = append(pons, p)
		}
	}
	sort.Ints(pons)
	for _, p := range pons {
		var what string
		switch {
		case r.AutoLearn[p] && r.PlugPlay[p]:
			what = "auto-learn and plug-and-play are"
		case r.PlugPlay[p]:
			what = "plug-and-play is"
		default:
			what = "auto-learn is"
		}
		out = append(out, fmt.Sprintf("Auto-learn is on: the OLT accepts any ONU by itself (PON %d: %s on).", p, what))
	}
	var pub, priv, snmpBlocked, telnetDenied, sshd bool
	var loginPW, enablePW string
	for _, l := range r.Global {
		f := strings.Fields(l)
		switch {
		case strings.HasPrefix(l, "snmp-server community public"):
			pub = true
		case strings.HasPrefix(l, "snmp-server community private"):
			priv = true
		case strings.HasPrefix(l, "login-access-list deny snmp"):
			snmpBlocked = true
		case strings.HasPrefix(l, "login-access-list deny telnet"):
			telnetDenied = true
		case l == "sshd enable":
			sshd = true
		case strings.HasPrefix(l, "user login-password "):
			loginPW = strings.Join(f[2:], " ")
		case strings.HasPrefix(l, "user enable-password "):
			enablePW = strings.Join(f[2:], " ")
		}
	}
	if pub || priv {
		msg := "SNMP still has the default communities (public read-only, private read-write)"
		if snmpBlocked {
			msg += "; SNMP access is blocked by the login access list for now"
		}
		out = append(out, msg+".")
	}
	if telnetDenied {
		out = append(out, "Telnet to the OLT is denied by its login access list; SSH is the way in.")
	}
	if sshd {
		out = append(out, "The OLT's SSH server is enabled.")
	}
	if loginPW != "" && loginPW == enablePW {
		out = append(out, "The OLT's login password and enable password are the same.")
	}
	return out
}
