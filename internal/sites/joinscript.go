package sites

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"text/template"
	"time"

	"ispbill/internal/store"
)

//go:embed join.rsc.tmpl
var joinTmplSrc string

// PageFiles are the hotspot login pages the router fetches one by one.
var PageFiles = []string{"login.html", "alogin.html", "status.html", "logout.html", "rlogin.html", "error.html"}

// ScriptInput is everything the server decides before rendering the router
// script. The script has no router-side variables to fill in.
type ScriptInput struct {
	Site       store.Site // with defaults applied
	PrivateKey string     // the router's WireGuard private key (never stored)
	RESTPass   string
	ServerPub  string // the server's WireGuard public key
	Endpoint   string // host:port the router dials
	WGSubnet   netip.Prefix
	PublicBase string // https://billing.example.co.ke
	PublicIP   string
	Generated  time.Time
	// NoCheck is set when the admin used the "no certificate check" command
	// (router with a wrong clock or no CA store): the page downloads in
	// section 6 then skip the check too, or they would fail for the same reason.
	NoCheck bool
	// Stage picks the script: "" is the legacy all-in-one script (hotspot
	// buildings); "join" joins the router to the server and nothing else;
	// "cutover" switches the flats from the hotspot to server-managed IPoE;
	// "cleanup" deletes the disabled hotspot objects after the cutover was
	// confirmed. See ipoe_script.go.
	Stage string
	// Observe: the cutover leaves the unregistered range unwalled and suspends nobody.
	Observe bool
	// Force: the admin chose to cut over although the preflight found paying
	// flats not seen or devices the server does not know; the script then keeps them.
	Force bool
	// Accounts are the flats the cutover script writes (static lease and
	// queue each), rendered from the accounts table.
	Accounts []LeaseSeed
	// ROSVersion is the router's RouterOS version from the fetch User-Agent
	// ("7.25.1"); 7.13 and newer get ":onerror" section wrappers with the real
	// error text, anything else the old form. See secwrap.go.
	ROSVersion string
	// Token is the join token the router fetched with; the join scripts use it
	// to POST their report to /j/{token}/report.
	Token string
}

// scriptData is the template view of ScriptInput.
type scriptData struct {
	SiteID, SiteName, Generated                             string
	WGIface                                                 string
	MTU                                                     int
	PrivateKey, ServerPublicKey                             string
	TunnelIP, ServerIP                                      string
	EndpointHost                                            string
	EndpointPort                                            int
	RESTUser, RESTPass                                      string
	RESTPort                                                int
	LegacyContainer, Provision                              bool
	CustomerNetwork, Gateway, GatewayCIDR, PoolFrom, PoolTo string
	OLTInterface, WANList, Bridge, CustomerIface            string
	VLANID                                                  int
	VLANName                                                string
	BenchPort                                               string
	HotspotServer, HotspotProfile                           string
	PublicBase, PublicIP                                    string
	HTTPSBase                                               bool
	PageFiles                                               []string
	NetflowPort                                             int
	NoCheck                                                 bool
	Token, ROSVersion                                       string
	ReportPost                                              string // the router-side line that sends $report to the server
	KitN                                                    int    // the kit section's number
	KitTick, KitRollback                                    string // the kit scripts, escaped for a quoted string
	KitTickName, KitRollbackName, KitTimerName              string
	KitInterval, KitComment, KitScriptPolicy, KitTickPolicy string
}

// fillKit sets the fixed kit fields; n is the kit section's number.
func (d *scriptData) fillKit(n int, hotspot string) {
	d.KitN = n
	d.KitTick, d.KitRollback = rqs(kitTickSource()), rqs(kitRollbackSource(hotspot))
	d.KitTickName, d.KitRollbackName, d.KitTimerName = KitTickScript, KitRollback, KitTimer
	d.KitInterval, d.KitComment, d.KitScriptPolicy, d.KitTickPolicy = KitInterval, KitComment, kitScriptPolic, kitTickPolicy
}

