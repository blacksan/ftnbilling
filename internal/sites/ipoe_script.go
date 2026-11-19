package sites

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"text/template"

	"ispbill/internal/billing"
	"ispbill/internal/netplan"
	"ispbill/internal/store"
)

// Stages of the IPoE rollout. Each is its own one-time command from the
// building page, and none of them touches the WAN interface, the routes, the
// router's users, or any service other than www.
const (
	// StageJoin brings the router to the server (WireGuard, tunnel firewall,
	// REST user, cutover kit). No customer is affected.
	StageJoin = "join"
	// StageCutover switches the flats from the hotspot to server-managed IPoE.
	StageCutover = "cutover"
	// StageCleanup deletes the disabled hotspot objects, a week after the cutover.
	StageCleanup = "cleanup"

	// RollbackMinutes: without "Confirm cutover" the router goes back to the
	// hotspot by itself after this long.
	RollbackMinutes = 15

	// Names on the router, shared with the server (Confirm cutover removes the timer).
	RollbackScript   = "ftn-rollback"
	RollbackTick     = "ftn-rollback-tick"
	RollbackSchedule = "ftn-rollback-timer"
	timerPrefix      = "ftn: rollback timer " // the scheduler's comment is this plus the tick count

	// The two address lists that mean "walled": unregistered devices (the
	// ftn-unreg pool range) and flats with no paid time.
	listUnreg = "ftn-unreg"
)

//go:embed ipoe_join.rsc.tmpl
var ipoeJoinSrc string

//go:embed ipoe_cutover.rsc.tmpl
var ipoeCutoverSrc string

//go:embed ipoe_cleanup.rsc.tmpl
var ipoeCleanupSrc string

// LeaseSeed is one flat as the cutover script writes it: a static lease, a
// queue, and (when not entitled) an entry on the suspended list.
type LeaseSeed struct {
	Code, Flat, MAC, IP string
	MaxLimit            string // "10M/10M"
	Suspended           bool
}

var (
	codeRe  = regexp.MustCompile(`^[A-Z0-9]{3,12}$`)
	limitRe = regexp.MustCompile(`^\d{1,5}M/\d{1,5}M$`)
)

// stageData is the template view of the IPoE stages.
type stageData struct {
	scriptData
	Stage                           string
	File                            string
	RollbackMin                     int
	DHCPServer, UnregList, SuspList string
	UnregFrom, UnregTo              string
	StaticFrom                      string
	Accounts                        []LeaseSeed
	Observe, Force                  bool
	Known                           string // the flats' MACs, comma separated: everything else on the interface is a device to keep
	IsolateRule, BlockRestRule      string
	Suspended                       []LeaseSeed
	Filter                          []string // firewall filter rules, ready to paste, last-added-first
	NAT                             []string // dst-nat rules, same
	Masquerade                      bool
	RollbackSource, TickSource      string
	TickLen                         int
	WaitNote                        string
	ArpKinds                        []string // interface menus whose arp mode the switch changes
}

var (
	ipoeJoinTmpl    = parseScript("ipoe_join", ipoeJoinSrc)
	ipoeCutoverTmpl = parseScript("ipoe_cutover", ipoeCutoverSrc)
	ipoeCleanupTmpl = parseScript("ipoe_cleanup", ipoeCleanupSrc)
)

// fwOn is fw for another table (the NAT table): the same guard, the same
// "static rules only" rule.
func fwOn(path, args, comment string) string {
	return fmt.Sprintf("  :if ([:len [%s find where dynamic=no]] > 0) do={ %s add %s place-before=([%s find where dynamic=no]->0) comment=\"%s\" } else={ %s add %s comment=\"%s\" }",
		path, path, args, path, rq(comment), path, args, rq(comment))
}

