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
}

// RenderPeers returns the peers file: one [Peer] block per router, sorted by
// tunnel address, each allowed exactly one /32. Peers with an invalid key or
// address are skipped. The format is deliberately tiny because the root sync
// script accepts nothing else.
func RenderPeers(peers []Peer) []byte {
	type row struct {
		addr netip.Addr
		key  string
	}
	var rows []row
	for _, p := range peers {
		a, err := netip.ParseAddr(p.TunnelIP)
		if err != nil || !a.Is4() || !ValidKey(p.PublicKey) {
			continue
		}
		rows = append(rows, row{a, p.PublicKey})
	}
	sort.Slice(rows, func(i, j int) bool {
		if c := rows[i].addr.Compare(rows[j].addr); c != 0 {
			return c < 0
		}
		return rows[i].key < rows[j].key
	})
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\n\n", r.key, r.addr)
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
