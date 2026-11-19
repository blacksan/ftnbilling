package billing

import (
	"strings"

	"ispbill/internal/store"
)

// ouiVendors maps the first three bytes of a MAC address to the maker of the
// device. It is deliberately small: the makers of ONTs, home routers and
// phones people actually plug into a building's network, so the operator can
// tell a Huawei ONT from a phone at a glance. An address that is not listed
// has no vendor (blank), never a guess.
var ouiVendors = map[string]string{}

func init() {
	for vendor, prefixes := range map[string]string{
		"MikroTik":     "00:0C:42 4C:5E:0C 64:D1:54 B8:69:F4 CC:2D:E0 D4:CA:6D E4:8D:8C 2C:C8:1B 48:8F:5A DC:2C:6E",
		"Huawei":       "00:E0:FC 00:18:82 00:25:9E 00:9A:CD 28:6E:D4 48:46:FB 54:89:98 70:72:3C 78:F5:57 AC:E2:15 E0:24:7F F4:C7:14 20:F3:A3 4C:54:99",
		"ZTE":          "00:15:EB 00:19:C6 00:1E:73 00:22:93 00:25:12 00:26:ED 34:4B:50 4C:09:B4 98:6C:F5 8C:E0:81",
		"TP-Link":      "50:C7:BF 14:CC:20 EC:08:6B 98:DA:C4 60:E3:27 A4:2B:B0 C0:25:E9 F4:F2:6D 54:AF:97 30:B5:C2 10:FE:ED 18:D6:C7 AC:84:C6 D8:07:B6",
		"Tenda":        "C8:3A:35 04:95:E6 50:2B:73 CC:2D:21",
		"Xiaomi":       "64:09:80 28:6C:07 34:CE:00 78:11:DC 9C:99:A0 F8:A4:5F 58:44:98 74:23:44",
		"Samsung":      "00:00:F0 00:12:47 00:15:99 8C:77:12 F4:7B:5E 5C:0A:5B",
		"Apple":        "00:03:93 00:0A:95 00:11:24 00:17:F2 3C:07:54 40:6C:8F 70:56:81 A4:5E:60 B8:E8:56 F0:18:98 D0:03:4B 28:CF:E9 AC:BC:32",
		"Intel":        "00:1B:21 3C:97:0E",
		"Raspberry Pi": "B8:27:EB DC:A6:32 E4:5F:01 D8:3A:DD",
		"Ubiquiti":     "00:15:6D 00:27:22 04:18:D6 24:A4:3C 44:D9:E7 68:72:51 74:83:C2 78:8A:20 80:2A:A8 B4:FB:E4 DC:9F:DB F0:9F:C2 FC:EC:DA",
		"Netgear":      "00:09:5B 00:14:6C 00:1B:2F 00:24:B2 20:4E:7F 28:C6:8E A0:21:B7",
		"D-Link":       "00:05:5D 00:0D:88 00:11:95 00:13:46 00:15:E9 1C:7E:E5 28:10:7B 90:94:E4",
		"Google":       "3C:5A:B4 F4:F5:D8 54:60:09 A4:77:33",
		"Amazon":       "44:65:0D F0:D2:F1 74:C2:46 68:54:FD 84:D6:D0",
		"Hikvision":    "44:19:B6 28:57:BE C0:56:E3",
	} {
		for _, p := range strings.Fields(prefixes) {
			ouiVendors[p] = vendor
		}
	}
}

// PrivateAddress is shown for a randomised (locally administered) MAC, which
// phones use per network: it is not an ONT's WAN address.
const PrivateAddress = "Private address (random)"

// Vendor names the maker of a MAC address from its prefix: "" when unknown.
// A locally administered address (second-lowest bit of the first byte set)
// is a randomised one and is reported as PrivateAddress.
func Vendor(mac string) string {
	m, ok := store.CanonMAC(mac)
	if !ok {
		return ""
	}
	if first := hexNibble(m[1]); first&2 != 0 {
		return PrivateAddress
	}
	return ouiVendors[m[:8]]
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 0
}
