package olt

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file is the ONLY place that knows what a V-SOL OLT's command line looks like. The grammar below is the one the
// bench OLT printed with its own `find` command (V1600GS-R, software V1.0.5), plus the bench `show running-config` and
// `show version`. What the bench did not show carries a TODO(bench) marker. Nothing else in the program hardcodes
// CLI text.
//
// Facts from the bench:
//   - SSH lands at "gpon-olt#" (already privileged). The top level is FRR (vtysh) based. `show version` and
//     `show running-config` are run from configuration mode; `show onu ...` and `onu ...` run inside
//     `interface gpon 0/<pon>` (prompt "gpon-olt(config-pon-0/1)#", grammar mode slot_pon).
//   - Profiles (dba, line, srv, pri, onu) are entered from configuration mode with `profile <kind> [id N] [name WORD]`,
//     filled, then `commit` and `exit`.
//   - ONU serials are printed with lower-case hex ("VSOL00127e0f"); inventory holds upper case. Matching is case
//     insensitive and the server sends the serial in the OLT's own form (checked by reading it back).
//   - The OLT web UI is not needed for TR-069: the ACS URL and login go in a Pri profile (`tr069_mng`).
//   - Descriptions cannot contain spaces; GE0/1 needs its PVID set before it leaves VLAN 1.

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
	// The model and software version the command table was checked against. A different version is shown as a warning.
	CheckedModel, CheckedSoftware string

	// Prompts and prompts' neighbours.
	PromptRE   *regexp.Regexp // the last line of the buffer, e.g. "gpon-olt#" or "gpon-olt(config-pon-0/1)#"
	MoreRE     *regexp.Regexp // pagination marker
	UserRE     *regexp.Regexp // "Username:" / "login:"
	PasswordRE *regexp.Regexp // "Password:"
	ErrorRE    *regexp.Regexp // an output line that means a configuration command failed (matched per line)
	QueryErrRE *regexp.Regexp // the same for a show command (only the clear refusals: a table may hold any word)

	// Deny is the hard denylist: a line matching any rule is never sent, whatever asked for it.
	Deny []DenyRule
	// Prefix-aware denials (see VSOL): DenyVerbs for the first word, DenyAnywhere for any word, DenyAfterNo for the word
	// after "no", and the only arguments "write" may take. SubVerbsOK are first words that are denied at the top level
	// of configuration mode but are ordinary inside an interface, VLAN or profile ("service" in a line profile).
	// AllowLines are the exact shapes of the few lines that start with a denied word (the management address).
	DenyVerbs, DenyAnywhere, DenyAfterNo, WriteArgsOK, SubVerbsOK []string
	AllowLines                                                    []*regexp.Regexp
	// SecureShapes are the exact shapes of the few lines that Session.RunSecure may send although the denylist refuses
	// them everywhere else: the "Secure this OLT" action (user manager passwords, the default SNMP communities).
	SecureShapes []*regexp.Regexp
	// RestoreShapes are the extra shapes of the lines that put an OLD login or enable password back after a failed change
	// (the old one may be short or typed by the owner; never '?', '!' or a space).
	RestoreShapes []*regexp.Regexp

	// Fixed commands.
	EnableCmd, ConfigCmd, EndCmd, ExitCmd, SaveCmd string
	NoPaging                                       string // sent once, a failure is ignored
	VersionCmd, RunningCmd                         string
	// VersionMode and RunningMode are the modes those two run in (the bench: configuration mode).
	VersionMode, RunningMode Mode

	// Read commands inside a PON interface.
	CmdInfo, CmdState, CmdOptical, CmdDistance, CmdAutoFind string

	// Names of the objects the plan creates or binds. The bench OLT already has dba_1g, srv_hgu and line_hgu (made by the
	// owner): they are used as they are. The Pri profile is new.
	DefaultDBA, DefaultLine, DefaultSrv, DefaultPri, DefaultONUProfile string
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

