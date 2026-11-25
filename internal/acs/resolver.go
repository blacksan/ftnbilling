package acs

import (
	"sort"
	"strings"

	"ispbill/internal/store"
)

// The resolver: given the parameter tree walked from one ONU (paths, and the values that were read), it finds the
// real path of each thing the server wants to read, by the name of the leaf and of the objects above it. It works on
// TR-098 (InternetGatewayDevice.) and TR-181 (Device.) trees and across vendors, because it never assumes a vendor
// object name: the PON levels are the leaves RxPower/TxPower/Temperature/Voltage/Bias of whatever object carries a
// PON or optical name, the WAN counters are the standard byte counters, and so on.
//
// A category with no matching parameter gets no path. It is listed in Gaps (a path is never made up), and the caller
// falls back to nothing, not to a guess, for it.
//
// Categories (keys of TreeResolved.Paths):
//
//	optical.object                     the object that holds the PON levels
//	optical.rx|tx|temp|volt|bias       the leaf of each level
//	wan.devices                        the WAN objects to read (WANDevice.N. in TR-098, IP.Interface.N. in TR-181)
//	wan.rx_bytes|tx_bytes              the byte counters (the WAN's own total, else the per-connection Stats)
//	wan.status                         the connection status leaves (ConnectionStatus, IP.Interface.N.Status)
//	wan.link                           the physical link status
//	hosts.table                        the host table (...Hosts.Host.)
//	wifi.ssid                          the SSID leaves, one per network
//	wifi.passphrase                    the passphrase leaf of each network (a KeyPassphrase)
//	wifi.preshared_key                 the PreSharedKey leaf of a network, which on most boxes is the raw key
//	diag                               the TR-143 diagnostics objects the box has (names)

// secretLeaf says whether the value of a parameter is a secret: the name of the leaf is one (passphrase, password,
// key, ...), or the parameter sits in a PreSharedKey or WEP key object.
func secretLeaf(path string) bool {
	i := strings.LastIndex(path, ".")
	leaf := path[i+1:]
	if Sensitive(leaf) {
		return true
	}
	for _, seg := range strings.Split(path[:i+1], ".") {
		l := strings.ToLower(seg)
		if l == "presharedkey" || l == "wepkey" || l == "wpakey" {
			return true
		}
	}
	return false
}

// leafOf returns the last segment of a parameter path.
func leafOf(path string) string { return path[strings.LastIndex(path, ".")+1:] }

// parentOf returns the object a parameter belongs to, with its trailing dot.
func parentOf(path string) string { return path[:strings.LastIndex(path, ".")+1] }

