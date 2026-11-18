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
	// REST user, traffic flow). No customer is affected.
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
	Suspended                       []LeaseSeed
	Filter                          []string // firewall filter rules, ready to paste, last-added-first
	NAT                             []string // dst-nat rules, same
	Masquerade                      bool
	RollbackSource, TickSource      string
	TickLen                         int
	WaitNote                        string
}

var stageFuncs = template.FuncMap{"rq": rq, "fw": fw}

var (
	ipoeJoinTmpl    = template.Must(template.New("ipoe_join").Funcs(stageFuncs).Option("missingkey=error").Parse(ipoeJoinSrc))
	ipoeCutoverTmpl = template.Must(template.New("ipoe_cutover").Funcs(stageFuncs).Option("missingkey=error").Parse(ipoeCutoverSrc))
	ipoeCleanupTmpl = template.Must(template.New("ipoe_cleanup").Funcs(stageFuncs).Option("missingkey=error").Parse(ipoeCleanupSrc))
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
	b.WriteString(g(`/ip hotspot enable [find where name="` + hotspot + `"]`))
	b.WriteString(g(`/ip firewall filter remove [find where comment~"^ftn: ipoe"]`))
	b.WriteString(g(`/ip firewall nat remove [find where comment~"^ftn: ipoe"]`))
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
			if ip, err := netip.ParseAddr(a.IP); err != nil || !ip.Is4() || !plan.InNetwork(a.IP) {
				return nil, fmt.Errorf("cutover script: flat %q has no address inside %s", a.Flat, plan.Network)
			}
			d.Accounts = append(d.Accounts, a)
			if a.Suspended {
				d.Suspended = append(d.Suspended, a)
			}
		}
		d.PublicIP = in.PublicIP
		d.RollbackSource = rqs(rollbackBase(s.HotspotServer))
		d.TickSource = rqs(tickSource())
		d.TickLen = len(timerPrefix)
		d.Filter, d.NAT = ipoeRules(plan, in.PublicIP)
		tmpl = ipoeCutoverTmpl
	case StageCleanup:
		tmpl = ipoeCleanupTmpl
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, d); err != nil {
		return nil, err
	}
	if bytes.Contains(b.Bytes(), []byte("{{")) || bytes.Contains(b.Bytes(), []byte("<no value>")) {
		return nil, errors.New("join script: unrendered template text left in the output")
	}
	return b.Bytes(), nil
}

// ipoeRules builds the walled-garden rules. Each is placed at the top of its
// table (above FastTrack), so they are rendered last-wanted-first: the rule
// added last ends up first.
//
// Top to bottom in the forward chain, for each walled list (unregistered
// devices, flats with no paid time):
//
//	accept DNS (udp, tcp 53)
//	accept tcp 80 and 443 to PUBLIC_IP: the payment page
//	reject tcp with a reset (HTTPS fails at once), reject everything else
//
// then established and related traffic of the customer network is accepted
// above FastTrack, so the per-flat queues see it (FastTrack would bypass them).
// Walled flats are matched first, so cutting a flat off also ends its open
// connections.
func ipoeRules(plan netplan.Plan, publicIP string) (filter, nat []string) {
	lists := []string{listUnreg, billing.SuspendedList}
	type rule struct{ args, comment string }
	var top []rule // top to bottom
	for _, l := range lists {
		top = append(top, rule{"chain=forward action=accept protocol=udp dst-port=53 src-address-list=" + l, "ftn: ipoe walled dns udp " + l})
	}
	for _, l := range lists {
		top = append(top, rule{"chain=forward action=accept protocol=tcp dst-port=53 src-address-list=" + l, "ftn: ipoe walled dns tcp " + l})
	}
	for _, l := range lists {
		top = append(top, rule{"chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=" + publicIP + " src-address-list=" + l, "ftn: ipoe walled payment page " + l})
	}
	for _, l := range lists {
		top = append(top, rule{"chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=" + l, "ftn: ipoe walled reject tcp " + l})
	}
	for _, l := range lists {
		top = append(top, rule{"chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=" + l, "ftn: ipoe walled reject " + l})
	}
	top = append(top,
		rule{"chain=forward action=accept connection-state=established,related src-address=" + plan.Network, "ftn: ipoe customer est/rel out (above FastTrack, so queues apply)"},
		rule{"chain=forward action=accept connection-state=established,related dst-address=" + plan.Network, "ftn: ipoe customer est/rel in (above FastTrack, so queues apply)"},
	)
	for i := len(top) - 1; i >= 0; i-- {
		filter = append(filter, fw(top[i].args, top[i].comment))
	}
	// Input: flats may use the router's DNS (the DHCP server hands out the gateway as the resolver).
	filter = append([]string{
		fw("chain=input action=accept protocol=tcp dst-port=53 in-interface=$ifc", "ftn: ipoe dns tcp"),
		fw("chain=input action=accept protocol=udp dst-port=53 in-interface=$ifc", "ftn: ipoe dns udp"),
	}, filter...)
	// The walled flats' plain web requests go to the payment page.
	for i := len(lists) - 1; i >= 0; i-- {
		nat = append(nat, fwOn("/ip/firewall/nat", "chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!"+publicIP+" src-address-list="+lists[i]+" to-addresses="+publicIP+" to-ports=80",
			"ftn: ipoe walled http to the payment page "+lists[i]))
	}
	return filter, nat
}
