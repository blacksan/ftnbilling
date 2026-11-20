package billing

import (
	"strings"

	"ispbill/internal/store"
)

// ouiVendors maps the first three bytes of a MAC address to the maker of the
// device. It is the curated table: the makers of ONTs, home routers, USB
// network adapters and the phones and laptops people plug into a building's
// network, under the short names the operator is used to. An address that is
// not listed has no vendor (blank), never a guess.
//
// Behind it sits ieeeOUI (oui_ieee.go): the whole IEEE MA-L registry, generated
// offline by tools/mkoui from the file the IEEE publishes. Nothing is looked up
// over the network at run time. The curated names win.
var ouiVendors = map[string]string{}

// curatedOUI is the data of ouiVendors, one row per maker. A prefix may appear
// once only (TestOUITableHasNoDuplicatePrefixes).
var curatedOUI = []struct{ Vendor, Prefixes string }{
	{"MikroTik", "00:0C:42 4C:5E:0C 64:D1:54 B8:69:F4 CC:2D:E0 D4:CA:6D E4:8D:8C 2C:C8:1B 48:8F:5A DC:2C:6E 6C:3B:6B 74:4D:28 18:FD:74 C4:AD:34 78:9A:18 48:A9:8A"}, // Routerboard.com
	{"Huawei", "00:E0:FC 00:18:82 00:25:9E 00:9A:CD 28:6E:D4 48:46:FB 54:89:98 70:72:3C 78:F5:57 AC:E2:15 E0:24:7F F4:C7:14 20:F3:A3 4C:54:99 00:46:4B 00:66:4B 04:02:1F 0C:37:DC 10:1B:54 1C:8E:5C 20:08:ED 24:09:95 2C:AB:00 34:6B:D3 48:AD:08 4C:1F:CC 5C:4C:A9 68:A0:F6 7C:60:97 80:FB:06 88:53:D4 9C:28:EF A4:99:47 AC:85:3D B4:15:13 C8:D1:5E CC:96:A0 D0:7A:B5 E0:19:1D E8:CD:2D F8:01:13"},
	{"ZTE", "00:15:EB 00:19:C6 00:1E:73 00:22:93 00:25:12 00:26:ED 34:4B:50 4C:09:B4 98:6C:F5 8C:E0:81"},
	{"TP-Link", "50:C7:BF 14:CC:20 EC:08:6B 98:DA:C4 60:E3:27 A4:2B:B0 C0:25:E9 F4:F2:6D 54:AF:97 30:B5:C2 10:FE:ED 18:D6:C7 AC:84:C6 D8:07:B6 00:27:19 00:0A:EB 14:CF:92 50:BD:5F 64:70:02 90:F6:52 94:0C:6D B0:4E:26 F4:EC:38 E8:DE:27 74:DA:88 1C:3B:F3"},
	{"Realtek", "00:E0:4C"},
	{"Ugreen", "6C:1F:F7"},
	{"Tenda", "C8:3A:35 04:95:E6 50:2B:73 CC:2D:21"},
	{"Xiaomi", "64:09:80 28:6C:07 34:CE:00 78:11:DC 9C:99:A0 F8:A4:5F 58:44:98 74:23:44"},
	{"Samsung", "00:00:F0 00:12:47 00:15:99 8C:77:12 F4:7B:5E 5C:0A:5B"},
	{"Apple", "00:03:93 00:0A:95 00:11:24 00:17:F2 3C:07:54 40:6C:8F 70:56:81 A4:5E:60 B8:E8:56 F0:18:98 D0:03:4B 28:CF:E9 AC:BC:32 00:1C:B3 00:1E:C2 00:25:00 00:26:BB 3C:15:C2 F0:DB:F8 00:1F:F3 00:23:DF 00:24:36 00:25:4B 00:26:08 00:26:4A"},
	{"Intel", "00:1B:21 3C:97:0E 00:13:E8 00:15:00 3C:A9:F4 A4:4E:31 7C:B2:7D F8:94:C2 34:13:E8"},
	{"Dell", "00:14:22 18:03:73 B8:CA:3A F8:BC:12 14:FE:B5"},
	{"Raspberry Pi", "B8:27:EB DC:A6:32 E4:5F:01 D8:3A:DD"},
	{"Ubiquiti", "00:15:6D 00:27:22 04:18:D6 24:A4:3C 44:D9:E7 68:72:51 74:83:C2 78:8A:20 80:2A:A8 B4:FB:E4 DC:9F:DB F0:9F:C2 FC:EC:DA"},
	{"Netgear", "00:09:5B 00:14:6C 00:1B:2F 00:24:B2 20:4E:7F 28:C6:8E A0:21:B7"},
	{"D-Link", "00:05:5D 00:0D:88 00:11:95 00:13:46 00:15:E9 1C:7E:E5 28:10:7B 90:94:E4"},
	{"Google", "3C:5A:B4 F4:F5:D8 54:60:09 A4:77:33"},
	{"Amazon", "44:65:0D F0:D2:F1 74:C2:46 68:54:FD 84:D6:D0"},
	{"Hikvision", "44:19:B6 28:57:BE C0:56:E3"},
}

func init() {
	for _, row := range curatedOUI {
		for _, p := range strings.Fields(row.Prefixes) {
			ouiVendors[p] = row.Vendor
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
	if v := ouiVendors[m[:8]]; v != "" {
		return v
	}
	return ieeeOUI[m[:8]]
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
