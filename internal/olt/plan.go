package olt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Desired is what an OLT should look like, computed from server data (see desired.go). Nothing here talks to an OLT.
type Desired struct {
	InternetVLAN     int
	InternetUntagged bool // the router takes the internet VLAN untagged (its customer VLAN is 0)
	TR069VLAN        int  // 0 = the router has no ONU management network yet
	MgmtVLAN         int
	MgmtIP           string
	MgmtMask         string
	MgmtGateway      string
	Uplink           string
	DBA              string
	DBAMax           int
	LineProfile      string // bound to every ONU
	SrvProfile       string // bound to every ONU
	PriProfile       string // bound to ONUs the plan authorises
	ONUProfile       string // the "onu add ... profile" name (an existing OLT profile)
	// ACS is the address and login ONUs are given for TR-069 (nil: no TR-069 lines are planned).
	ACS *ACS
	// Provisioning is on for this OLT: only then does the plan include turning off the OLT's own auto-learn.
	Provisioning bool
	ONUs         []DesiredONU
	// OnlyONUs leaves the base configuration (VLANs, profiles, uplink, management address) out of the plan: the
	// automatic ONU add changes nothing but ONUs.
	OnlyONUs bool
}

// DesiredONU is one ONU the OLT should have services for. Add means it is not on the OLT yet.
type DesiredONU struct {
	PON         int
	ID          int // 0 = the next free id (the plan fills it in)
	Serial      string
	EquipmentID string
	Label       string // house or apartment, for the page
	Add         bool
}

// Step is one titled group of commands in a plan.
type Step struct {
	Title string
	Lines []string
}

// PlanAdd says which ONU a plan authorises, with the id it picked.
type PlanAdd struct {
	PON         int
	ID          int
	Serial      string
	EquipmentID string
	Label       string
}

// Plan is the exact command list that would bring the OLT to the desired state, with why.
type Plan struct {
	Steps    []Step
	Adds     []PlanAdd
	Notes    []string // things the plan cannot do (the TR-069 login, unknown syntax)
	Warnings []string
}

// Empty reports whether nothing needs doing.
func (p Plan) Empty() bool { return len(p.Commands()) == 0 }

// Commands is every configuration line, in order, as it will be sent.
func (p Plan) Commands() []string {
	var out []string
	for _, s := range p.Steps {
		out = append(out, s.Lines...)
	}
	return out
}

// Hash identifies the command list: an apply names the hash the owner looked at. It is taken over the redacted lines,
// so a secret in a line is not what the hash is made of.
func (p Plan) Hash() string {
	h := sha256.Sum256([]byte(strings.Join(redactAll(p.Commands()), "\n")))
	return hex.EncodeToString(h[:8])
}

// Display is the plan as it may be shown or stored: every secret in a line (the ACS login) is hidden.
func (p Plan) Display() Plan {
	q := p
	q.Steps = make([]Step, len(p.Steps))
	for i, s := range p.Steps {
		q.Steps[i] = Step{Title: s.Title, Lines: redactAll(s.Lines)}
	}
	return q
}

func redactAll(l []string) []string {
	out := make([]string, len(l))
	for i, x := range l {
		out[i] = Redact(x)
	}
	return out
}

// MaxONUID is the highest ONU id on a PON port (GPON: 128 per port on the V1600G).
const MaxONUID = 128

// NextFreeID returns the lowest id from 1 that is not in used.
func NextFreeID(used []int) (int, bool) {
	u := map[int]bool{}
	for _, x := range used {
		u[x] = true
	}
	for i := 1; i <= MaxONUID; i++ {
		if !u[i] {
			return i, true
		}
	}
	return 0, false
}

