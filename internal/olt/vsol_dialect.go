package olt

import (
	"fmt"
	"regexp"
	"strings"
)

// This file is the ONLY place that knows what a V-SOL OLT's command line looks like. The syntax below comes from
// research (vendor blogs, a third-party sample, one bench session), not from the V1600GS-R manual, so every line that
// is not yet confirmed carries a TODO(bench) marker. When the bench `show running-config` and `show version` are in,
// fix the table here; nothing else in the program hardcodes CLI text.
//
// Confirmed on the bench (owner, 2026-11): the OLT has a web UI where the service profile's TR069 tab holds the ACS URL
// and login; descriptions cannot contain spaces; GE0/1 needs its PVID set before it leaves VLAN 1.

// Mode is where in the CLI a command runs.
type Mode int

const (
	ModeUser   Mode = iota // gpon-olt>
	ModePriv               // gpon-olt#
	ModeConfig             // gpon-olt(config...)#
)

// Dialect is a CLI dialect: how prompts and errors look, what may never be sent, and every command text.
type Dialect struct {
	Name string

	// Prompts and prompts' neighbours.
	PromptRE   *regexp.Regexp // the last line of the buffer, e.g. "gpon-olt#" or "gpon-olt(config-if)#"
	MoreRE     *regexp.Regexp // pagination marker
	UserRE     *regexp.Regexp // "Username:" / "login:"
	PasswordRE *regexp.Regexp // "Password:"
	ErrorRE    *regexp.Regexp // an output line that means a configuration command failed (matched per line)
	QueryErrRE *regexp.Regexp // the same for a show command (only the clear refusals: a table may hold any word)

	// Deny is the hard denylist: a line matching any rule is never sent, whatever asked for it.
	Deny []DenyRule
	// Prefix-aware denials (see VSOL): DenyVerbs for the first word, DenyAnywhere for any word, DenyAfterNo for the word
	// after "no", and the only arguments "write" may take.
	DenyVerbs, DenyAnywhere, DenyAfterNo, WriteArgsOK []string

	// Fixed commands.
	EnableCmd, ConfigCmd, EndCmd, ExitCmd, SaveCmd string
	NoPaging                                       string // sent once, a failure is ignored
	VersionCmd, RunningCmd, AutoFindCmd            string

	// Build the commands of the actions below. The functions only return text.
	StateQuery   func(pon int) Query // the ONU state table of one PON port
	OpticalQuery func(pon int) Query // optical power and distance of one PON port
	// TODO(bench): the OMCI TR-069 CLI (ACS URL, login) is not known; the bench did it in the web UI service profile's TR069 tab.
	TR069Note string
}

// Query is a read-only command sequence and the mode it starts in.
type Query struct {
	Mode Mode
	Cmds []string
}

// DenyRule is one hard-denied command shape.
type DenyRule struct {
	RE  *regexp.Regexp
	Why string
}

func ci(s string) *regexp.Regexp { return regexp.MustCompile(`(?i)` + s) }

