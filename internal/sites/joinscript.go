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

var joinTmpl = template.Must(template.New("join").Funcs(template.FuncMap{"rq": rq, "fw": fw}).Option("missingkey=error").Parse(joinTmplSrc))

// RenderJoinScript renders the router script. It is deterministic for fixed
// input (the golden-file tests rely on that).
func RenderJoinScript(in ScriptInput) ([]byte, error) {
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
		PageFiles: PageFiles, NetflowPort: 2055,
	}
	if s.VLANID > 0 {
		d.VLANName = fmt.Sprintf("vlan%d-customers", s.VLANID)
		d.CustomerIface = d.VLANName
	}
	var b bytes.Buffer
	if err := joinTmpl.Execute(&b, d); err != nil {
		return nil, err
	}
	if bytes.Contains(b.Bytes(), []byte("{{")) {
		return nil, errors.New("join script: unrendered template text left in the output")
	}
	return b.Bytes(), nil
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
func JoinCommand(base, token string, checkCert bool) string {
	chk, q := "yes", ""
	if !checkCert {
		chk, q = "no", ""
	}
	return fmt.Sprintf(`/tool/fetch url="%s/j/%s%s" dst-path=ftn-join.rsc check-certificate=%s; :delay 1s; /import ftn-join.rsc`,
		strings.TrimRight(base, "/"), token, q, chk)
}
