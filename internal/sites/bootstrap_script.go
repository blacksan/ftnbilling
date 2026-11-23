package sites

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"text/template"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

//go:embed bootstrap.rsc.tmpl
var bootstrapTmplSrc string

// File names on the router.
const (
	// MgmtWinboxComment and MgmtICMPComment mark the input accepts of the remote-WinBox subnet.
	MgmtWinboxComment = "ftn: mgmt winbox"
	MgmtICMPComment   = "ftn: mgmt icmp"

	SetupFile = "ftn-setup.rsc"
	ResetFile = "ftn-reset.rsc"

	// ResetWindow is how long after the setup script was fetched the reset stub is served.
	ResetWindow = 5 * time.Minute
)

// BootstrapInput is everything the server decides before it renders the
// bootstrap script. The script has no router-side variables to fill in.
type BootstrapInput struct {
	Site       store.Site // with defaults applied
	PPPoEPass  string     // opened from the sealed password; only for pppoe
	PrivateKey string     // the router's WireGuard private key (never stored)
	RESTPass   string
	ServerPub  string // the server's WireGuard public key
	Endpoint   string // host:port the router dials
	WGSubnet   netip.Prefix
	// MgmtSubnet is the remote-WinBox subnet (the server's wg-mgmt interface):
	// the router accepts WinBox from it through the tunnel. Zero means DefaultMgmtRange.
	MgmtSubnet netip.Prefix
	PublicBase string // https://billing.example.co.ke
	PublicIP   string // the server's public address (walled flats are sent here)
	Generated  time.Time
	// NoCheck: the admin used the "no certificate check" command (router with a
	// wrong clock or no CA store): the report POST skips the check too.
	NoCheck bool
	// ROSVersion is the router's RouterOS version from the fetch User-Agent
	// ("7.25.1"); 7.13 and newer get ":onerror" section wrappers with the real
	// error text, anything else the old form. See secwrap.go.
	ROSVersion string
	// Token is the token the router fetched with; the script uses it to POST
	// its report to /j/{token}/report.
	Token string
	// NetflowPort is the server's traffic-flow port; 0 leaves traffic-flow alone.
	NetflowPort int
	// WebFigDomain (WEBFIG_DOMAIN) is, with the host of PublicBase and PublicIP, what the admin port may reach.
	WebFigDomain string
	// ONU management (TR-069): used when the site has a management subnet (Site.ONUMgmtN > 0). ACSPort and ACSURL
	// are the ACS the ONUs are pointed to (zero values mean tcp/7547 on the tunnel address); CRPorts the
	// connection-request ports the server may use towards the ONUs (empty = 7547 and 30005).
	ACSPort int
	ACSURL  string
	CRPorts []int
}

// bootstrapData is the template view of BootstrapInput.
type bootstrapData struct {
	SiteID, SiteName, Generated string
	Identity                    string
	ROSVersion                  string
	NeedPorts                   string // RouterOS array literal of the ports to wait for

	WGIface                          string
	MTU                              int
	PrivateKey, ServerPublicKey      string
	TunnelIP, ServerIP               string
	MgmtSubnet                       string // remote-WinBox subnet, e.g. 10.98.0.0/24
	EndpointHost                     string
	EndpointPort                     int
	RESTUser, RESTPass               string
	RESTPort                         int
	WANIface, WANMode                string
	WANAddress, WANGateway, WANDNS   string
	PPPoEUser, PPPoEPass             string
	PPPoEIface                       string
	LANPorts                         []string
	LAN                              LANPlan
	LANBridge, CustBridge            string
	OLTInterface, CustIface          string
	VLANID                           int
	VLANName                         string
	CustomerPorts                    []string
	Plan                             SubnetPlan
	DHCPServer, UnregList, UnregPool string
	DNSServers                       string
	ONU                              *ONUMgmtPlan
	ONUSetup                         []string
	Filter, NAT                      []string
	AdminAllow                       []AdminAllowEntry
	AdminList                        string
	KeepUp                           string // RouterOS array literal of the ports that stay on: the uplink and the admin ports
	NetflowPort                      int
	NoCheck                          bool
	Token                            string
	ReportPost                       string
	Files                            []string
}