// VSOL is the dialect of the V-SOL V1600G family (V1600GS-R, software V1.0.5, on the bench).
func VSOL() *Dialect {
	d := &Dialect{
		Name:            "vsol-v1600g",
		CheckedModel:    "V1600GS-R",
		CheckedSoftware: "V1.0.5",
		// gpon-olt#  gpon-olt(config)#  gpon-olt(config-pon-0/1)#  (host name letters, digits, - _ .)
		// Any parenthesised mode name counts: the user manager sub-mode ("user manager") has a prompt the bench did not show.
		PromptRE:   regexp.MustCompile(`^[A-Za-z0-9_.\-]+(\([A-Za-z0-9_.\-/ ]+\))?[>#]\s?$`),
		MoreRE:     regexp.MustCompile(`(?i)-+\s*more\s*-+|\(more\)|press any key|<--- more --->`),
		UserRE:     regexp.MustCompile(`(?i)(user ?name|login)\s*:\s*$`),
		PasswordRE: regexp.MustCompile(`(?i)password\s*:\s*$`),
		// "% Command incomplete", "% Unknown command" and the like start with "% ".
		ErrorRE:    regexp.MustCompile(`(?i)^\s*%|unknown command|unrecognized command|\berror\b|\binvalid\b|incomplete command|ambiguous command|\bfailed\b|not exist|already exist|not found|conflict`),
		QueryErrRE: regexp.MustCompile(`(?i)^\s*%|unknown command|unrecognized command|incomplete command|ambiguous command|invalid (input|command)`),

		EnableCmd: "enable", ConfigCmd: "configure terminal", EndCmd: "end", ExitCmd: "exit", SaveCmd: "write",
		NoPaging:    "terminal length 0", // TODO(bench): status unknown; a failure is ignored
		VersionCmd:  "show version",
		RunningCmd:  "show running-config",
		VersionMode: ModeConfig,
		RunningMode: ModeConfig,

		CmdInfo:     "show onu info",
		CmdState:    "show onu state",
		CmdOptical:  "show onu optical-info",
		CmdDistance: "show onu distance", // TODO(bench): the table layout is unknown; read leniently, never an error
		CmdAutoFind: "show onu auto-find",

		DefaultDBA: "dba_1g", DefaultLine: "line_hgu", DefaultSrv: "srv_hgu", DefaultPri: "ftn_pri",
		DefaultONUProfile: "default",
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
		// The bench grammar's dangerous commands.
		{ci(`^start-shell\b`), "a shell on the OLT"},
		{ci(`^(no\s+)?onu-s\w*`), "scheduled ONU reboot"},
		{ci(`^(no\s+)?onu\s+auto-d\w*`), "automatic ONU deletion"},
		{ci(`^(no\s+)?auto-up\w*`), "automatic firmware upgrade"},
		{ci(`^rogue\S*`), "rogue ONU detection (it can shut a PON port down)"},
		{ci(`^(no\s+)?set\s+(license|cust|onu)`), "licence or customer key"},
		{ci(`^(no\s+)?(onu\s+\S+\s+)?pri\s+(factory_reset|mac_clean)`), "ONU factory reset or MAC table clean"},
		{ci(`^gponapp\b`), "debug"},
	}
	// The CLI accepts any unambiguous prefix of a command word ("reb", "wr er", "fo"), so the regular expressions above
	// are not enough on their own: a first word that is a prefix of one of DenyVerbs is refused too, and so is any word
	// (3 letters or more) that is a prefix of one of DenyAnywhere, wherever it stands ("system reboot", "onu reset 3").
	d.DenyVerbs = []string{"erase", "factory", "default", "reboot", "reload", "restart", "reset", "restore", "format", "delete", "remove",
		"copy", "tftp", "ftp", "upgrade", "upload", "download", "load", "update", "firmware", "username", "user", "passwd", "password",
		"aaa", "crypto", "shutdown", "boot", "clear", "debug", "rm", "cp",
		// The bench OLT's top-level (config) words that are never needed: system, file, login and routing settings.
		"start-shell", "exec", "software", "line", "sshd", "ssh", "telnet", "tacacs", "login-access-list", "set", "schedule", "clean",
		"save", "time", "timezone", "dst", "ntp", "remote", "router", "route-map", "route", "frr", "zebra", "bfd", "mpls", "evpn", "vrf",
		"pseudowire", "nexthop-group", "allow-external-route-update", "key", "monitor", "service", "snmp", "snmp-server", "web", "ip",
		"hostname", "domainname", "del", "output", "watchfrr", "gponapp"}
	d.DenyAnywhere = []string{"erase", "factory", "factory_reset", "mac_clean", "reboot", "reload", "reset", "restore", "format", "upgrade",
		"firmware", "debug", "license", "cust"}
	// After "no", a word that is a prefix of one of these is refused (user, password, address and route removal).
	d.DenyAfterNo = []string{"username", "user", "passwd", "password", "aaa", "enable", "ipaddress", "vlanif", "gateway", "cust"}
	// "write" saves; "write <anything>" (erase, to a server, ...) is refused, except these.
	d.WriteArgsOK = []string{"memory", "mem"}
	d.SubVerbsOK = []string{"service"}
	d.AllowLines = []*regexp.Regexp{
		regexp.MustCompile(`^ip address \d{1,3}(\.\d{1,3}){3}/\d{1,2}$`),
		regexp.MustCompile(`^ip route 0\.0\.0\.0/0 \d{1,3}(\.\d{1,3}){3}$`),
	}
	d.SecureShapes = []*regexp.Regexp{
		regexp.MustCompile(`^user manager$`),
		// A new login or enable password is made by the server: letters and digits only (the CLI reads '?' as help and '!' as a
		// comment), 24 characters or more. A restored password may be an older one the owner typed (no '?', '!' or space).
		regexp.MustCompile(`^user (login|enable)-password [A-Za-z][A-Za-z0-9_-]{0,31} [A-Za-z0-9]{24,64}$`),
		// Only the two default SNMP communities, by name.
		regexp.MustCompile(`^no snmp-server community (public|private)$`),
		// SSH only (never telnet, snmp or web): permit one host (the router's OLT-management address), deny every other
		// IPv4 and IPv6 source, switch the list on; and the same lines with `no` (the undo when a new session cannot log in).
		// Bench grammar: `login-access-list <permit|deny> <telnet|web|snmp|ssh> A.B.C.D A.B.C.D` (address and MASK),
		// `login-access-list ipv6 <permit|deny> <...> X:X::X:X/M`, `login-access-list enable`.
		regexp.MustCompile(`^(no )?login-access-list permit ssh ((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d) 255\.255\.255\.255$`),
		regexp.MustCompile(`^(no )?login-access-list deny ssh 0\.0\.0\.0 0\.0\.0\.0$`),
		regexp.MustCompile(`^(no )?login-access-list ipv6 deny ssh ::/0$`),
		regexp.MustCompile(`^login-access-list enable$`),
		regexp.MustCompile(`^exit$`),
	}
	d.RestoreShapes = []*regexp.Regexp{
		regexp.MustCompile(`^user (login|enable)-password [A-Za-z][A-Za-z0-9_-]{0,31} [A-Za-z0-9@#%^&*_+=.,:;~-]{1,64}$`),
	}
	return d
}

