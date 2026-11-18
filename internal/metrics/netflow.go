package metrics

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// ---- NetFlow v5 ----------------------------------------------------------

const (
	v5HeaderLen = 24
	v5RecordLen = 48
	v5MaxCount  = 30
)

// V5Header is the NetFlow v5 packet header.
type V5Header struct {
	Count        int
	UptimeMs     uint32
	UnixSecs     uint32
	Sequence     uint32
	SamplingMode int // 0 none, 1 deterministic, 2 random
	Sampling     int // 1-in-N; values < 1 mean 1
}

// V5Record is one flow.
type V5Record struct {
	Src, Dst         netip.Addr
	Packets, Octets  uint32
	SrcPort, DstPort uint16
	TCPFlags, Proto  uint8
}

// ParseV5 decodes a NetFlow v5 export packet. It validates the version, the
// record count and the exact length.
func ParseV5(b []byte) (V5Header, []V5Record, error) {
	var h V5Header
	if len(b) < v5HeaderLen {
		return h, nil, errors.New("netflow: short packet")
	}
	if v := binary.BigEndian.Uint16(b[0:2]); v != 5 {
		return h, nil, fmt.Errorf("netflow: version %d is not 5", v)
	}
	h.Count = int(binary.BigEndian.Uint16(b[2:4]))
	if h.Count < 1 || h.Count > v5MaxCount {
		return h, nil, fmt.Errorf("netflow: bad record count %d", h.Count)
	}
	if len(b) != v5HeaderLen+h.Count*v5RecordLen {
		return h, nil, fmt.Errorf("netflow: length %d does not match %d records", len(b), h.Count)
	}
	h.UptimeMs = binary.BigEndian.Uint32(b[4:8])
	h.UnixSecs = binary.BigEndian.Uint32(b[8:12])
	h.Sequence = binary.BigEndian.Uint32(b[16:20])
	si := binary.BigEndian.Uint16(b[22:24])
	h.SamplingMode, h.Sampling = int(si>>14), int(si&0x3fff)
	if h.Sampling < 1 {
		h.Sampling = 1
	}
	recs := make([]V5Record, h.Count)
	for i := range recs {
		r := b[v5HeaderLen+i*v5RecordLen:]
		recs[i] = V5Record{
			Src:      netip.AddrFrom4([4]byte(r[0:4])),
			Dst:      netip.AddrFrom4([4]byte(r[4:8])),
			Packets:  binary.BigEndian.Uint32(r[16:20]),
			Octets:   binary.BigEndian.Uint32(r[20:24]),
			SrcPort:  binary.BigEndian.Uint16(r[32:34]),
			DstPort:  binary.BigEndian.Uint16(r[34:36]),
			TCPFlags: r[37],
			Proto:    r[38],
		}
	}
	return h, recs, nil
}

// ---- service classification ----------------------------------------------

// ServiceRule maps hostname fragments to a service name. Rules are tried in
// order and the first match wins, so put specific services before general
// ones (YouTube before Google, WhatsApp before Meta). EDIT THIS TABLE to
// add or change services; nothing else needs to change.
type ServiceRule struct {
	Service string
	Hosts   []string // case-insensitive substrings of the remote hostname
}

var ServiceRules = []ServiceRule{
	{"YouTube", []string{"googlevideo", "youtube", "ytimg", "youtu.be"}},
	{"Netflix", []string{"netflix", "nflxvideo", "nflximg", "nflxso"}},
	{"WhatsApp", []string{"whatsapp"}},
	{"Meta", []string{"facebook", "fbcdn", "instagram", "cdninstagram", "fbsbx"}},
	{"TikTok", []string{"tiktok", "byteoversea", "tiktokcdn", "ibytedtos", "musical.ly"}},
	{"Spotify", []string{"spotify", "scdn.co"}},
	{"Apple", []string{"apple", "icloud", "mzstatic", "aaplimg"}},
	{"Microsoft", []string{"microsoft", "windowsupdate", "msftconnect", "office", "live.com", "bing.com", "skype", "azure", "xboxlive"}},
	{"Zoom", []string{"zoom.us", "zoom.com", "zoomgov"}},
	{"Showmax", []string{"showmax"}},
	{"DStv", []string{"dstv", "multichoice"}},
	{"Google", []string{"google", "gstatic", "ggpht", "gvt1", "gvt2", "googleapis", "googleusercontent", "android.com"}},
	{"CDN (other)", []string{"akamai", "cloudflare", "fastly", "edgekey", "edgesuite", "cloudfront", "cdn"}},
}

