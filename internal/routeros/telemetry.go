package routeros

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Telemetry is the read-only monitoring surface used by the metrics collector.
// REST and Mock both implement it.
type Telemetry interface {
	ActiveSessions(ctx context.Context) ([]Session, error)
	SimpleQueues(ctx context.Context) ([]SimpleQueue, error)
	Interfaces(ctx context.Context) ([]Iface, error)
	DNSCache(ctx context.Context) ([]DNSEntry, error)
	PingHost(ctx context.Context, target string, count int) (PingResult, error)
	System(ctx context.Context) (SysInfo, error)
}

// Session is one /ip/hotspot/active row. BytesIn is upload (from the client),
// BytesOut is download (to the client).
type Session struct {
	ID, User, Address, MAC string
	BytesIn, BytesOut      uint64
	Uptime                 time.Duration
}

// SimpleQueue is a /queue/simple row. RateA/MaxA is the first number of the
// "a/b" pair and RateB/MaxB the second; both pairs share the same ordering, so
// ratios are meaningful whichever direction RouterOS lists first.
type SimpleQueue struct {
	Name, Target, Comment string
	RateA, RateB          int64 // bit/s
	MaxA, MaxB            int64 // bit/s, 0 = unlimited
	// BytesUp and BytesDown are the queue's byte counters (RouterOS lists
	// "upload/download" from the target's side, like max-limit). They only go
	// up until the queue is recreated or counters are reset.
	BytesUp, BytesDown uint64
}

type Iface struct {
	Name, Type                 string
	RxBytes, TxBytes           uint64
	Running, Disabled, Dynamic bool
}

type DNSEntry struct{ Name, Data, Type string }

type PingResult struct {
	Target   string
	Sent     int
	Received int
	AvgRTT   time.Duration
	LossPct  float64
}

type SysInfo struct {
	CPULoad           int
	FreeMem, TotalMem uint64
	Uptime            string
	TempC             float64
	HasTemp           bool
}

func atou(s string) uint64  { n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64); return n }
func atoi64(s string) int64 { n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64); return n }
func isTrue(s string) bool  { return s == "true" || s == "yes" }

// ParseRouterDuration parses RouterOS time strings such as "12ms345us",
// "1s200ms", "2h3m4s", "1w2d" or "00:01:02".
func ParseRouterDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if strings.Contains(s, ":") { // [d ]hh:mm:ss
		var d time.Duration
		if i := strings.Index(s, "d"); i > 0 {
			n, _ := strconv.Atoi(s[:i])
			d += time.Duration(n) * 24 * time.Hour
			s = strings.TrimSpace(s[i+1:])
		}
		parts := strings.Split(s, ":")
		mult := []time.Duration{time.Hour, time.Minute, time.Second}
		for i, p := range parts {
			if i >= 3 {
				break
			}
			f, _ := strconv.ParseFloat(p, 64)
			d += time.Duration(f * float64(mult[i]))
		}
		return d
	}
	var total time.Duration
	num := ""
	for i := 0; i < len(s); {
		c := s[i]
		if (c >= '0' && c <= '9') || c == '.' {
			num += string(c)
			i++
			continue
		}
		j := i
		for j < len(s) && !((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
			j++
		}
		unit := s[i:j]
		f, _ := strconv.ParseFloat(num, 64)
		num = ""
		var u time.Duration
		switch unit {
		case "w":
			u = 7 * 24 * time.Hour
		case "d":
			u = 24 * time.Hour
		case "h":
			u = time.Hour
		case "m":
			u = time.Minute
		case "s":
			u = time.Second
		case "ms":
			u = time.Millisecond
		case "us":
			u = time.Microsecond
		}
		total += time.Duration(f * float64(u))
		i = j
	}
	return total
}

func (c *REST) ActiveSessions(ctx context.Context) ([]Session, error) {
	var rows []map[string]string
	if err := c.do(ctx, http.MethodGet, "/ip/hotspot/active", nil, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(rows))
	for _, r := range rows {
		out = append(out, Session{
			ID: r[".id"], User: r["user"], Address: r["address"], MAC: r["mac-address"],
			BytesIn: atou(r["bytes-in"]), BytesOut: atou(r["bytes-out"]), Uptime: ParseRouterDuration(r["uptime"]),
		})
	}
	return out, nil
}

func splitPair(s string) (int64, int64) {
	a, b, _ := strings.Cut(s, "/")
	return atoi64(a), atoi64(b)
}

func (c *REST) SimpleQueues(ctx context.Context) ([]SimpleQueue, error) {
	var rows []map[string]string
	if err := c.do(ctx, http.MethodGet, "/queue/simple", nil, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]SimpleQueue, 0, len(rows))
	for _, r := range rows {
		q := SimpleQueue{Name: r["name"], Target: r["target"], Comment: r["comment"]}
		q.RateA, q.RateB = splitPair(r["rate"])
		q.MaxA, q.MaxB = splitPair(r["max-limit"])
		up, down := splitPair(r["bytes"])
		q.BytesUp, q.BytesDown = uint64(up), uint64(down)
		out = append(out, q)
	}
	return out, nil
}