// ---- version -------------------------------------------------------------------------------------------------------

// VersionQuery is the read of the OLT's version.
func (d *Dialect) VersionQuery() Query { return Query{d.VersionMode, []string{d.VersionCmd}} }

// VersionNote is the warning to show when the OLT is not the model and software the command table was checked
// against; empty when it is (or when the OLT did not say).
func (d *Dialect) VersionNote(model, software string) string {
	if model == "" && software == "" {
		return ""
	}
	if strings.EqualFold(model, d.CheckedModel) && strings.EqualFold(software, d.CheckedSoftware) {
		return ""
	}
	return fmt.Sprintf("Commands checked against %s software %s; this OLT reports %s software %s. Read every plan before applying it.",
		d.CheckedModel, d.CheckedSoftware, orUnknown(model), orUnknown(software))
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// ---- reads inside a PON interface ----------------------------------------------------------------------------------

// PONEnter and PONExit enter and leave a PON port's interface.
func (d *Dialect) PONEnter(pon int) []string {
	return []string{fmt.Sprintf("interface gpon 0/%d", pon)}
}
func (d *Dialect) PONExit() []string { return []string{"exit"} }

// PONReadQuery reads everything about one PON port in one visit: the ONU table, states, optics, distances and the
// auto-find list.
func (d *Dialect) PONReadQuery(pon int) Query {
	cmds := append(d.PONEnter(pon), d.CmdInfo, d.CmdState, d.CmdOptical, d.CmdDistance, d.CmdAutoFind)
	return Query{ModeConfig, append(cmds, d.PONExit()...)}
}

// ---- the command builders (text only) ------------------------------------------------------------------------------

// VLANLines creates a VLAN.
func (d *Dialect) VLANLines(vlan int) []string { return []string{fmt.Sprintf("vlan %d", vlan), "exit"} }

// UplinkLines puts VLANs on the uplink port: tagged lists tagged VLANs, untagged is the one untagged VLAN (0 = none),
// which also becomes the port's PVID (bench: the port needs it to leave VLAN 1). port is "0/1".
func (d *Dialect) UplinkLines(port string, tagged []int, untagged int) []string {
	l := []string{"interface gigabitEthernet " + port}
	for _, v := range tagged {
		l = append(l, fmt.Sprintf("switchport hybrid vlan %d tagged", v))
	}
	if untagged > 0 {
		l = append(l, fmt.Sprintf("switchport hybrid vlan %d untagged", untagged), fmt.Sprintf("switchport hybrid pvid vlan %d", untagged))
	}
	return append(l, "exit")
}

// MgmtIfaceLines gives the OLT its management address on the management VLAN, with its default route. The bench shows
// `interface vlan 1` / `ip address 192.168.8.200/24`.
func (d *Dialect) MgmtIfaceLines(vlan int, ip, mask, gateway string) []string {
	return []string{
		fmt.Sprintf("interface vlan %d", vlan), fmt.Sprintf("ip address %s/%d", ip, maskBits(mask)), "exit",
		// TODO(bench): the default-route syntax is not in the captured grammar; `ip route 0.0.0.0/0 GW` is the FRR form.
		fmt.Sprintf("ip route 0.0.0.0/0 %s", gateway),
	}
}

// maskBits turns "255.255.255.248" into 29 (0 when it is not a netmask).
func maskBits(mask string) int {
	a, err := netip.ParseAddr(mask)
	if err != nil || !a.Is4() {
		return 0
	}
	b := a.As4()
	n := 0
	for _, x := range b {
		for i := 7; i >= 0; i-- {
			if x&(1<<i) == 0 {
				return n
			}
			n++
		}
	}
	return n
}

// DBALines creates a DBA profile of one maximum rate (kbit/s). Bench: `profile dba id 10 name dba_1g` holds
// `type 4 maximum 1000000` and, unlike the line, srv and onu profiles, no `commit` (the running configuration is what
// the OLT replays at boot, so the DBA profile mode takes none).
func (d *Dialect) DBALines(name string, maximum int) []string {
	return []string{"profile dba name " + name, fmt.Sprintf("type 4 maximum %d", maximum), "exit"}
}

// LineProfileLines creates a line profile like the bench's line_hgu: a T-CONT on the DBA profile, one gemport, one
// service for the internet VLAN (and the ONU management VLAN) and a service-port for each.
func (d *Dialect) LineProfileLines(name, dba string, inetVLAN, mgmtVLAN int) []string {
	vl := []int{inetVLAN}
	svc := "internet"
	if mgmtVLAN > 0 {
		vl = append(vl, mgmtVLAN)
		svc = "internet_mgmt"
	}
	sort.Ints(vl)
	var list []string
	for _, v := range vl {
		list = append(list, strconv.Itoa(v))
	}
	l := []string{
		"profile line name " + name,
		"tcont 1 dba " + dba,
		"gemport 1 tcont 1 gemport_name gem_1", // TODO(bench-verify): shown by the bench running config; the creating command is not in `find`
		fmt.Sprintf("service %s gemport 1 tag %s", svc, strings.Join(list, ",")),
		fmt.Sprintf("service-port 1 gemport 1 uservlan %d vlan %d", inetVLAN, inetVLAN),
		"service-port 1 description internet",
	}
	if mgmtVLAN > 0 {
		l = append(l, fmt.Sprintf("service-port 2 gemport 1 uservlan %d vlan %d", mgmtVLAN, mgmtVLAN), "service-port 2 description onu_mgmt")
	}
	return append(l, "commit", "exit")
}

// SrvProfileLines creates a service profile of an HGU: one VEIP port, VLANs pass transparently (bench srv_hgu).
func (d *Dialect) SrvProfileLines(name string) []string {
	return []string{"profile srv name " + name, "portvlan veip 1 mode transparent", "commit", "exit"}
}

// ACS is what an ONU needs to reach the ACS. The values are secrets (the login): they only ever appear in command
// lines that are sent, and Redact hides them everywhere else.
type ACS struct {
	URL, User, Pass string
	InformSeconds   int
}

// Valid reports whether the values can go in a command line (one word each, no spaces).
func (a ACS) Valid() bool {
	return acsURLRE.MatchString(a.URL) && acsWordRE.MatchString(a.User) && acsWordRE.MatchString(a.Pass)
}

var (
	acsURLRE  = regexp.MustCompile(`^https?://[A-Za-z0-9.\-]+(:\d{1,5})?(/[A-Za-z0-9._~/%\-]*)?$`)
	acsWordRE = regexp.MustCompile(`^[A-Za-z0-9._@\-]{1,64}$`)
)

// tr069MngLine is the OMCI TR-069 management line (grammar: `tr069_mng enable acs_server url WORD username WORD password
// WORD certificate <enable|disable> inform enable inform_interval N reverse_connection username WORD password WORD`).
// The connection-request login is the shared ACS login too: the ACS replaces it per ONU after first contact.
func (d *Dialect) tr069MngLine(a ACS) string {
	cert := "disable"
	if strings.HasPrefix(a.URL, "https://") {
		cert = "enable"
	}
	every := a.InformSeconds
	if every <= 0 {
		every = 300
	}
	return fmt.Sprintf("tr069_mng enable acs_server url %s username %s password %s certificate %s inform enable inform_interval %d reverse_connection username %s password %s",
		a.URL, a.User, a.Pass, cert, every, a.User, a.Pass)
}

// PriProfileLines creates the Pri profile new ONUs are bound to: the internet WAN (route, DHCP, NAT, the internet VLAN,
// all LAN ports and the first SSID, like the bench's ONU 1) and, when mgmtVLAN > 0 and acs is set, a TR-069 WAN (route
// mode tr069, DHCP, no NAT, the management VLAN) with the OMCI TR-069 login. A profile is only ever created, never
// edited: `wan_adv add route` is not idempotent.
func (d *Dialect) PriProfileLines(name string, inetVLAN, mgmtVLAN int, acs *ACS) []string {
	l := []string{"profile pri name " + name,
		"wan_adv add route",
		"wan_adv index 1 route mode internet mtu 1500",
		"wan_adv index 1 route ipv4 dhcp nat enable",
		fmt.Sprintf("wan_adv index 1 vlan tag wan_vlan %d 0 qos enable", inetVLAN),
		"wan_adv index 1 bind lan lan1 lan2 lan3 lan4",
		"wan_adv index 1 bind ssid ssid1",
	}
	if mgmtVLAN > 0 && acs != nil {
		l = append(l, "wan_adv add route", "wan_adv index 2 route mode tr069", "wan_adv index 2 route ipv4 dhcp nat disable",
			fmt.Sprintf("wan_adv index 2 vlan tag wan_vlan %d 0", mgmtVLAN), d.tr069MngLine(*acs))
	}
	return append(l, "commit", "exit")
}

// ONUAddLine authorises an ONU by serial inside the PON interface. The serial is sent in the OLT's own form: vendor
// letters upper case, the 8 hex digits lower case ("VSOL00127e0f"), which is how the bench prints it in `show onu info`
// and keeps it in the running configuration it replays at boot. That form is known to be accepted; upper-case hex is
// not. Every comparison on the server side stays case insensitive.
func (d *Dialect) ONUAddLine(id int, profile, serial string) string {
	return fmt.Sprintf("onu add %d profile %s sn %s", id, profile, oltSerial(serial))
}

var gponSNFormRE = regexp.MustCompile(`^[A-Za-z]{4}[0-9A-Fa-f]{8}$`)

// oltSerial is a GPON serial in the form the OLT prints it; anything else is returned unchanged.
func oltSerial(s string) string {
	if !gponSNFormRE.MatchString(s) {
		return s
	}
	return strings.ToUpper(s[:4]) + strings.ToLower(s[4:])
}

// ONURemoveLine removes an authorised ONU (inside the PON interface).
func (d *Dialect) ONURemoveLine(id int) string { return fmt.Sprintf("no onu %d", id) }

// ONUBindLine binds a line, srv or pri profile to an ONU (inside the PON interface).
func (d *Dialect) ONUBindLine(id int, kind, name string) string {
	return fmt.Sprintf("onu %d profile %s name %s", id, kind, name)
}

// ONUDescLine names an ONU. A description is one word.
func (d *Dialect) ONUDescLine(id int, label string) string {
	return fmt.Sprintf("onu %d desc %s", id, descWord(label))
}

var descBadRE = regexp.MustCompile(`[^A-Za-z0-9_\-]+`)

// descWord makes a label one word of at most 24 characters ("" when nothing is left).
func descWord(s string) string {
	s = strings.Trim(descBadRE.ReplaceAllString(strings.TrimSpace(s), "_"), "_")
	if len(s) > 24 {
		s = s[:24]
	}
	return s
}

// ONUWANLines gives one ONU its TR-069 WAN and OMCI TR-069 login, per ONU (an ONU that was authorised before the Pri
// profile existed). index is the WAN index the new connection gets: the next one after the ONU's existing ones.
func (d *Dialect) ONUWANLines(id, index, mgmtVLAN int, acs ACS) []string {
	p := fmt.Sprintf("onu %d pri ", id)
	return []string{
		p + "wan_adv add route",
		fmt.Sprintf("%swan_adv index %d route mode tr069", p, index),
		fmt.Sprintf("%swan_adv index %d route ipv4 dhcp nat disable", p, index),
		fmt.Sprintf("%swan_adv index %d vlan tag wan_vlan %d 0", p, index, mgmtVLAN),
		p + "wan_adv commit", // TODO(bench-verify): the grammar has `wan_adv <commit|show>`; the bench config does not show whether a commit is needed
		p + d.tr069MngLine(acs),
	}
}

// AutoLearnOffLines stop a PON port from authorising any ONU that plugs in (bench: `onu auto-learn` and
// `plug_and_play enable vlan 1` are on).
func (d *Dialect) AutoLearnOffLines(pon int) []string {
	return append(append(d.PONEnter(pon), "no onu auto-learn", "plug_and_play disable"), d.PONExit()...)
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

// ---- Secure this OLT -------------------------------------------------------------------------------------------------

// SecureUserLines change the login and enable password of one user (inside `user manager`). Both are one word, with no
// space, '?' or '!'.
func (d *Dialect) SecureUserLines(user, login, enable string) []string {
	l := []string{"user manager"}
	if login != "" {
		l = append(l, "user login-password "+user+" "+login)
	}
	if enable != "" {
		l = append(l, "user enable-password "+user+" "+enable)
	}
	return append(l, "exit")
}

// SecureSNMPLines remove the default SNMP communities that exist (words are "public" and "private" only).
func (d *Dialect) SecureSNMPLines(words []string) []string {
	var l []string
	for _, w := range words {
		l = append(l, "no snmp-server community "+w)
	}
	return l
}

// ShowACLCmd lists the login access list (layout not captured on the bench: read leniently, with the running config).
const ShowACLCmd = "show login-access-list"

// ACLPlan is the SSH restriction: the exact lines, in the order they are sent (permit first, then the denies).
type ACLPlan struct {
	Source               string
	Permit, Deny4, Deny6 string
	Enable               string
}

// SecureACLPlan builds the lines that allow SSH from the one address src (a host: mask 255.255.255.255) and deny it from
// everywhere else. src must be a plain unicast IPv4 address.
func (d *Dialect) SecureACLPlan(src string) (ACLPlan, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(src))
	if err != nil || !a.Is4() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return ACLPlan{}, fmt.Errorf("%q is not a usable source address", src)
	}
	return ACLPlan{Source: a.String(),
		Permit: "login-access-list permit ssh " + a.String() + " 255.255.255.255",
		Deny4:  "login-access-list deny ssh 0.0.0.0 0.0.0.0",
		Deny6:  "login-access-list ipv6 deny ssh ::/0",
		Enable: "login-access-list enable"}, nil
}

// ShowSNMPCmd lists the SNMP communities.
const ShowSNMPCmd = "show snmp-server community"

// SecureLineOK reports whether a line has one of the exact shapes of the secure action (restore: also the shapes that put
// an old password back).
func (d *Dialect) SecureLineOK(line string, restore bool) bool {
	shapes := d.SecureShapes
	if restore {
		shapes = append(append([]*regexp.Regexp(nil), shapes...), d.RestoreShapes...)
	}
	for _, re := range shapes {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}