// BuildPlan compares what the OLT has (run) with what it should have (want) and returns the missing lines. Anything the
// running configuration already has is left alone; nothing is ever removed by a plan.
func (d *Dialect) BuildPlan(want Desired, run *Running) Plan {
	var p Plan
	add := func(title string, lines []string) {
		if len(lines) > 0 {
			p.Steps = append(p.Steps, Step{title, lines})
		}
	}
	if want.OnlyONUs {
		return d.buildONUs(want, run, p)
	}
	// VLANs.
	var vl []string
	seen := map[int]bool{}
	for _, v := range []int{want.InternetVLAN, want.TR069VLAN, want.MgmtVLAN} {
		if v > 1 && !seen[v] && !run.VLANs[v] {
			seen[v] = true
			vl = append(vl, d.VLANLines(v)...)
		}
	}
	add("VLANs", vl)
	// Profiles. Existing ones (the bench OLT has dba_1g, srv_hgu and line_hgu) are used as they are, never edited.
	if want.DBA != "" && !run.HasBlock("profile dba name "+want.DBA) {
		add("DBA profile "+want.DBA, d.DBALines(want.DBA, want.DBAMax))
	}
	if want.LineProfile != "" && !run.HasBlock("profile line name "+want.LineProfile) {
		add("Line profile "+want.LineProfile, d.LineProfileLines(want.LineProfile, want.DBA, want.InternetVLAN, want.TR069VLAN))
	}
	if want.SrvProfile != "" && !run.HasBlock("profile srv name "+want.SrvProfile) {
		add("Service profile "+want.SrvProfile+" (HGU, VEIP)", d.SrvProfileLines(want.SrvProfile))
	}
	if want.PriProfile != "" && !run.HasBlock("profile pri name "+want.PriProfile) {
		add("Pri profile "+want.PriProfile+" (WAN, TR-069)", d.PriProfileLines(want.PriProfile, want.InternetVLAN, want.TR069VLAN, want.ACS))
	}
	// Uplink tagging and PVID.
	if want.Uplink != "" {
		var tagged []int
		untagged := 0
		if want.InternetUntagged {
			untagged = want.InternetVLAN
		} else if want.InternetVLAN > 0 {
			tagged = append(tagged, want.InternetVLAN)
		}
		if want.TR069VLAN > 0 {
			tagged = append(tagged, want.TR069VLAN)
		}
		if want.MgmtVLAN > 0 {
			tagged = append(tagged, want.MgmtVLAN)
		}
		add("Uplink "+want.Uplink, missingBlock(run, d.UplinkLines(want.Uplink, tagged, untagged)))
	}
	// Management address.
	if want.MgmtVLAN > 0 && want.MgmtIP != "" {
		lines := d.MgmtIfaceLines(want.MgmtVLAN, want.MgmtIP, want.MgmtMask, want.MgmtGateway)
		var miss []string
		hdr := lines[0]
		var blk []string
		for _, l := range lines[1:] {
			if Norm(l) == "exit" {
				break
			}
			blk = append(blk, l)
		}
		for _, l := range blk {
			if !run.Has(hdr, l) {
				miss = append(miss, l)
			}
		}
		if len(miss) > 0 {
			add("Management address", append(append([]string{hdr}, miss...), "exit"))
		}
		for _, l := range lines[len(blk)+2:] { // global lines after the block (the default route)
			if !run.Has("", l) {
				add("Management route", []string{l})
			}
		}
	}
	// The OLT's own auto-learn authorises any ONU that plugs in, which defeats "never add an unknown ONU".
	var learn []int
	for pon := range run.AutoLearn {
		learn = append(learn, pon)
	}
	for pon := range run.PlugPlay {
		if !run.AutoLearn[pon] {
			learn = append(learn, pon)
		}
	}
	sort.Ints(learn)
	for _, pon := range learn {
		if want.Provisioning {
			add(fmt.Sprintf("PON %d: stop the OLT authorising ONUs by itself", pon), d.AutoLearnOffLines(pon))
		} else {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Auto-learn is on: the OLT accepts any ONU by itself (PON %d). With provisioning on, the plan turns it off.", pon))
		}
	}
	p = d.buildONUs(want, run, p)
	switch {
	case want.TR069VLAN > 0 && want.ACS != nil:
		p.Notes = append(p.Notes, fmt.Sprintf("TR-069: new ONUs get the Pri profile %s (a TR-069 WAN on VLAN %d and the ACS address and login; the login is hidden here). ONUs already on the OLT get the same as per-ONU lines.", want.PriProfile, want.TR069VLAN))
	case want.TR069VLAN > 0:
		p.Notes = append(p.Notes, "The ACS is not running or has no login yet, so the plan has no TR-069 lines.")
	default:
		p.Notes = append(p.Notes, "This router has no ONU management network (VLAN 99) yet: the ONUs get no TR-069 service until it is set up on the router's ONUs tab.")
	}
	return p
}

// missingBlock returns the block (header ... exit) with only the lines the running config lacks, or nil.
func missingBlock(run *Running, block []string) []string {
	if len(block) < 2 {
		return nil
	}
	hdr := block[0]
	var miss []string
	for _, l := range block[1:] {
		if Norm(l) == "exit" {
			continue
		}
		if !run.Has(hdr, l) {
			miss = append(miss, l)
		}
	}
	if len(miss) == 0 {
		return nil
	}
	return append(append([]string{hdr}, miss...), "exit")
}

