package acs

import (
	"strconv"
	"strings"

	"ispbill/internal/store"
)

// Data-model profiles. A profile says which parameter names carry the Wi-Fi name, the passphrase and the
// WPA2-PSK security mode of one instance, for one data model root and, where needed, one vendor. The table is
// data: a quirk is a new row, tried by OUI or manufacturer before the generic row of the same root. The
// passphrase has several candidates; the first that the device accepts is remembered (ONU.Profile.PassIdx).

// Setting is one parameter of a profile, relative to the instance path.
type Setting struct {
	Leaf, Value, Type string
}

// Profile is one row of the table.
type Profile struct {
	Name string
	Root string
	// Match says whether the row applies to this vendor; nil matches every device of the root.
	Match func(manufacturer, oui, class string) bool
	// SSID and Enable are the leaves of the Wi-Fi name and on/off switch of an instance.
	SSID, Enable string
	// Pass are the passphrase candidates, in the order they are tried (the owner-confirmed one for the model first).
	Pass []string
	// Security is set together with the passphrase, best effort: a name the device refuses (9005) is dropped.
	Security []Setting
	// Band leaves: tried in order to find 2.4 / 5 GHz of an instance.
	BandLeaves []string
	// Channel, Standard leaves read at discovery.
	Channel, Standard string
}

func vendorIs(names []string, ouis []string) func(m, oui, c string) bool {
	return func(m, oui, c string) bool {
		lm := strings.ToLower(m)
		for _, n := range names {
			if strings.Contains(lm, n) {
				return true
			}
		}
		o := strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(oui))
		for _, v := range ouis {
			if o == v {
				return true
			}
		}
		return false
	}
}

var (
	sec098 = []Setting{
		{Leaf: "BeaconType", Value: "11i"},
		{Leaf: "IEEE11iAuthenticationMode", Value: "PSKAuthentication"},
		{Leaf: "IEEE11iEncryptionModes", Value: "AESEncryption"},
	}
	sec098Huawei = []Setting{
		{Leaf: "BeaconType", Value: "11i"},
		{Leaf: "IEEE11iAuthenticationMode", Value: "PSKAuthentication"},
		{Leaf: "WPAAuthenticationMode", Value: "PSKAuthentication"},
		{Leaf: "IEEE11iEncryptionModes", Value: "AESEncryption"},
	}
	sec181 = []Setting{
		{Leaf: "Security.ModeEnabled", Value: "WPA2-Personal"},
	}
)