// rqs escapes a multi-line script body for a RouterOS quoted string: the
// router script stored with /system/script add source="...".
func rqs(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`).Replace(s)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// rollbackBase is the first version of the ftn-rollback script: it switches
// the building back to the hotspot. The cutover script appends the lines for
// the old DHCP servers once it has found them on the router. Every line is
// guarded, so one missing object cannot stop the rest.
func rollbackBase(hotspot string) string {
	g := func(cmd string) string { return ":do { " + cmd + " } on-error={}\n" }
	var b strings.Builder
	b.WriteString(":log warning \"ftn: IPoE rollback running\"\n")
	b.WriteString(g(`/ip dhcp-server disable [find where name="` + billing.DHCPServerName + `"]`)) // before the old server is enabled: one server per interface
	// nobody is walled while ftn-dhcp is off: the walled rules reject these lists (the server's sweep does the same for kits that lack this line)
	b.WriteString(g(`/ip firewall address-list disable [find where list~"^ftn-(unreg|suspended)\$"]`))
	b.WriteString(g(`/ip hotspot enable [find where name="` + hotspot + `"]`))
	b.WriteString(g(`/ip firewall filter remove [find where comment~"^ftn: ipoe"]`))
	b.WriteString(g(`/ip firewall nat remove [find where comment~"^ftn: ipoe"]`))
	b.WriteString(g(`/ipv6 firewall filter remove [find where comment~"^ftn: ipoe"]`))
	b.WriteString(g(`/ip arp remove [find where comment~"^ftn:acct:"]`))
	b.WriteString(g(`/ip arp remove [find where comment="ftn: keep"]`))
	b.WriteString(g(`/ip dhcp-server lease remove [find where comment="ftn: keep"]`))
	b.WriteString(g(`/queue simple remove [find where comment~"^ftn:acct:"]`))
	b.WriteString(g(`/system scheduler remove [find where name="` + RollbackSchedule + `"]`))
	return b.String()
}

// tickSource runs every minute. It counts in the scheduler's own comment (so
// the count survives a reboot) and runs the rollback on the RollbackMinutes-th tick.
func tickSource() string {
	return fmt.Sprintf(`:local id [/system scheduler find where name="%s"]
:if ([:len $id] = 0) do={ :error "no timer" }
:local c [/system scheduler get $id comment]
:local n [:tonum [:pick $c %d [:len $c]]]
:set n ($n + 1)
:if ($n >= %d) do={ /system script run %s } else={ /system scheduler set $id comment=("%s" . $n) }
`, RollbackSchedule, len(timerPrefix), RollbackMinutes, RollbackScript, timerPrefix)
}

func renderStage(in ScriptInput) ([]byte, error) {
	s := in.Site
	if s.TunnelIP == "" || s.RESTPort <= 0 {
		return nil, errors.New("join script: site is missing its tunnel address or REST port")
	}
	if !in.WGSubnet.Contains(netip.MustParseAddr(s.TunnelIP)) {
		return nil, fmt.Errorf("join script: tunnel address %s is outside %s", s.TunnelIP, in.WGSubnet)
	}
	for _, f := range []struct{ label, v string }{{"WAN list", s.WANList}, {"OLT interface", s.OLTInterface}, {"hotspot server", s.HotspotServer}, {"REST user", s.RESTUser}} {
		if !ValidIface(f.v) {
			return nil, fmt.Errorf("join script: %s %q is not a safe name", f.label, f.v)
		}
	}
	plan, err := PlanSubnet(s.CustomerSubnet)
	if err != nil {
		return nil, err
	}
	host, port, err := splitEndpoint(in.Endpoint)
	if err != nil && in.Stage == StageJoin {
		return nil, err
	}
	d := stageData{Stage: in.Stage, File: "ftn-" + in.Stage + ".rsc", RollbackMin: RollbackMinutes,
		DHCPServer: billing.DHCPServerName, UnregList: listUnreg, SuspList: billing.SuspendedList,
		UnregFrom: plan.UnregFrom, UnregTo: plan.UnregTo, StaticFrom: plan.StaticFrom}
	d.scriptData = scriptData{
		SiteID: s.ID, SiteName: s.Name, Generated: in.Generated.UTC().Format("2006-01-02 15:04 UTC"),
		WGIface: WGInterface, MTU: TunnelMTU, PrivateKey: in.PrivateKey, ServerPublicKey: in.ServerPub,
		TunnelIP: s.TunnelIP, ServerIP: ServerTunnelIP(in.WGSubnet), EndpointHost: host, EndpointPort: port,
		RESTUser: s.RESTUser, RESTPass: in.RESTPass, RESTPort: s.RESTPort,
		CustomerNetwork: plan.Network, Gateway: plan.Gateway, GatewayCIDR: plan.GatewayIP, PoolFrom: plan.PoolFrom, PoolTo: plan.PoolTo,
		OLTInterface: s.OLTInterface, WANList: s.WANList, HotspotServer: s.HotspotServer, HotspotProfile: HotspotProf,
		PublicBase: strings.TrimRight(in.PublicBase, "/"), PublicIP: in.PublicIP, NetflowPort: 2055, NoCheck: in.NoCheck,
		Token: in.Token, ROSVersion: in.ROSVersion, HTTPSBase: strings.HasPrefix(in.PublicBase, "https://"),
	}
	d.ReportPost = reportPostLine(d.PublicBase, in.Token, d.HTTPSBase, in.NoCheck)
	d.fillKit(4, s.HotspotServer)
	if in.ROSVersion == "" {
		if v, ok := stageROS.Load(s.ID); ok {
			in.ROSVersion = v.(string)
		}
		d.ROSVersion = in.ROSVersion
	}
	var tmpl *template.Template
	switch in.Stage {
	case StageJoin:
		if in.PrivateKey == "" || in.RESTPass == "" {
			return nil, errors.New("join script: site is missing its tunnel keys or REST credentials")
		}
		tmpl = ipoeJoinTmpl
	case StageCutover:
		if _, err := netip.ParseAddr(in.PublicIP); err != nil {
			return nil, errors.New("cutover script: PUBLIC_IP is not set on the server: the walled flats could not reach the payment page")
		}
		for _, a := range in.Accounts {
			if !codeRe.MatchString(a.Code) || !limitRe.MatchString(a.MaxLimit) {
				return nil, fmt.Errorf("cutover script: flat %q has an unusable code or speed", a.Flat)
			}
			if _, ok := store.CanonMAC(a.MAC); !ok {
				return nil, fmt.Errorf("cutover script: flat %q has no valid MAC", a.Flat)
			}
			if ip, err := netip.ParseAddr(a.IP); err != nil || !ip.Is4() || !plan.InStatic(a.IP) {
				return nil, fmt.Errorf("cutover script: flat %q has no address in the static range %s-%s (the range below it is the unregistered pool, which is walled)", a.Flat, plan.StaticFrom, plan.StaticTo)
			}
			d.Accounts = append(d.Accounts, a)
			if a.Suspended {
				d.Suspended = append(d.Suspended, a)
			}
		}
		d.PublicIP, d.Observe, d.Force = in.PublicIP, in.Observe, in.Force
		var macs []string
		for _, a := range in.Accounts {
			c, _ := store.CanonMAC(a.MAC)
			macs = append(macs, c)
		}
		d.Known = strings.Join(macs, ",")
		d.RollbackSource = rqs(rollbackBase(s.HotspotServer))
		d.TickSource = rqs(tickSource())
		d.TickLen = len(timerPrefix)
		d.ArpKinds = []string{"bridge", "vlan", "ethernet"}
		d.Filter, d.NAT = ipoeRules(plan, in.PublicIP, s.RESTPort)
		tmpl = ipoeCutoverTmpl
	case StageCleanup:
		d.IsolateRule, d.BlockRestRule = isolateRule(), blockRestRule(s.RESTPort)
		tmpl = ipoeCleanupTmpl
	}
	out, err := execScript(tmpl, in.ROSVersion, d)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(out, []byte("{{")) || bytes.Contains(out, []byte("<no value>")) {
		return nil, errors.New("join script: unrendered template text left in the output")
	}
	return out, nil
}

func isolateRule() string {
	return fw("chain=forward action=drop in-interface=$ifc out-interface=$ifc", "ftn: ipoe isolate flats")
}

func blockRestRule(port int) string {
	return fw(fmt.Sprintf("chain=input action=drop protocol=tcp in-interface=$ifc dst-port=%d", port), "ftn: ipoe block rest")
}

// ipoeRule is one firewall or NAT rule of the walled garden as data: it is
// rendered to a pasteable script line (ipoeRules, the frozen paste scripts) and
// to a REST body (Body, the server-run provisioning job).
type ipoeRule struct {
	Table   string // "filter" or "nat"
	Props   [][2]string
	Comment string
}

// newIpoeRule parses "k=v k=v" (values hold no spaces).
func newIpoeRule(table, args, comment string) ipoeRule {
	r := ipoeRule{Table: table, Comment: comment}
	for _, f := range strings.Fields(args) {
		k, v, _ := strings.Cut(f, "=")
		r.Props = append(r.Props, [2]string{k, v})
	}
	return r
}

// Args is the rule as "k=v k=v" (the script text; $ifc is the script's interface variable).
func (r ipoeRule) Args() string {
	var b strings.Builder
	for i, p := range r.Props {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p[0] + "=" + p[1])
	}
	return b.String()
}

// Path is the REST path of the rule's table.
func (r ipoeRule) Path() string {
	if r.Table == "nat" {
		return "/ip/firewall/nat"
	}
	return "/ip/firewall/filter"
}

// Body is the REST body of the rule for the customer interface iface ($ifc is replaced).
func (r ipoeRule) Body(iface string) map[string]string {
	out := map[string]string{"comment": r.Comment}
	for _, p := range r.Props {
		out[p[0]] = strings.ReplaceAll(p[1], "$ifc", iface)
	}
	return out
}

func (r ipoeRule) script() string {
	if r.Table == "nat" {
		return fwOn("/ip/firewall/nat", r.Args(), r.Comment)
	}
	return fw(r.Args(), r.Comment)
}

// ipoeRuleSet builds the walled-garden rules, each table top to bottom.
//
// Forward chain, top to bottom, for each walled list (unregistered devices,
// flats with no paid time): accept tcp 80/443 to PUBLIC_IP (the payment page),
// reject tcp with a reset (HTTPS fails at once), reject everything else. DNS
// is not forwarded: it is redirected to the router (nat). Then established and
// related traffic of the customer network is accepted above FastTrack, so the
// per-flat queues see it, and flat-to-flat traffic is dropped.
// Input chain: the REST port is blocked from the flats and a walled flat may
// only use the router's DNS, DHCP and ping.
func ipoeRuleSet(plan netplan.Plan, publicIP string, restPort int) (filter, nat []ipoeRule) {
	lists := []string{listUnreg, billing.SuspendedList}
	f := func(args, comment string) ipoeRule { return newIpoeRule("filter", args, comment) }
	var fwd []ipoeRule
	for _, l := range lists {
		fwd = append(fwd, f("chain=forward action=accept protocol=tcp dst-port=80,443 dst-address="+publicIP+" src-address-list="+l, "ftn: ipoe walled payment page "+l))
	}
	for _, l := range lists {
		fwd = append(fwd, f("chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list="+l, "ftn: ipoe walled reject tcp "+l))
	}
	for _, l := range lists {
		fwd = append(fwd, f("chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list="+l, "ftn: ipoe walled reject "+l))
	}
	fwd = append(fwd,
		f("chain=forward action=accept connection-state=established,related src-address="+plan.Network, "ftn: ipoe customer est/rel out (above FastTrack, so queues apply)"),
		f("chain=forward action=accept connection-state=established,related dst-address="+plan.Network, "ftn: ipoe customer est/rel in (above FastTrack, so queues apply)"),
		f("chain=forward action=drop in-interface=$ifc out-interface=$ifc", "ftn: ipoe isolate flats"),
	)
	in := []ipoeRule{
		f(fmt.Sprintf("chain=input action=drop protocol=tcp in-interface=$ifc dst-port=%d", restPort), "ftn: ipoe block rest"),
		f("chain=input action=accept protocol=udp dst-port=53 in-interface=$ifc", "ftn: ipoe dns udp"),
		f("chain=input action=accept protocol=tcp dst-port=53 in-interface=$ifc", "ftn: ipoe dns tcp"),
	}
	for _, l := range lists {
		in = append(in,
			f("chain=input action=accept protocol=udp dst-port=67 src-address-list="+l, "ftn: ipoe walled dhcp "+l),
			f("chain=input action=accept protocol=icmp src-address-list="+l, "ftn: ipoe walled icmp "+l),
			f("chain=input action=drop src-address-list="+l, "ftn: ipoe walled input drop "+l),
		)
	}
	filter = append(fwd, in...) // top to bottom of the table (the chains do not interact)
	n := func(args, comment string) ipoeRule { return newIpoeRule("nat", args, comment) }
	for _, l := range lists {
		nat = append(nat,
			n("chain=dstnat action=redirect protocol=udp dst-port=53 src-address-list="+l+" to-ports=53", "ftn: ipoe walled dns redirect udp "+l),
			n("chain=dstnat action=redirect protocol=tcp dst-port=53 src-address-list="+l+" to-ports=53", "ftn: ipoe walled dns redirect tcp "+l),
		)
	}
	for _, l := range lists {
		nat = append(nat, n("chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!"+publicIP+" src-address-list="+l+" to-addresses="+publicIP+" to-ports=80",
			"ftn: ipoe walled http to the payment page "+l))
	}
	return filter, nat
}

// ipoeRules renders the walled-garden rules as script lines. Each rule is
// placed at the top of its table (above FastTrack), so they are rendered
// last-wanted-first: the rule added last ends up first.
func ipoeRules(plan netplan.Plan, publicIP string, restPort int) (filter, nat []string) {
	fr, nr := ipoeRuleSet(plan, publicIP, restPort)
	for i := len(fr) - 1; i >= 0; i-- {
		filter = append(filter, fr[i].script())
	}
	for i := len(nr) - 1; i >= 0; i-- {
		nat = append(nat, nr[i].script())
	}
	return filter, nat
}