// rq escapes a value for use inside a RouterOS double-quoted string. A "$"
// is always escaped (a "$" followed by a name is a variable, and at the end of
// a string it is an error).
func rq(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(s)
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`).Replace(s)
}

// walled is the walled-garden rule set split by chain, as the bootstrap script
// and the walled.rules check/fix use it. The two "ftn: ipoe dns" accepts of
// ipoeRuleSet are left out: "ftn: customer dns" is the same accept.
type walled struct {
	Forward, Input, NAT []ipoeRule
}

func walledRules(plan SubnetPlan, publicIP string, restPort int) walled {
	f, n := ipoeRuleSet(plan, publicIP, restPort)
	var w walled
	for _, r := range f {
		switch {
		case strings.HasPrefix(r.Comment, "ftn: ipoe dns "):
		case strings.HasPrefix(r.Args(), "chain=input"):
			w.Input = append(w.Input, r)
		default:
			w.Forward = append(w.Forward, r)
		}
	}
	w.NAT = n
	return w
}

// All is every rule of the set (filter first, then NAT).
func (w walled) All() []ipoeRule {
	var out []ipoeRule
	out = append(out, w.Forward...)
	out = append(out, w.Input...)
	return append(out, w.NAT...)
}

func ruleLine(table, args, comment string) string {
	return fmt.Sprintf(`  /ip/firewall/%s add %s comment="%s"`, table, args, rq(comment))
}

func (r ipoeRule) line() string {
	return ruleLine(r.Table, strings.ReplaceAll(r.Args(), "$ifc", CustomerBridge), r.Comment)
}

// bootstrapFirewall is the whole firewall of the router, top to bottom. The
// script owns the tables: nothing is opened on the WAN (the router dials the
// tunnel itself) and there is no FastTrack, so the flats' queues see traffic.
func bootstrapFirewall(plan SubnetPlan, lan LANPlan, serverIP, mgmt, publicIP string, restPort int, onu *ONUMgmtPlan) (filter, nat []string) {
	w := walledRules(plan, publicIP, restPort)
	f := func(args, comment string) { filter = append(filter, ruleLine("filter", args, comment)) }
	// Forward. The admin port rules come first of all: nothing above them can accept the admin LAN's traffic,
	// and the established/related accept below cannot let it out (see adminfw.go).
	for _, r := range adminRuleSet(lan.Network) {
		filter = append(filter, r.line())
	}
	for _, r := range w.Forward {
		filter = append(filter, r.line())
	}
	f("chain=forward action=accept connection-state=established,related", "ftn: forward est/rel")
	f("chain=forward action=drop connection-state=invalid", "ftn: forward drop invalid")
	// The ONU management network (TR-069) crosses the tunnel to the ACS and back: these accepts sit above the
	// two tunnel drops below, and the management VLAN may go nowhere else.
	var onuIn []ipoeRule
	if onu != nil {
		var fwd []ipoeRule
		fwd, onuIn = onu.Rules()
		for _, r := range fwd {
			filter = append(filter, r.line())
		}
	}
	f("chain=forward action=drop in-interface="+WGInterface, "ftn: wg no forward in")
	f("chain=forward action=drop out-interface="+WGInterface, "ftn: wg no forward out")
	f("chain=forward action=accept in-interface-list=LAN out-interface="+CustomerBridge, "ftn: lan to customers")
	f("chain=forward action=accept in-interface="+CustomerBridge+" out-interface-list=WAN", "ftn: customers to wan")
	f("chain=forward action=drop", "ftn: forward drop rest")
	// Input.
	f("chain=input action=accept connection-state=established,related", "ftn: input est/rel")
	f("chain=input action=drop connection-state=invalid", "ftn: input drop invalid")
	f("chain=input action=drop protocol=udp dst-port=53 in-interface-list=WAN", "ftn: no WAN DNS")
	f("chain=input action=drop protocol=tcp dst-port=53 in-interface-list=WAN", "ftn: no WAN DNS")
	f(fmt.Sprintf("chain=input action=accept protocol=tcp in-interface=%s src-address=%s dst-port=%d", WGInterface, serverIP, restPort), "ftn: wg rest")
	f(fmt.Sprintf("chain=input action=accept protocol=icmp in-interface=%s src-address=%s", WGInterface, serverIP), "ftn: wg icmp")
	// Remote WinBox: admin devices of the server's wg-mgmt interface arrive through the tunnel with their own
	// addresses (the server does not NAT them). Only WinBox (and ping) from that subnet; it sits before the drop.
	f(fmt.Sprintf("chain=input action=accept protocol=tcp dst-port=8291 src-address=%s in-interface=%s", mgmt, WGInterface), MgmtWinboxComment)
	f(fmt.Sprintf("chain=input action=accept protocol=icmp src-address=%s in-interface=%s", mgmt, WGInterface), MgmtICMPComment)
	f("chain=input action=drop in-interface="+WGInterface, "ftn: wg drop other input")
	f("chain=input action=accept in-interface-list=LAN", "ftn: lan input")
	f("chain=input action=accept protocol=udp dst-port=53 in-interface="+CustomerBridge, custDNSComment)
	f("chain=input action=accept protocol=tcp dst-port=53 in-interface="+CustomerBridge, custDNSComment)
	f("chain=input action=accept protocol=udp dst-port=67 in-interface="+CustomerBridge, "ftn: customer dhcp")
	f("chain=input action=accept protocol=icmp in-interface="+CustomerBridge, "ftn: customer icmp")
	for _, r := range onuIn {
		filter = append(filter, r.line())
	}
	for _, r := range w.Input {
		filter = append(filter, r.line())
	}
	f("chain=input action=drop", "ftn: input drop rest")
	// NAT.
	n := func(args, comment string) { nat = append(nat, ruleLine("nat", args, comment)) }
	n("chain=srcnat action=masquerade out-interface-list=WAN src-address="+plan.Network, "ftn: customer masquerade")
	// Only what the admin accept lets through (the billing site) ever meets this rule; without it that traffic could not come back.
	n("chain=srcnat action=masquerade out-interface-list=WAN src-address="+lan.Network, "ftn: lan masquerade")
	for _, r := range w.NAT {
		nat = append(nat, r.line())
	}
	return filter, nat
}

var identityBad = regexp.MustCompile(`[^A-Za-z0-9-]+`)

func identityOf(name string) string {
	id := strings.Trim(identityBad.ReplaceAllString(name, "-"), "-")
	if id == "" {
		id = "router"
	}
	return "FTN-" + id
}

// RenderBootstrap renders the one script that builds a blank router. It is
// deterministic for fixed input (the golden-file tests rely on that) and holds
// the router's WireGuard private key, REST password and (pppoe) the PPPoE
// password: it is single use and removes itself.
func RenderBootstrap(in BootstrapInput) ([]byte, error) {
	s := in.Site
	if s.TunnelIP == "" || s.RESTPort <= 0 || in.PrivateKey == "" || in.RESTPass == "" {
		return nil, errors.New("bootstrap script: the router is missing its tunnel address, REST port or credentials")
	}
	if ap, err := netip.ParseAddr(s.TunnelIP); err != nil || !in.WGSubnet.Contains(ap) {
		return nil, fmt.Errorf("bootstrap script: tunnel address %s is outside %s", s.TunnelIP, in.WGSubnet)
	}
	mgmt := in.MgmtSubnet
	if !mgmt.IsValid() {
		mgmt = DefaultMgmtRange
	}
	mgmt = mgmt.Masked()
	if err := ValidateNetMgmt(s, in.WGSubnet, mgmt); err != nil {
		return nil, fmt.Errorf("bootstrap script: %w", err)
	}
	host, port, err := splitEndpoint(in.Endpoint)
	if err != nil {
		return nil, err
	}
	if !ValidIface(s.RESTUser) || s.RESTUser == "admin" {
		return nil, fmt.Errorf("bootstrap script: REST user %q is not a safe name", s.RESTUser)
	}
	if ip, err := netip.ParseAddr(in.PublicIP); err != nil || !ip.Is4() {
		return nil, errors.New("bootstrap script: PUBLIC_IP is not set on the server: the walled houses could not reach the payment page")
	}
	plan, err := PlanSubnet(s.CustomerSubnet)
	if err != nil {
		return nil, err
	}
	lan, err := PlanLAN(s.LANSubnet)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(in.PublicBase)
	if err != nil || u.Host == "" || strings.ContainsAny(in.PublicBase, `"$\ `) {
		return nil, fmt.Errorf("bootstrap script: PUBLIC_BASE_URL %q is not usable", in.PublicBase)
	}
	if s.WANMode == WANPPPoE && in.PPPoEPass == "" {
		return nil, errors.New("bootstrap script: the PPPoE password is not available")
	}
	if HasControl(in.PPPoEPass) || HasControl(s.PPPoEUser) || HasControl(s.Name) {
		return nil, errors.New("bootstrap script: a name or PPPoE value has a control character (line break or tab)")
	}
	serverIP := ServerTunnelIP(in.WGSubnet)
	d := bootstrapData{
		SiteID: s.ID, SiteName: s.Name, Generated: in.Generated.UTC().Format("2006-01-02 15:04 UTC"), Identity: identityOf(s.Name), ROSVersion: in.ROSVersion,
		WGIface: WGInterface, MTU: TunnelMTU, PrivateKey: in.PrivateKey, ServerPublicKey: in.ServerPub,
		TunnelIP: s.TunnelIP, ServerIP: serverIP, MgmtSubnet: mgmt.String(), EndpointHost: host, EndpointPort: port,
		RESTUser: s.RESTUser, RESTPass: in.RESTPass, RESTPort: s.RESTPort,
		WANIface: s.WANInterface, WANMode: s.WANMode, WANAddress: s.WANAddress, WANGateway: s.WANGateway,
		PPPoEUser: s.PPPoEUser, PPPoEPass: in.PPPoEPass, PPPoEIface: "pppoe-wan",
		LANPorts: SplitPorts(s.LANPorts), LAN: lan, LANBridge: LANBridge, CustBridge: CustomerBridge,
		OLTInterface: s.OLTInterface, CustIface: s.OLTInterface, VLANID: s.VLANID, CustomerPorts: SplitPorts(s.CustomerPorts),
		Plan: plan, DHCPServer: billing.DHCPServerName, UnregList: listUnreg, UnregPool: listUnreg,
		NetflowPort: in.NetflowPort, NoCheck: in.NoCheck, Token: in.Token,
		Files: []string{SetupFile, ResetFile},
	}
	dns := []string{"1.1.1.1", "8.8.8.8"}
	if s.WANMode == WANStatic {
		var own []string
		for _, x := range SplitPorts(s.WANDNS) {
			if x != "1.1.1.1" && x != "8.8.8.8" {
				own = append(own, x)
			}
		}
		dns = append(own, dns...)
	}
	d.DNSServers = strings.Join(dns, ",")
	if s.VLANID > 0 {
		d.VLANName = customerVLANName(s.VLANID)
		d.CustIface = d.VLANName
	}
	need := []string{s.WANInterface, s.OLTInterface}
	need = append(need, d.LANPorts...)
	need = append(need, d.CustomerPorts...)
	seen := map[string]bool{}
	var q []string
	for _, n := range need {
		if !seen[n] {
			seen[n] = true
			q = append(q, `"`+n+`"`)
		}
	}
	d.NeedPorts = "{" + strings.Join(q, ";") + "}"
	httpsBase := u.Scheme == "https"
	base := strings.TrimRight(in.PublicBase, "/")
	d.ReportPost = reportPostLine(base, in.Token, httpsBase, in.NoCheck)
	d.AdminAllow = AdminAllowFor(in.PublicBase, in.WebFigDomain, in.PublicIP)
	d.AdminList = AdminAllowList
	keep := []string{`"` + s.WANInterface + `"`}
	for _, p := range d.LANPorts {
		keep = append(keep, `"`+p+`"`)
	}
	d.KeepUp = "{" + strings.Join(keep, ";") + "}"
	var onu *ONUMgmtPlan
	if s.ONUMgmtN > 0 {
		op, err := PlanONUMgmt(s, serverIP, in.ACSPort, in.ACSURL, in.CRPorts)
		if err != nil {
			return nil, fmt.Errorf("bootstrap script: %w", err)
		}
		if err := checkONUOverlap(op, plan, lan, in.WGSubnet, mgmt, s); err != nil {
			return nil, fmt.Errorf("bootstrap script: %w", err)
		}
		onu = &op
		d.ONU, d.ONUSetup = onu, onu.SetupLines(ONUMgmtParent(s))
	}
	d.Filter, d.NAT = bootstrapFirewall(plan, lan, serverIP, mgmt.String(), in.PublicIP, s.RESTPort, onu)
	out, err := execScript(bootstrapTmpl, in.ROSVersion, d)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(out, []byte("{{")) || bytes.Contains(out, []byte("<no value>")) {
		return nil, errors.New("bootstrap script: unrendered template text left in the output")
	}
	return out, nil
}