// profiles is the table, most specific first. The generic rows come last for each root.
//
// Passphrase order (research notes, wifi-params): the standard PreSharedKey.1.KeyPassphrase first, then the plain
// KeyPassphrase, then vendor spellings. A refusal (9005, 9007) moves to the next; a box that merely ACCEPTS a name has
// not proved it works (one ONU took a vendor name and changed nothing): only the owner's "It works" promotes a name
// into the per-model table (works.go). PreSharedKey.1.PreSharedKey is deliberately not a candidate: it holds the
// 64-hex key, not a passphrase.
var profiles = []Profile{
	// V-SOL: the fleet's standard ONU. The parameter names are the generic ones; nothing here is bench-verified yet
	// (BENCH-CHECK: manufacturer string, data model root, which passphrase leaf takes, band leaves).
	{Name: "tr098-vsol", Root: store.RootTR098,
		Match: vendorIs([]string{"v-sol", "vsol", "v sol", "vsolution"}, nil),
		SSID:  "SSID", Enable: "Enable", Pass: []string{"PreSharedKey.1.KeyPassphrase", "KeyPassphrase"}, Security: sec098,
		BandLeaves: []string{"OperatingFrequencyBand", "Standard"}, Channel: "Channel", Standard: "Standard"},
	{Name: "tr181-vsol", Root: store.RootTR181,
		Match: vendorIs([]string{"v-sol", "vsol", "v sol", "vsolution"}, nil),
		SSID:  "SSID", Enable: "Enable", Pass: []string{"Security.KeyPassphrase", "Security.PreSharedKey"}, Security: sec181,
		BandLeaves: []string{"OperatingFrequencyBand"}, Channel: "Channel"},
	{Name: "tr098-zte", Root: store.RootTR098,
		Match: vendorIs([]string{"zte"}, []string{"0019C6", "001E73", "002293", "002512", "344B50", "4C09B4", "9CD24B"}),
		SSID:  "SSID", Enable: "Enable", Pass: []string{"PreSharedKey.1.KeyPassphrase", "KeyPassphrase"}, Security: sec098,
		BandLeaves: []string{"OperatingFrequencyBand", "X_ZTE-COM_WlanBand", "Standard"}, Channel: "Channel", Standard: "Standard"},
	// Huawei: the standard names first, then the X_HW_ spellings some builds expose. BENCH-CHECK on a real unit.
	{Name: "tr098-huawei", Root: store.RootTR098,
		Match: vendorIs([]string{"huawei"}, []string{"00E0FC", "001E10", "00259E", "3C4711", "ACE215", "F4C714"}),
		SSID:  "SSID", Enable: "Enable", Pass: []string{"PreSharedKey.1.KeyPassphrase", "KeyPassphrase", "X_HW_KeyPassphrase", "X_HW_PreSharedKey"}, Security: sec098Huawei,
		BandLeaves: []string{"OperatingFrequencyBand", "X_HW_Band", "Standard"}, Channel: "Channel", Standard: "Standard"},
	{Name: "tr098", Root: store.RootTR098, SSID: "SSID", Enable: "Enable",
		Pass: []string{"PreSharedKey.1.KeyPassphrase", "KeyPassphrase"}, Security: sec098,
		BandLeaves: []string{"OperatingFrequencyBand", "Standard"}, Channel: "Channel", Standard: "Standard"},
	{Name: "tr181-huawei", Root: store.RootTR181,
		Match: vendorIs([]string{"huawei"}, []string{"00E0FC", "001E10", "00259E", "3C4711", "ACE215", "F4C714"}),
		SSID:  "SSID", Enable: "Enable", Pass: []string{"Security.KeyPassphrase", "Security.X_HW_KeyPassphrase"}, Security: sec181,
		BandLeaves: []string{"OperatingFrequencyBand"}, Channel: "Channel"},
	{Name: "tr181", Root: store.RootTR181, SSID: "SSID", Enable: "Enable",
		Pass: []string{"Security.KeyPassphrase", "Security.PreSharedKey"}, Security: sec181,
		BandLeaves: []string{"OperatingFrequencyBand"}, Channel: "Channel"},
}

// pickProfile returns the row for a device: the first row of its root whose Match accepts it.
func pickProfile(root, manufacturer, oui, class string) Profile {
	for _, p := range profiles {
		if p.Root == root && (p.Match == nil || p.Match(manufacturer, oui, class)) {
			return p
		}
	}
	for _, p := range profiles {
		if p.Name == "tr098" {
			return p
		}
	}
	return profiles[0]
}

// rootOf names the data model root from the parameter names of an Inform ("InternetGatewayDevice." or "Device.").
func rootOf(params []ParamValue) string {
	for _, p := range params {
		switch {
		case strings.HasPrefix(p.Name, store.RootTR098):
			return store.RootTR098
		case strings.HasPrefix(p.Name, store.RootTR181):
			return store.RootTR181
		}
	}
	return ""
}

// bandInfo is what a Wi-Fi instance (or its radio) says about its band.
type bandInfo struct {
	Freq     string // OperatingFrequencyBand: "2.4GHz", "5GHz", "6GHz"
	Standard string // "b,g,n", "a,n,ac", "802.11ax"
	Possible string // PossibleChannels: "1-13", "36,40,44,48"
}