// PortRule is the fallback when the hostname is unknown: it looks at the
// remote port (and protocol, 0 = any). First match wins.
type PortRule struct {
	Service        string
	Proto          uint8 // 6 tcp, 17 udp, 0 any
	PortLo, PortHi uint16
}

var PortRules = []PortRule{
	{"DNS", 0, 53, 53},
	{"NTP", 17, 123, 123},
	{"Calls / STUN", 17, 3478, 3481},
	{"Calls / STUN", 17, 19302, 19302},
	{"Messaging / push", 6, 5222, 5223},
	{"HTTPS (other)", 6, 443, 443},
	{"HTTPS (other)", 17, 443, 443}, // QUIC
	{"HTTP", 6, 80, 80},
	{"HTTP", 6, 8080, 8080},
}

// ServiceOther is used when nothing matches.
const ServiceOther = "Other"

// Classify decides the service of a flow from the hostnames known for the
// remote IP, falling back to the remote port.
func Classify(hosts []string, proto uint8, remotePort uint16) string {
	if len(hosts) > 0 {
		joined := strings.ToLower(strings.Join(hosts, " "))
		for _, r := range ServiceRules {
			for _, h := range r.Hosts {
				if strings.Contains(joined, h) {
					return r.Service
				}
			}
		}
	}
	for _, r := range PortRules {
		if (r.Proto == 0 || r.Proto == proto) && remotePort >= r.PortLo && remotePort <= r.PortHi {
			return r.Service
		}
	}
	return ServiceOther
}

// ---- receiver ---------------------------------------------------------------

// The UDP socket is owned by internal/sites, which demultiplexes datagrams by
// source tunnel address and hands each one to the right collector.

// HandleNetflow processes one datagram.
func (c *Collector) HandleNetflow(now time.Time, pkt []byte, from string) {
	h, recs, err := ParseV5(pkt)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.D.NF.Bad++
		return
	}
	c.D.NF.Packets++
	c.D.NF.LastAt = now
	c.D.NF.Exporter = from
	for _, r := range recs {
		c.addFlowLocked(now, r, uint64(h.Sampling))
	}
}

// addFlowLocked attributes one flow. The client is the address inside the
// customer subnet; the other end is the remote side.
func (c *Collector) addFlowLocked(now time.Time, r V5Record, mult uint64) {
	if mult < 1 {
		mult = 1
	}
	bytes := uint64(r.Octets) * mult
	var client, remote netip.Addr
	var remotePort uint16
	switch {
	case c.inSubnet(r.Src) && !c.inSubnet(r.Dst):
		client, remote, remotePort = r.Src, r.Dst, r.DstPort // upload
	case c.inSubnet(r.Dst) && !c.inSubnet(r.Src):
		client, remote, remotePort = r.Dst, r.Src, r.SrcPort // download
	default:
		return
	}
	svc := Classify(c.hostsFor(remote), r.Proto, remotePort)
	c.D.NF.Flows++
	c.D.NF.Bytes += bytes
	c.D.Svc.add(now, svc, bytes)
	if code := c.codeForIP(client.String(), now); code != "" {
		c.D.NF.Attrib += bytes
		days := c.D.SvcAcct[code]
		if days == nil {
			days = map[int64]map[string]uint64{}
			c.D.SvcAcct[code] = days
		}
		k := BucketStart(TD, now)
		if days[k] == nil {
			days[k] = map[string]uint64{}
		}
		days[k][svc] += bytes
	}
}