// VSOL is the dialect of the V-SOL V1600G family (V1600GS-R on the bench).
func VSOL() *Dialect {
	d := &Dialect{
		Name: "vsol-v1600g",
		// gpon-olt>  gpon-olt#  gpon-olt(config)#  gpon-olt(config-if-gpon-0/1)#  (host name letters, digits, - _ .)
		PromptRE:   regexp.MustCompile(`^[A-Za-z0-9_.\-]+(\((?:config|cfg)[^)\n]*\))?[>#]\s?$`),
		MoreRE:     regexp.MustCompile(`(?i)-+\s*more\s*-+|\(more\)|press any key|<--- more --->`),
		UserRE:     regexp.MustCompile(`(?i)(user ?name|login)\s*:\s*$`),
		PasswordRE: regexp.MustCompile(`(?i)password\s*:\s*$`),
		ErrorRE:    regexp.MustCompile(`(?i)^\s*%|unknown command|unrecognized command|\berror\b|\binvalid\b|incomplete command|ambiguous command|\bfailed\b|not exist|already exist|not found|conflict`),

		QueryErrRE: regexp.MustCompile(`(?i)^\s*%|unknown command|unrecognized command|incomplete command|ambiguous command|invalid (input|command)`),

		EnableCmd: "enable", ConfigCmd: "configure terminal", EndCmd: "end", ExitCmd: "exit", SaveCmd: "write",
		NoPaging:    "terminal length 0", // TODO(bench): unverified; a failure is ignored
		VersionCmd:  "show version",
		RunningCmd:  "show running-config",
		AutoFindCmd: "show onu auto-find",

		TR069Note: "The ACS URL and login are not set over the OLT command line: set them once in the OLT web UI, service profile, TR069 tab (URL http://10.99.0.1:7547/ and the shared ACS login from Settings). TODO(bench): find the OMCI TR-069 CLI.",
	}
	d.StateQuery = func(pon int) Query {
		// TODO(bench): `show onu state` runs inside `interface gpon 0/<pon>` on the sample; some firmware takes `show onu state gpon 0/<pon>` at the # prompt.
		return Query{ModeConfig, []string{fmt.Sprintf("interface gpon 0/%d", pon), "show onu state", "exit"}}
	}
	d.OpticalQuery = func(pon int) Query {
		// TODO(bench): the optical table command is unknown (the MIB has gOnuOpticalInfoRxPwr/TxPwr, gOnuRttDistance).
		return Query{ModeConfig, []string{fmt.Sprintf("interface gpon 0/%d", pon), "show onu optical-info", "exit"}}
	}
	d.Deny = []DenyRule{
		{ci(`^(erase|eras|era)\b`), "erase"},
		{ci(`^(factory|default)\b`), "factory or default reset"},
		{ci(`^(reboot|reload|restart|reset|rebo|relo)\b`), "reboot"},
		{ci(`^(format|fo)\b`), "format"},
		{ci(`^(delete|del|rm|remove)\b.*(startup|config|flash|image)`), "delete of a configuration"},
		{ci(`^(delete|del|rm)\b`), "delete"},
		{ci(`^(write|wr)\s+(erase|default|factory|net|network|file|tftp|ftp)`), "write to elsewhere"},
		{ci(`^(copy|cp|tftp|ftp|upgrade|upload|download|load|update|firmware)\b`), "file transfer or firmware"},
		{ci(`^(username|user|passwd|password|aaa|crypto|enable\s+(password|secret))\b`), "user, password or key change"},
		{ci(`^no\s+(username|user|passwd|password|aaa|enable)\b`), "user or password removal"},
		{ci(`^(shutdown|shut)\b`), "port shutdown"},
		{ci(`^no\s+(ip\s*address|ipaddress|ip\s+route|ip\s+default-gateway|ip\s+gateway|gateway)\b`), "removal of the management address or route"},
		{ci(`^no\s+(interface\s+vlan|vlanif)\b`), "removal of a VLAN interface"},
	}
	// The CLI accepts any unambiguous prefix of a command word ("reb", "wr er", "fo"), so the regular expressions above
	// are not enough on their own: a first word that is a prefix of one of DenyVerbs is refused too, and so is any word
	// (3 letters or more) that is a prefix of one of DenyAnywhere, wherever it stands ("system reboot", "onu reset 3").
	d.DenyVerbs = []string{"erase", "factory", "default", "reboot", "reload", "restart", "reset", "restore", "format", "delete", "remove",
		"copy", "tftp", "ftp", "upgrade", "upload", "download", "load", "update", "firmware", "username", "user", "passwd", "password",
		"aaa", "crypto", "shutdown", "boot", "clear", "debug", "rm", "cp"}
	d.DenyAnywhere = []string{"erase", "factory", "reboot", "reload", "reset", "restore", "format", "upgrade", "firmware"}
	// After "no", a word that is a prefix of one of these is refused (user, password, address and route removal).
	d.DenyAfterNo = []string{"username", "user", "passwd", "password", "aaa", "enable", "ipaddress", "vlanif", "gateway"}
	// "write" saves; "write <anything>" (erase, to a server, ...) is refused, except these.
	d.WriteArgsOK = []string{"memory", "mem"}
	return d
}

// ---- the command builders (text only) -----------------------------------------------------------------------------

// Spec is what the plan builder asks the dialect to render.

// VLANLines creates a VLAN.
func (d *Dialect) VLANLines(vlan int) []string { return []string{fmt.Sprintf("vlan %d", vlan), "exit"} }

// UplinkLines puts VLANs on the uplink port: tagged lists tagged VLANs, untagged is the one untagged VLAN (0 = none),
// which also becomes the port's PVID (bench: the port needs it to leave VLAN 1).
func (d *Dialect) UplinkLines(port string, tagged []int, untagged int) []string {
	l := []string{"interface gigabitethernet " + port}
	for _, v := range tagged {
		l = append(l, fmt.Sprintf("switchport hybrid vlan %d tagged", v))
	}
	if untagged > 0 {
		l = append(l, fmt.Sprintf("switchport hybrid vlan %d untagged", untagged), fmt.Sprintf("switchport hybrid pvid vlan %d", untagged))
	}
	return append(l, "exit")
}