// bandOf decides 2.4 or 5 GHz of a Wi-Fi instance from what it reports: its frequency band first, then its
// standards, then its channel (or, when the channel is automatic, the channels it may use). It never guesses from
// the position of the instance; "" means the box did not say.
func bandOf(h bandInfo, channel int) string {
	f := strings.ToLower(strings.ReplaceAll(h.Freq, " ", ""))
	switch {
	case strings.HasPrefix(f, "2.4"):
		return "2.4"
	case strings.HasPrefix(f, "5"):
		return "5"
	case strings.HasPrefix(f, "6"):
		return "" // 6 GHz is not configured
	}
	toks := map[string]bool{}
	for _, t := range strings.FieldsFunc(strings.ToLower(h.Standard), func(r rune) bool { return r == ',' || r == ' ' || r == '/' || r == ';' }) {
		t = strings.TrimPrefix(strings.TrimPrefix(t, "802.11"), "ieee")
		toks[strings.TrimLeft(t, "-.")] = true
	}
	switch {
	case toks["ac"], toks["a"]:
		return "5"
	case toks["b"], toks["g"]:
		return "2.4"
	}
	switch {
	case channel >= 1 && channel <= 14:
		return "2.4"
	case channel >= 32:
		return "5"
	}
	lo, hi := channelRange(h.Possible)
	switch {
	case hi > 0 && hi <= 14:
		return "2.4"
	case lo >= 32:
		return "5"
	}
	return ""
}

// channelRange is the lowest and highest channel of a PossibleChannels value ("1-13", "36,40,44", "1,6,11").
func channelRange(s string) (lo, hi int) {
	for _, part := range strings.Split(s, ",") {
		for _, e := range strings.Split(strings.TrimSpace(part), "-") {
			n, err := strconv.Atoi(strings.TrimSpace(e))
			if err != nil || n <= 0 {
				continue
			}
			if lo == 0 || n < lo {
				lo = n
			}
			if n > hi {
				hi = n
			}
		}
	}
	return lo, hi
}

// primary picks the instance to configure for a band: the first enabled one, else the first. Guest networks
// (extra, usually disabled instances) are left alone.
func primary(wifi []store.ONUWiFi, band string) (store.ONUWiFi, bool) {
	var first *store.ONUWiFi
	for i := range wifi {
		w := wifi[i]
		if w.Band != band {
			continue
		}
		if w.Enabled {
			return w, true
		}
		if first == nil {
			first = &wifi[i]
		}
	}
	if first != nil {
		return *first, true
	}
	return store.ONUWiFi{}, false
}

// apPath is the object that holds the security of an instance: itself under TR-098, the AccessPoint under TR-181.
func apPath(w store.ONUWiFi) string {
	if w.AP != "" {
		return w.AP
	}
	return w.Path
}

// wifiSettings builds the SetParameterValues list for one instance: the name, the passphrase under leaf, and (unless
// withSec is false) the WPA2-PSK security settings. skip lists security names the box refused earlier in this task;
// the name and the passphrase are never skipped.
func (p Profile) wifiSettings(w store.ONUWiFi, ssid, pass, leaf string, withSec bool, skip map[string]bool) []ParamValue {
	out := []ParamValue{{Name: w.Path + p.SSID, Value: ssid}, {Name: apPath(w) + leaf, Value: pass}}
	if withSec {
		for _, s := range p.Security {
			if !skip[apPath(w)+s.Leaf] {
				out = append(out, ParamValue{Name: apPath(w) + s.Leaf, Value: s.Value, Type: s.Type})
			}
		}
	}
	return out
}

// Validation of the owner's input (also enforced by the API): a Wi-Fi name is 1 to 32 bytes of printable text;
// a WPA2 passphrase is 8 to 63 printable ASCII characters.

// ValidSSID reports why an SSID cannot be used ("" = fine).
func ValidSSID(s string) string {
	if len(s) < 1 || len(s) > 32 {
		return "the Wi-Fi name must be 1 to 32 characters (bytes)"
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "the Wi-Fi name has a control character"
		}
	}
	if strings.TrimSpace(s) != s {
		return "the Wi-Fi name cannot start or end with a space"
	}
	return ""
}

// ValidPassphrase reports why a WPA2 passphrase cannot be used ("" = fine).
func ValidPassphrase(s string) string {
	if len(s) < 8 || len(s) > 63 {
		return "the Wi-Fi password must be 8 to 63 characters"
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return "the Wi-Fi password may use printable English letters, digits and symbols only"
		}
	}
	return ""
}