func splitEndpoint(ep string) (string, int, error) {
	host, ps, err := net.SplitHostPort(ep)
	if err != nil {
		return "", 0, fmt.Errorf("WG_ENDPOINT %q must be host:port", ep)
	}
	var port int
	if _, err := fmt.Sscanf(ps, "%d", &port); err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("WG_ENDPOINT %q has a bad port", ep)
	}
	if strings.ContainsAny(host, "\"$\\ ") || host == "" {
		return "", 0, fmt.Errorf("WG_ENDPOINT %q has a bad host", ep)
	}
	return host, port, nil
}

// parseScript parses a script template; the section wrappers are placeholders
// here and replaced per render, see execScript.
func parseScript(name, src string) *template.Template {
	return template.Must(template.New(name).Funcs(template.FuncMap{"rq": rq}).Funcs(sectionFuncs("")).Option("missingkey=error").Parse(src))
}

// execScript renders t with the section wrappers for the router's RouterOS version.
func execScript(t *template.Template, rosVersion string, data any) ([]byte, error) {
	c, err := t.Clone()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := c.Funcs(sectionFuncs(rosVersion)).Execute(&b, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

var bootstrapTmpl = parseScript("bootstrap", bootstrapTmplSrc)

// ResetStubScript is the fixed text of ftn-reset.rsc, the second file of the
// paste line. It runs through /import, so the reset command asks nothing.
// BENCH-CHECK: run-after-reset, keep-users and the non-interactive reset on 7.25.1.
const ResetStubScript = `# Fast Tech Net: reset stub. It erases the router and runs ftn-setup.rsc, which builds it again.
:if ([:len [/file/find where name="ftn-setup.rsc"]] = 0) do={ :error "ftn: ftn-setup.rsc is missing, nothing was reset" }
:if ([/file/get [/file/find where name="ftn-setup.rsc"] size] < 1000) do={ :error "ftn: ftn-setup.rsc is empty or cut short, nothing was reset" }
:put "ftn: erasing the router; it restarts and builds itself from ftn-setup.rsc"
/system/reset-configuration no-defaults=yes skip-backup=yes keep-users=yes run-after-reset=ftn-setup.rsc
`

// BootstrapCommand is the one-line paste shown to the operator (Line A). The
// fallback skips certificate checking and is for routers with a wrong clock or
// no CA store; it asks for /j/{token}?nocheck=1 so the script's report skips
// the check too.
//
// A RouterOS in device-mode "home" blocks fetch, and changing that needs a
// button press and a reboot, so it is never done silently: the line checks
// first. If fetch is off it runs ONE /system/device-mode/update and says to
// paste the same line again; nothing else happens. Otherwise it turns the
// trust anchors on, removes stale files, fetches the two files (the second
// fetch needs no new token: the reset stub is served for ResetWindow after the
// setup fetch) and imports the stub. The fetches and the import are wrapped: if
// any of them fails the two files are removed and the router says that nothing
// was changed. A second paste is harmless: the token is single use, the fetch
// fails and nothing is reset. Note that the server has already made new keys for
// the router when the setup file was handed out, so a failure after the first
// fetch leaves the server and the router out of step until a new line is made;
// the Setup view says so (state "stalled").
// BENCH-CHECK: [/system/device-mode/get fetch] returns a bool or "yes"/"no"
// on 7.25.1 (both are accepted); the update command's argument name.
func BootstrapCommand(base, token string, checkCert bool) string {
	base = strings.TrimRight(base, "/")
	q, chk := "", "yes"
	if !checkCert {
		q, chk = NoCheckQuery, "no"
	}
	return fmt.Sprintf(`:local o "^(true|yes)\$"; :local d 0; :local v0 [:tostr [/system/device-mode/get fetch]]; :put ("ftn: device-mode fetch=" . $v0); :if ([:len $v0] = 0) do={ :put "ftn: cannot read device-mode, nothing was changed" } else={ :if ($v0 ~ $o) do={ :do { /certificate/settings/set builtin-trust-anchors=trusted } on-error={}; :do { :if (([:len [/ip/dns/get servers]] = 0) && ([:len [/ip/dns/get dynamic-servers]] = 0)) do={ /ip/dns/set servers=%[7]s; :set d 1; :put "ftn: the router had no DNS servers, set %[7]s"; :delay 1s } } on-error={}; :do { :resolve %[8]s } on-error={ :put "ftn: the router cannot look up %[8]s yet: check its internet connection" }; /file/remove [/file/find where name="%[1]s"]; /file/remove [/file/find where name="%[2]s"]; :do { /tool/fetch url="%[3]s/j/%[4]s%[5]s" dst-path=%[1]s check-certificate=%[6]s; /tool/fetch url="%[3]s/j/%[4]s/reset" dst-path=%[2]s check-certificate=%[6]s; :delay 1s; /import %[2]s } on-error={ /file/remove [/file/find where name="%[1]s"]; /file/remove [/file/find where name="%[2]s"]; :put "ftn: setup failed, nothing was changed, files removed"; :if ($d = 1) do={ :do { /ip/dns/set servers="" } on-error={}; :put "ftn: the DNS servers this line set were put back to empty" } } } else={ :put "ftn: press the router button within 5 minutes (it then reboots), then paste this same line again"; /system/device-mode/update fetch=yes } }`,
		SetupFile, ResetFile, base, token, q, chk, PasteLineDNS, hostOf(base))
}

// PasteLineDNS is what the paste line gives a router that has no DNS servers at all, so it can
// reach the server; the setup script then sets the router's real DNS servers in section 1.
const PasteLineDNS = "1.1.1.1,8.8.8.8"

// hostOf is the host name of the server's base URL, for the paste line's look-up check.
func hostOf(base string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if i := strings.IndexAny(h, "/:"); i >= 0 {
		h = h[:i]
	}
	return h
}

// NoCheckQuery is the query string of the no-certificate-check command.
const NoCheckQuery = "?nocheck=1"