// rq escapes a value for use inside a RouterOS double-quoted string.
func rq(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(s)
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`).Replace(s)
}

// fw renders one firewall rule that goes to the top of the filter table. The
// guard is needed because place-before=0 fails on an empty table. Only
// static rules are counted and chosen: the first row can be a dynamic rule
// (the FastTrack dummy), and RouterOS refuses to place a rule before one,
// which would abort the whole script. Rules are added bottom-up, so the last
// one added ends up first.
func fw(args, comment string) string {
	const f = "/ip/firewall/filter"
	return fmt.Sprintf("  :if ([:len [%s find where dynamic=no]] > 0) do={ %s add %s place-before=([%s find where dynamic=no]->0) comment=\"%s\" } else={ %s add %s comment=\"%s\" }",
		f, f, args, f, rq(comment), f, args, rq(comment))
}

//go:embed kit_section.rsc.tmpl
var kitSectionSrc string

// parseScript parses a script template with its helper functions (the section
// wrappers are placeholders here and replaced per render, see execScript) and
// the shared kit section.
func parseScript(name, src string) *template.Template {
	t := template.New(name).Funcs(template.FuncMap{"rq": rq, "fw": fw}).Funcs(sectionFuncs("")).Option("missingkey=error")
	return template.Must(template.Must(t.Parse(src)).Parse(kitSectionSrc))
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

var joinTmpl = parseScript("join", joinTmplSrc)

// RenderJoinScript renders the router script. It is deterministic for fixed
// input (the golden-file tests rely on that).
func RenderJoinScript(in ScriptInput) ([]byte, error) {
	switch in.Stage {
	case "":
	case StageJoin, StageCutover, StageCleanup:
		return renderStage(in)
	default:
		return nil, fmt.Errorf("join script: unknown stage %q", in.Stage)
	}
	s := in.Site
	if s.TunnelIP == "" || s.RESTPort <= 0 || in.PrivateKey == "" || in.RESTPass == "" {
		return nil, errors.New("join script: site is missing its tunnel address, REST port or credentials")
	}
	if !in.WGSubnet.Contains(netip.MustParseAddr(s.TunnelIP)) {
		return nil, fmt.Errorf("join script: tunnel address %s is outside %s", s.TunnelIP, in.WGSubnet)
	}
	host, port, err := splitEndpoint(in.Endpoint)
	if err != nil {
		return nil, err
	}
	for _, f := range []struct{ label, v string }{{"WAN list", s.WANList}, {"OLT interface", s.OLTInterface}, {"hotspot server", s.HotspotServer}, {"REST user", s.RESTUser}} {
		if !ValidIface(f.v) {
			return nil, fmt.Errorf("join script: %s %q is not a safe name", f.label, f.v)
		}
	}
	if s.BenchPort != "" && !ValidIface(s.BenchPort) {
		return nil, fmt.Errorf("join script: bench port %q is not a safe name", s.BenchPort)
	}
	plan, err := PlanSubnet(s.CustomerSubnet)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(in.PublicBase)
	if err != nil || u.Host == "" || strings.ContainsAny(in.PublicBase, `"$\ `) {
		return nil, fmt.Errorf("join script: PUBLIC_BASE_URL %q is not usable", in.PublicBase)
	}
	d := scriptData{
		SiteID: s.ID, SiteName: s.Name, Generated: in.Generated.UTC().Format("2006-01-02 15:04 UTC"),
		WGIface: WGInterface, MTU: TunnelMTU, PrivateKey: in.PrivateKey, ServerPublicKey: in.ServerPub,
		TunnelIP: s.TunnelIP, ServerIP: ServerTunnelIP(in.WGSubnet), EndpointHost: host, EndpointPort: port,
		RESTUser: s.RESTUser, RESTPass: in.RESTPass, RESTPort: s.RESTPort,
		LegacyContainer: s.LegacyContainer, Provision: s.Provision,
		CustomerNetwork: plan.Network, Gateway: plan.Gateway, GatewayCIDR: plan.GatewayIP, PoolFrom: plan.PoolFrom, PoolTo: plan.PoolTo,
		OLTInterface: s.OLTInterface, WANList: s.WANList, Bridge: HotspotBridge, CustomerIface: s.OLTInterface,
		VLANID: s.VLANID, BenchPort: s.BenchPort, HotspotServer: s.HotspotServer, HotspotProfile: HotspotProf,
		PublicBase: strings.TrimRight(in.PublicBase, "/"), PublicIP: in.PublicIP, HTTPSBase: u.Scheme == "https",
		PageFiles: PageFiles, NetflowPort: 2055, NoCheck: in.NoCheck,
		Token: in.Token, ROSVersion: in.ROSVersion,
	}
	d.ReportPost = reportPostLine(d.PublicBase, in.Token, d.HTTPSBase, in.NoCheck)
	d.fillKit(7, s.HotspotServer)
	if s.VLANID > 0 {
		d.VLANName = fmt.Sprintf("vlan%d-customers", s.VLANID)
		d.CustomerIface = d.VLANName
	}
	out, err := execScript(joinTmpl, in.ROSVersion, d)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(out, []byte("{{")) {
		return nil, errors.New("join script: unrendered template text left in the output")
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

// JoinCommand is the one-line paste shown to the admin. The fallback skips
// certificate checking and is for routers with a wrong clock or no CA store.
// It asks for /j/{token}?nocheck=1 so the script it receives skips the check
// for its own page downloads as well.
func JoinCommand(base, token string, checkCert bool) string {
	return JoinCommandStage(base, token, checkCert, "")
}

// JoinCommandStage is JoinCommand for a stage of the IPoE scripts: the file is
// ftn-<stage>.rsc and the link asks for ?stage=<stage>. The legacy stage ("")
// is the hotspot building's script, so its command also checks the hotspot
// feature of device-mode.
func JoinCommandStage(base, token string, checkCert bool, stage string) string {
	return JoinCommandFor(base, token, checkCert, stage, stage == "")
}

// JoinCommandFor builds the pasted line. A RouterOS in device-mode "home"
// blocks fetch, scheduler and hotspot, and changing that needs a button press
// and a reboot, so it is never done by a script and never silently: the line
// itself checks device-mode first. If a feature the script needs is off, it
// runs ONE /system/device-mode/update (the router then waits for the button)
// and says to paste the same line again; otherwise it fetches and imports.
// The cleanup stage needs no scheduler. hotspot adds the hotspot feature.
// BENCH-CHECK: [/system/device-mode/get <feature>] returns a bool or "yes"/"no"
// on 7.25.1 (both are accepted); the update command's argument names.
func JoinCommandFor(base, token string, checkCert bool, stage string, hotspot bool) string {
	file, q := "ftn-join.rsc", ""
	if stage != "" {
		file, q = "ftn-"+stage+".rsc", "?stage="+stage
	}
	chk := "yes"
	if !checkCert {
		chk = "no"
		if q == "" {
			q = NoCheckQuery
		} else {
			q += "&nocheck=1"
		}
	}
	feats := []string{"fetch"}
	if stage != StageCleanup {
		feats = append(feats, "scheduler")
	}
	if hotspot {
		feats = append(feats, "hotspot")
	}
	var conds, sets []string
	for _, f := range feats {
		conds = append(conds, fmt.Sprintf(`([:tostr [/system/device-mode/get %s]] ~ $o)`, f))
		sets = append(sets, f+"=yes")
	}
	return fmt.Sprintf(`:local o "^(true|yes)$"; :if (%s) do={ /tool/fetch url="%s/j/%s%s" dst-path=%s check-certificate=%s; :delay 1s; /import %s } else={ :put "ftn: press the router button within 5 minutes (it then reboots), then paste this same line again"; /system/device-mode/update %s }`,
		strings.Join(conds, " && "), strings.TrimRight(base, "/"), token, q, file, chk, file,
		strings.Join(sets, " "))
}

// NoCheckQuery is the query string of the no-certificate-check join command.
const NoCheckQuery = "?nocheck=1"

// JoinCommandForce is JoinCommandStage for a cutover the admin chose to run
// although some paying flats have not been seen on the router yet.
func JoinCommandForce(base, token string, checkCert bool, stage string) string {
	cmd := JoinCommandStage(base, token, checkCert, stage)
	return strings.Replace(cmd, "?stage="+stage, "?stage="+stage+"&force=1", 1)
}