// buildONUs adds the ONU steps (authorisations and services) to the plan.
func (d *Dialect) buildONUs(want Desired, run *Running, p Plan) Plan {
	add := func(title string, lines []string) {
		if len(lines) > 0 {
			p.Steps = append(p.Steps, Step{title, lines})
		}
	}
	// ONUs, grouped per PON port.
	used := map[int][]int{}
	for pon, l := range run.ONUs {
		for _, o := range l {
			used[pon] = append(used[pon], o.ID)
		}
	}
	byPON := map[int][]DesiredONU{}
	for _, o := range want.ONUs {
		byPON[o.PON] = append(byPON[o.PON], o)
	}
	var pons []int
	for k := range byPON {
		pons = append(pons, k)
	}
	sort.Ints(pons)
	for _, pon := range pons {
		hdr := d.PONEnter(pon)[0]
		var lines []string
		for _, o := range byPON[pon] {
			id := o.ID
			if _, ro, ok := run.ONUBySerial(o.Serial); ok && !o.Add {
				id = ro.ID
			}
			if o.Add {
				if _, _, on := run.ONUBySerial(o.Serial); on {
					continue // already authorised (somebody else added it meanwhile)
				}
				if id == 0 {
					n, ok := NextFreeID(used[pon])
					if !ok {
						p.Warnings = append(p.Warnings, fmt.Sprintf("PON %d has no free ONU id for %s", pon, o.Serial))
						continue
					}
					id = n
				}
				used[pon] = append(used[pon], id)
				lines = append(lines, d.ONUAddLine(id, want.ONUProfile, o.Serial))
				p.Adds = append(p.Adds, PlanAdd{PON: pon, ID: id, Serial: o.Serial, EquipmentID: o.EquipmentID, Label: o.Label})
				// New ONUs get everything from the profiles: the line profile (T-CONT, gemport, services), the service
				// profile (VEIP) and the Pri profile (internet WAN, TR-069 WAN and ACS login).
				if want.LineProfile != "" {
					lines = append(lines, d.ONUBindLine(id, "line", want.LineProfile))
				}
				if want.SrvProfile != "" {
					lines = append(lines, d.ONUBindLine(id, "srv", want.SrvProfile))
				}
				if want.PriProfile != "" {
					lines = append(lines, d.ONUBindLine(id, "pri", want.PriProfile))
				}
				if descWord(o.Label) != "" {
					lines = append(lines, d.ONUDescLine(id, o.Label))
				}
				continue
			} else if id == 0 {
				continue
			}
			// An ONU that is on the OLT already keeps what it has; only what is missing is added. It is not given the
			// Pri profile (its own WAN lines would be doubled): the TR-069 WAN is added to it line by line.
			if want.LineProfile != "" {
				if l := d.ONUBindLine(id, "line", want.LineProfile); !run.Has(hdr, l) {
					lines = append(lines, l)
				}
			}
			if want.SrvProfile != "" {
				if l := d.ONUBindLine(id, "srv", want.SrvProfile); !run.Has(hdr, l) {
					lines = append(lines, l)
				}
			}
			if want.ACS != nil && want.TR069VLAN > 0 {
				lines = append(lines, d.existingONUTR069(id, run, pon, want)...)
			}
		}
		if len(lines) > 0 {
			add(fmt.Sprintf("PON %d: ONUs", pon), append(append(d.PONEnter(pon), lines...), d.PONExit()...))
		}
	}
	return p
}

var (
	wanIdxRE = regexp.MustCompile(`\bwan_adv index (\d+)\b`)
	wanTRRE  = regexp.MustCompile(`\bwan_adv index \d+ route mode tr069\b`)
)

// existingONUTR069 is what an ONU that is already on the OLT lacks for TR-069: a TR-069 WAN and the OMCI TR-069 login.
func (d *Dialect) existingONUTR069(id int, run *Running, pon int, want Desired) []string {
	var lines []string
	for _, e := range run.ONUs[pon] {
		if e.ID == id {
			lines = e.Lines
		}
	}
	hasWAN, hasMng, top := false, false, 0
	for _, l := range lines {
		if strings.HasPrefix(l, fmt.Sprintf("onu %d profile pri ", id)) {
			return nil // bound to a Pri profile: the profile has the TR-069 WAN
		}
		if wanTRRE.MatchString(l) {
			hasWAN = true
		}
		if strings.HasPrefix(l, fmt.Sprintf("onu %d pri tr069_mng enable", id)) {
			hasMng = true
		}
		if m := wanIdxRE.FindStringSubmatch(l); m != nil {
			if n, _ := strconv.Atoi(m[1]); n > top {
				top = n
			}
		}
	}
	all := d.ONUWANLines(id, top+1, want.TR069VLAN, *want.ACS)
	switch {
	case hasWAN && hasMng:
		return nil
	case hasWAN: // the WAN is there, the login is not: the last line
		return all[len(all)-1:]
	}
	return all
}