// instanceOf returns the part of path up to and including the numbered instance that follows marker
// ("X.WLANConfiguration." in "X.WLANConfiguration.5.SSID" gives "X.WLANConfiguration.5."), "" when there is none.
func instanceOf(path, marker string) string {
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	rest := path[i+len(marker):]
	n, _, ok := strings.Cut(rest, ".")
	if !ok || n == "" {
		return ""
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return path[:i+len(marker)] + n + "."
}

type treeIndex struct {
	root   string
	params []store.TreeParam
	val    map[string]string
}

func newTreeIndex(root string, ps []store.TreeParam) *treeIndex {
	x := &treeIndex{root: root, params: append([]store.TreeParam(nil), ps...), val: map[string]string{}}
	sort.SliceStable(x.params, func(i, j int) bool { return store.TreePathLess(x.params[i].Path, x.params[j].Path) })
	for _, p := range x.params {
		if p.Value != "" && !p.Secret {
			x.val[p.Path] = p.Value
		}
	}
	return x
}

// Resolve finds the real parameter paths in a tree. It is pure.
func Resolve(root string, ps []store.TreeParam) store.TreeResolved {
	x := newTreeIndex(root, ps)
	r := store.TreeResolved{Paths: map[string][]string{}}
	gap := func(g string) {
		if len(r.Gaps) < store.MaxTreeGaps {
			r.Gaps = append(r.Gaps, g)
		}
	}
	note := func(g string) {
		if len(r.Notes) < store.MaxTreeGaps {
			r.Notes = append(r.Notes, g)
		}
	}
	x.optical(&r, gap)
	if root == store.RootTR181 {
		x.wan181(&r, gap)
	} else {
		x.wan098(&r, gap)
	}
	x.hosts(&r, gap)
	x.wifi(&r, gap, note)
	x.diag(&r)
	if len(r.Paths) == 0 {
		r.Paths = nil
	}
	return r
}

// ---- optical ----

// opticalObject says whether an object path (relative to the root) looks like the PON or optical one.
func opticalObject(rel string) bool {
	l := strings.ToLower(rel)
	if strings.Contains(l, "component") {
		return false
	}
	if opticalName(rel) {
		return true
	}
	if strings.Contains(l, "wanconnection") || strings.Contains(l, "commoninterface") || strings.Contains(l, "ethernet") || strings.Contains(l, "dsl") {
		return false
	}
	return strings.Contains(l, "optic") || strings.Contains(l, "transceiver") || strings.Contains(l, "sfp")
}

func (x *treeIndex) optical(r *store.TreeResolved, gap func(string)) {
	type cand struct{ field, path string }
	byObj := map[string][]cand{}
	var order []string
	for _, p := range x.params {
		leaf := leafOf(p.Path)
		f := opticalField(leaf)
		if f == "" || p.Secret {
			continue
		}
		obj := parentOf(p.Path)
		rel := strings.TrimPrefix(obj, x.root)
		if !opticalObject(rel) {
			continue
		}
		if _, seen := byObj[obj]; !seen {
			order = append(order, obj)
		}
		byObj[obj] = append(byObj[obj], cand{f, p.Path})
	}
	if len(order) == 0 {
		gap("optical: no PON or optical object found in the parameter tree")
		return
	}
	// The object with the most distinct levels wins (the first one on a tie).
	best, bestN := "", -1
	for _, obj := range order {
		set := map[string]bool{}
		for _, c := range byObj[obj] {
			set[c.field] = true
		}
		if len(set) > bestN {
			best, bestN = obj, len(set)
		}
	}
	r.Paths["optical.object"] = []string{best}
	got := map[string]bool{}
	for _, c := range byObj[best] {
		if !got[c.field] {
			got[c.field] = true
			r.Paths["optical."+c.field] = []string{c.path}
		}
	}
	for _, f := range []string{"rx", "tx", "temp", "volt", "bias"} {
		if !got[f] {
			gap("optical: no " + f + " leaf in " + strings.TrimPrefix(best, x.root))
		}
	}
}

// ---- WAN ----

func (x *treeIndex) wan098(r *store.TreeResolved, gap func(string)) {
	var rxC, txC, rxS, txS, status, link []string
	devs := map[string]bool{}
	var devOrder []string
	addDev := func(p string) {
		if d := instanceOf(p, "WANDevice."); d != "" && !devs[d] {
			devs[d] = true
			devOrder = append(devOrder, d)
		}
	}
	for _, p := range x.params {
		path, leaf := p.Path, leafOf(p.Path)
		switch {
		case strings.Contains(path, ".WANCommonInterfaceConfig."):
			switch leaf {
			case "TotalBytesReceived":
				rxC = append(rxC, path)
				addDev(path)
			case "TotalBytesSent":
				txC = append(txC, path)
				addDev(path)
			case "PhysicalLinkStatus":
				link = append(link, path)
			}
		case strings.Contains(path, ".WANConnectionDevice.") && strings.Contains(path, ".Stats."):
			switch strings.TrimPrefix(leaf, "Ethernet") {
			case "BytesReceived":
				rxS = append(rxS, path)
				addDev(path)
			case "BytesSent":
				txS = append(txS, path)
				addDev(path)
			}
		case strings.Contains(path, ".WANConnectionDevice.") && leaf == "ConnectionStatus":
			status = append(status, path)
		}
	}
	rx, tx := rxC, txC
	if len(rx) == 0 {
		rx = rxS
	}
	if len(tx) == 0 {
		tx = txS
	}
	if len(rx) == 0 || len(tx) == 0 {
		gap("wan: no byte counters (TotalBytes* or connection Stats) found in the parameter tree, so no bit rate")
	}
	set := func(k string, v []string) {
		if len(v) > 0 {
			if len(v) > 8 {
				v = v[:8]
			}
			r.Paths[k] = v
		}
	}
	set("wan.rx_bytes", rx)
	set("wan.tx_bytes", tx)
	set("wan.status", status)
	set("wan.link", link)
	if len(status) == 0 {
		gap("wan: no connection status leaf (ConnectionStatus) found in the parameter tree")
	}
	if len(devOrder) > 4 {
		devOrder = devOrder[:4]
	}
	set("wan.devices", devOrder)
}

// wan181 picks the IP interface that is the WAN: by the name or alias the box reported (wan, ppp, internet); when no
// interface has a name read, the only interface that has counters.
func (x *treeIndex) wan181(r *store.TreeResolved, gap func(string)) {
	ifs := map[string]bool{}
	var order []string
	for _, p := range x.params {
		if !strings.Contains(p.Path, ".Stats.") || strings.TrimPrefix(leafOf(p.Path), "Ethernet") != "BytesReceived" {
			continue
		}
		if i := instanceOf(p.Path, "IP.Interface."); i != "" && !ifs[i] {
			ifs[i] = true
			order = append(order, i)
		}
	}
	var pick string
	named := false
	for _, i := range order {
		nm := strings.ToLower(x.val[i+"Name"] + " " + x.val[i+"Alias"])
		if strings.TrimSpace(nm) != "" {
			named = true
		}
		if strings.Contains(nm, "wan") || strings.Contains(nm, "ppp") || strings.Contains(nm, "internet") {
			pick = i
			break
		}
	}
	if pick == "" && !named && len(order) == 1 {
		pick = order[0]
	}
	if pick == "" {
		gap("wan: no IP interface identified as the WAN (named like WAN/PPP/Internet) with byte counters in the parameter tree")
		return
	}
	r.Paths["wan.devices"] = []string{pick}
	has2 := func(leaf string) string {
		for _, p := range x.params {
			if p.Path == pick+leaf {
				return p.Path
			}
		}
		return ""
	}
	for _, c := range []struct{ key, a, b string }{{"wan.rx_bytes", "Stats.BytesReceived", "Stats.EthernetBytesReceived"}, {"wan.tx_bytes", "Stats.BytesSent", "Stats.EthernetBytesSent"}} {
		if p := has2(c.a); p != "" {
			r.Paths[c.key] = []string{p}
		} else if p := has2(c.b); p != "" {
			r.Paths[c.key] = []string{p}
		}
	}
	if p := has2("Status"); p != "" {
		r.Paths["wan.status"] = []string{p}
	} else {
		gap("wan: no Status leaf on the WAN interface in the parameter tree")
	}
}

// ---- hosts ----

func (x *treeIndex) hosts(r *store.TreeResolved, gap func(string)) {
	for _, p := range x.params {
		i := strings.Index(p.Path, ".Hosts.Host.")
		if i < 0 {
			continue
		}
		switch leafOf(p.Path) {
		case "PhysAddress", "MACAddress", "IPAddress", "HostName":
			if instanceOf(p.Path, ".Hosts.Host.") != "" {
				r.Paths["hosts.table"] = []string{p.Path[:i] + ".Hosts.Host."}
				return
			}
		}
	}
	gap("hosts: no Hosts.Host table found in the parameter tree")
}

// ---- Wi-Fi ----

// wifiMarkers are the objects that hold one Wi-Fi network each.
var wifiMarkers = []string{".WLANConfiguration.", ".WiFi.AccessPoint.", ".WiFi.SSID."}

func (x *treeIndex) wifi(r *store.TreeResolved, gap, note func(string)) {
	var ssids, pass, psk []string
	type inst struct {
		best     string
		bestRank int
		psk      string
	}
	insts := map[string]*inst{}
	var order []string
	for _, p := range x.params {
		path, leaf := p.Path, leafOf(p.Path)
		if leaf == "SSID" && (strings.Contains(path, ".WLANConfiguration.") || strings.Contains(path, ".WiFi.SSID.")) {
			if instanceOf(path, ".WLANConfiguration.") != "" || instanceOf(path, ".WiFi.SSID.") != "" {
				ssids = append(ssids, path)
			}
		}
		var in string
		for _, m := range []string{".WLANConfiguration.", ".WiFi.AccessPoint."} {
			if in = instanceOf(path, m); in != "" {
				break
			}
		}
		if in == "" {
			continue
		}
		rel := strings.ToLower(strings.TrimPrefix(path, in))
		last := rel[strings.LastIndex(rel, ".")+1:]
		rank := -1
		switch {
		case strings.HasSuffix(last, "keypassphrase"):
			switch {
			case strings.HasPrefix(rel, "presharedkey.") && last == "keypassphrase":
				rank = 0 // the standard TR-098 place
			case last == "keypassphrase":
				rank = 1 // WLANConfiguration.N.KeyPassphrase, TR-181 Security.KeyPassphrase
			default:
				rank = 2 // a vendor spelling (X_HW_KeyPassphrase)
			}
		case strings.HasSuffix(last, "presharedkey") && !strings.HasSuffix(last, "keypassphrase"):
			s := insts[in]
			if s == nil {
				s = &inst{bestRank: 99}
				insts[in] = s
				order = append(order, in)
			}
			if s.psk == "" {
				s.psk = path
			}
			continue
		}
		if rank < 0 {
			continue
		}
		s := insts[in]
		if s == nil {
			s = &inst{bestRank: 99}
			insts[in] = s
			order = append(order, in)
		}
		if rank < s.bestRank {
			s.best, s.bestRank = path, rank
		}
	}
	if len(ssids) == 0 {
		gap("wifi: no SSID leaf found in the parameter tree")
	} else {
		if len(ssids) > 16 {
			ssids = ssids[:16]
		}
		r.Paths["wifi.ssid"] = ssids
	}
	for _, in := range order {
		s := insts[in]
		rel := strings.TrimPrefix(in, x.root)
		switch {
		case s.best != "":
			pass = append(pass, s.best)
			if s.psk != "" {
				note("wifi: " + rel + " has both a KeyPassphrase and a PreSharedKey leaf; the passphrase is read from " + strings.TrimPrefix(s.best, in) + " (PreSharedKey holds the raw key)")
			}
		case s.psk != "":
			psk = append(psk, s.psk)
			gap("wifi: " + rel + " has a PreSharedKey leaf but no KeyPassphrase leaf; PreSharedKey normally holds the raw 64-digit key, not a passphrase a person can type")
		}
	}
	if len(pass) > 16 {
		pass = pass[:16]
	}
	if len(pass) > 0 {
		r.Paths["wifi.passphrase"] = pass
	} else if len(ssids) > 0 && len(psk) == 0 {
		gap("wifi: no passphrase leaf (KeyPassphrase or PreSharedKey) found in the parameter tree")
	}
	if len(psk) > 0 {
		r.Paths["wifi.preshared_key"] = psk
	}
}

// ---- diagnostics ----

func (x *treeIndex) diag(r *store.TreeResolved) {
	seen := map[string]bool{}
	var out []string
	for _, p := range x.params {
		rest := strings.TrimPrefix(p.Path, x.root)
		rest = strings.TrimPrefix(rest, "IP.Diagnostics.")
		name, _, ok := strings.Cut(rest, ".")
		if !ok {
			continue
		}
		switch name {
		case "DownloadDiagnostics", "UploadDiagnostics", "IPPingDiagnostics", "TraceRouteDiagnostics", "NSLookupDiagnostics":
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	if len(out) > 0 {
		r.Paths["diag"] = out
	}
}
