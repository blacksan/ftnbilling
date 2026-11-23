package wg

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ispbill/internal/store"
)

// Peer is one router: its public key and its tunnel address.
type Peer struct {
	PublicKey string
	TunnelIP  string
	// MgmtSubnet, when set, is the router's ONU management subnet (10.6.<n>.0/24). It is added to the peer's
	// AllowedIPs on the server so the ACS can reach the ONUs behind the router (connection requests) and
	// accepts their packets coming back through the tunnel.
	MgmtSubnet string
}

// ONUSupernet is the range every ONU management subnet comes from; the root sync script accepts a second
// AllowedIPs entry only inside it, and routes it into wg0.
const ONUSupernet = "10.6.0.0/16"

// RenderPeers returns the peers file: one [Peer] block per router, sorted by
// tunnel address, each allowed its one /32 and, when it has one, its ONU management /24. Peers with an invalid key or
// address are skipped. The format is deliberately tiny because the root sync
// script accepts nothing else.
func RenderPeers(peers []Peer) []byte {
	type row struct {
		addr netip.Addr
		key  string
		mg   string
	}
	super := netip.MustParsePrefix(ONUSupernet)
	seen := map[netip.Prefix]bool{}
	var rows []row
	for _, p := range peers {
		a, err := netip.ParseAddr(p.TunnelIP)
		if err != nil || !a.Is4() || !ValidKey(p.PublicKey) {
			continue
		}
		r := row{addr: a, key: p.PublicKey}
		if p.MgmtSubnet != "" {
			// Only a /24 inside the ONU range, and each subnet to one peer only (the first by tunnel address wins).
			if mp, err := netip.ParsePrefix(p.MgmtSubnet); err == nil && mp.Bits() == 24 && mp.Masked() == mp && super.Contains(mp.Addr()) {
				r.mg = mp.String()
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if c := rows[i].addr.Compare(rows[j].addr); c != 0 {
			return c < 0
		}
		return rows[i].key < rows[j].key
	})
	var b strings.Builder
	for _, r := range rows {
		allowed := r.addr.String() + "/32"
		if r.mg != "" {
			if mp := netip.MustParsePrefix(r.mg); !seen[mp] {
				seen[mp] = true
				allowed += ", " + r.mg
			}
		}
		fmt.Fprintf(&b, "[Peer]\nPublicKey = %s\nAllowedIPs = %s\n\n", r.key, allowed)
	}
	return []byte(b.String())
}

// WritePeers writes dir/peers.conf atomically and then rewrites dir/apply,
// which a root-owned systemd .path unit watches. The dir is created if needed.
func WritePeers(dir string, peers []Peer) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := store.WriteFileAtomic(filepath.Join(dir, "peers.conf"), RenderPeers(peers)); err != nil {
		return err
	}
	stamp := fmt.Sprintf("%d\n", time.Now().UnixNano())
	return os.WriteFile(filepath.Join(dir, "apply"), []byte(stamp), 0o640)
}