func (c *REST) Interfaces(ctx context.Context) ([]Iface, error) {
	var rows []map[string]string
	if err := c.do(ctx, http.MethodGet, "/interface", nil, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]Iface, 0, len(rows))
	for _, r := range rows {
		out = append(out, Iface{
			Name: r["name"], Type: r["type"], RxBytes: atou(r["rx-byte"]), TxBytes: atou(r["tx-byte"]),
			Running: isTrue(r["running"]), Disabled: isTrue(r["disabled"]), Dynamic: isTrue(r["dynamic"]),
		})
	}
	return out, nil
}

func (c *REST) DNSCache(ctx context.Context) ([]DNSEntry, error) {
	var rows []map[string]string
	if err := c.do(ctx, http.MethodGet, "/ip/dns/cache", nil, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]DNSEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, DNSEntry{Name: r["name"], Data: r["data"], Type: r["type"]})
	}
	return out, nil
}

// PingHost runs the router's ping tool through the REST API (POST /rest/ping).
func (c *REST) PingHost(ctx context.Context, target string, count int) (PingResult, error) {
	var rows []map[string]string
	body := map[string]string{"address": target, "count": strconv.Itoa(count)}
	to := time.Duration(count+8) * time.Second
	if err := c.doT(ctx, to, http.MethodPost, "/ping", nil, body, &rows); err != nil {
		return PingResult{}, err
	}
	return parsePing(target, count, rows), nil
}

// parsePing reads the per-reply rows and, when present, the summary row.
func parsePing(target string, count int, rows []map[string]string) PingResult {
	res := PingResult{Target: target, Sent: count}
	var sum time.Duration
	n := 0
	for _, r := range rows {
		if t, ok := r["time"]; ok && r["seq"] != "" {
			sum += ParseRouterDuration(t)
			n++
		}
		if v, ok := r["sent"]; ok {
			res.Sent = int(atoi64(v))
		}
		if v, ok := r["received"]; ok {
			res.Received = int(atoi64(v))
		}
		if v, ok := r["avg-rtt"]; ok && v != "" {
			res.AvgRTT = ParseRouterDuration(v)
		}
	}
	if res.Received == 0 {
		res.Received = n
	}
	if res.AvgRTT == 0 && n > 0 {
		res.AvgRTT = sum / time.Duration(n)
	}
	if res.Sent > 0 {
		res.LossPct = 100 * float64(res.Sent-res.Received) / float64(res.Sent)
		if res.LossPct < 0 {
			res.LossPct = 0
		}
	}
	return res
}

func (c *REST) System(ctx context.Context) (SysInfo, error) {
	var r map[string]string
	if err := c.do(ctx, http.MethodGet, "/system/resource", nil, nil, &r); err != nil {
		return SysInfo{}, err
	}
	si := SysInfo{CPULoad: int(atoi64(r["cpu-load"])), FreeMem: atou(r["free-memory"]), TotalMem: atou(r["total-memory"]), Uptime: r["uptime"]}
	// Health is optional (some boards have no sensors). RouterOS 7 returns a
	// list of {name,value}; older releases one flat object. Handle both.
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/system/health", nil, nil, &raw); err == nil {
		si.TempC, si.HasTemp = parseTemp(raw)
	}
	return si, nil
}

func parseTemp(raw json.RawMessage) (float64, bool) {
	pick := func(name, val string) (float64, bool) {
		if strings.Contains(name, "temperature") && !strings.Contains(name, "cpu-") || name == "cpu-temperature" {
			f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
			return f, err == nil
		}
		return 0, false
	}
	var list []map[string]string
	if json.Unmarshal(raw, &list) == nil {
		for _, r := range list {
			if f, ok := pick(r["name"], r["value"]); ok {
				return f, true
			}
		}
		return 0, false
	}
	var obj map[string]string
	if json.Unmarshal(raw, &obj) == nil {
		for k, v := range obj {
			if f, ok := pick(k, v); ok {
				return f, true
			}
		}
	}
	return 0, false
}

// Hosts reads /ip/hotspot/host.
func (c *REST) Hosts(ctx context.Context) ([]Host, error) {
	var rows []map[string]string
	if err := c.do(ctx, http.MethodGet, "/ip/hotspot/host", nil, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]Host, 0, len(rows))
	for _, r := range rows {
		addr := r["address"]
		if addr == "" {
			addr = r["to-address"]
		}
		out = append(out, Host{MAC: r["mac-address"], Address: addr, HostName: r["host-name"],
			Authorized: isTrue(r["authorized"]), Bypassed: isTrue(r["bypassed"])})
	}
	return out, nil
}