// MgmtIfaceLines gives the OLT its management address on the management VLAN, with its default route.
func (d *Dialect) MgmtIfaceLines(vlan int, ip, mask, gateway string) []string {
	return []string{
		fmt.Sprintf("interface vlan %d", vlan), fmt.Sprintf("ipaddress %s %s", ip, mask), "exit",
		// TODO(bench): the default-route syntax is unknown (ip route / ip default-gateway).
		fmt.Sprintf("ip route 0.0.0.0 0.0.0.0 %s", gateway),
	}
}

// DBALines creates a DBA profile (type 3: assured + maximum, kbit/s).
func (d *Dialect) DBALines(name string, assured, maximum int) []string {
	return []string{"profile dba name " + name, fmt.Sprintf("type 3 assured %d maximum %d", assured, maximum), "commit", "exit"}
}

// ONUProfileLines creates the ONU profile of an HGU (the ONU routes by itself; one VEIP port).
func (d *Dialect) ONUProfileLines(name string) []string {
	// TODO(bench): sample is `port-num eth 1`; the HGU form (VEIP=1) is a guess.
	return []string{"profile onu name " + name, "port-num veip 1", "commit", "exit"}
}

// PONEnter and PONExit enter and leave a PON port's interface.
func (d *Dialect) PONEnter(pon int) []string {
	return []string{fmt.Sprintf("interface gpon 0/%d", pon)}
}
func (d *Dialect) PONExit() []string { return []string{"exit"} }

// ONUAddLine authorises an ONU by serial inside the PON interface.
func (d *Dialect) ONUAddLine(id int, profile, serial string) string {
	return fmt.Sprintf("onu add %d profile %s sn %s", id, profile, serial)
}

// ONURemoveLine removes an authorised ONU (inside the PON interface). TODO(bench): unverified.
func (d *Dialect) ONURemoveLine(id int) string { return fmt.Sprintf("no onu %d", id) }

// ONUServiceLines are one ONU's service lines inside its PON interface: a T-CONT on the DBA profile, a gemport for
// the internet and one for TR-069 management, a service and a service-port each, and the port VLAN.
func (d *Dialect) ONUServiceLines(id int, dba string, inetVLAN, mgmtVLAN int) []string {
	l := []string{
		fmt.Sprintf("onu %d tcont 1 dba %s", id, dba),
		fmt.Sprintf("onu %d gemport 1 tcont 1", id),
		fmt.Sprintf("onu %d service 1 gemport 1 vlan %d", id, inetVLAN),
		fmt.Sprintf("onu %d service-port 1 gemport 1 uservlan %d vlan %d", id, inetVLAN, inetVLAN),
	}
	if mgmtVLAN > 0 {
		l = append(l,
			fmt.Sprintf("onu %d gemport 2 tcont 1", id),
			fmt.Sprintf("onu %d service 2 gemport 2 vlan %d", id, mgmtVLAN),
			fmt.Sprintf("onu %d service-port 2 gemport 2 uservlan %d vlan %d", id, mgmtVLAN, mgmtVLAN))
	}
	// TODO(bench): for an HGU the LAN side is the ONU's own; this line follows the sample and may be dropped.
	return append(l, fmt.Sprintf("onu %d portvlan eth 1 mode hybrid def_vlan %d", id, inetVLAN))
}

// AdoptSnippet is what the owner pastes into the OLT console (USB-C or the factory address) to make a factory OLT
// reachable by the server: the management VLAN, its address and default route, the tag on the uplink, then save.
func (d *Dialect) AdoptSnippet(vlan int, ip, mask, gateway, uplink string) []string {
	l := []string{d.EnableCmd, d.ConfigCmd}
	l = append(l, d.VLANLines(vlan)...)
	l = append(l, d.MgmtIfaceLines(vlan, ip, mask, gateway)...)
	l = append(l, d.UplinkLines(uplink, []int{vlan}, 0)...)
	return append(l, d.EndCmd, d.SaveCmd)
}

// ---- helpers -------------------------------------------------------------------------------------------------------

// Norm is a command line in the form lines are compared in: lower case, single spaces.
func Norm(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
