package routeros

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Executor is the optional extra a client may offer: the tools on the Tools
// page and the equipment finder. REST and Mock implement it; the web layer
// type-asserts it, so a client without it simply has no console.
type Executor interface {
	// Execute runs one console command through /rest/execute and returns its
	// text output. Callers must pass the command through Classify first.
	Execute(ctx context.Context, cmd string) (string, error)
	// Traceroute runs /tool/traceroute and returns one row per hop.
	Traceroute(ctx context.Context, target string, maxHops int) ([]Hop, error)
	// Leases lists DHCP leases, optionally only those of one MAC address.
	Leases(ctx context.Context, mac string) ([]Lease, error)
}

// Hop is one traceroute hop (the last report of that hop).
type Hop struct {
	N                              int
	Address                        string
	Loss, Sent                     string
	Last, Avg, Best, Worst, Status string
}

// Lease is one /ip/dhcp-server/lease row.
type Lease struct {
	MAC, Address, HostName, Status, Server string
}

// executeTimeout bounds one console command: the web layer's server write
// timeout is 30 s, so the router call must end before it.
const executeTimeout = 20 * time.Second

// Execute runs a console command: POST /rest/execute with as-string, which
// answers {"ret": "<output>"}. Some RouterOS builds refuse as-string; for a
// plain "print" the answer then comes from a GET of the same path.
func (c *REST) Execute(ctx context.Context, cmd string) (string, error) {
	var raw json.RawMessage
	err := c.doT(ctx, executeTimeout, http.MethodPost, "/execute", nil, map[string]string{"script": cmd, "as-string": ""}, &raw)
	if err == nil {
		return parseExecuteResult(raw), nil
	}
	if ae, ok := err.(*APIError); ok && ae.Status == 400 {
		if path, ok := printPath(cmd); ok {
			if out, err2 := c.printViaGET(ctx, path); err2 == nil {
				return out, nil
			}
		}
	}
	return "", err
}

// parseExecuteResult accepts {"ret":"..."} and [{"ret":"..."}].
func parseExecuteResult(raw json.RawMessage) string {
	var one map[string]any
	if json.Unmarshal(raw, &one) == nil {
		if v, ok := one["ret"]; ok {
			return fmt.Sprint(v)
		}
	}
	var many []map[string]any
	if json.Unmarshal(raw, &many) == nil {
		var parts []string
		for _, m := range many {
			if v, ok := m["ret"]; ok {
				parts = append(parts, fmt.Sprint(v))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return strings.TrimSpace(string(raw))
}

// printPath turns "/ip/address/print" or "ip address print" into the REST
// path "/ip/address". Only a bare print qualifies (no arguments).
func printPath(cmd string) (string, bool) {
	f := strings.Fields(strings.ReplaceAll(strings.TrimSpace(cmd), "/", " "))
	for len(f) > 0 && (f[len(f)-1] == "terse" || f[len(f)-1] == "without-paging") {
		f = f[:len(f)-1] // the flags the console forces
	}
	if len(f) < 2 || f[len(f)-1] != "print" {
		return "", false
	}
	for _, w := range f[:len(f)-1] {
		for _, r := range w {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return "", false
			}
		}
	}
	return "/" + strings.Join(f[:len(f)-1], "/"), true
}

// printViaGET formats GET /rest/<path> rows as "key=value" lines.
func (c *REST) printViaGET(ctx context.Context, path string) (string, error) {
	var rows []map[string]any
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &rows); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, r := range rows {
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "%d", i)
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%v", k, r[k])
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// Traceroute runs POST /rest/tool/traceroute. RouterOS streams updates, so the
// answer holds many rows per hop; the last row of each hop is kept. The exact
// row shape differs between versions, so every field is read leniently.
func (c *REST) Traceroute(ctx context.Context, target string, maxHops int) ([]Hop, error) {
	if maxHops <= 0 || maxHops > 30 {
		maxHops = 20
	}
	var rows []map[string]any
	body := map[string]string{"address": target, "count": "1", "max-hops": strconv.Itoa(maxHops)}
	if err := c.doT(ctx, executeTimeout, http.MethodPost, "/tool/traceroute", nil, body, &rows); err != nil {
		return nil, err
	}
	return parseTraceroute(rows), nil
}

func parseTraceroute(rows []map[string]any) []Hop {
	str := func(r map[string]any, k string) string {
		if v, ok := r[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	byHop := map[int]Hop{}
	var order []int
	seq := 0
	for _, r := range rows {
		n := 0
		if s := str(r, ".section"); s != "" {
			n, _ = strconv.Atoi(s)
			n++ // sections count from 0
		} else if s := str(r, "hop"); s != "" {
			n, _ = strconv.Atoi(s)
		} else {
			seq++
			n = seq
		}
		h := Hop{N: n, Address: str(r, "address"), Loss: str(r, "loss"), Sent: str(r, "sent"), Last: str(r, "last"),
			Avg: str(r, "avg"), Best: str(r, "best"), Worst: str(r, "worst"), Status: str(r, "status")}
		if h.Address == "" && h.Status == "" && h.Last == "" {
			continue // an empty progress row
		}
		if _, seen := byHop[n]; !seen {
			order = append(order, n)
		}
		byHop[n] = h
	}
	sort.Ints(order)
	out := make([]Hop, 0, len(order))
	for _, n := range order {
		out = append(out, byHop[n])
	}
	return out
}

// Leases lists DHCP leases (GET /rest/ip/dhcp-server/lease), filtered by MAC
// address when mac is not empty.
func (c *REST) Leases(ctx context.Context, mac string) ([]Lease, error) {
	var q url.Values
	if mac != "" {
		q = url.Values{"mac-address": {strings.ToUpper(mac)}}
	}
	var rows []map[string]string
	if err := c.do(ctx, http.MethodGet, "/ip/dhcp-server/lease", q, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]Lease, 0, len(rows))
	for _, r := range rows {
		addr := r["active-address"]
		if addr == "" {
			addr = r["address"]
		}
		out = append(out, Lease{MAC: strings.ToUpper(r["mac-address"]), Address: addr, HostName: r["host-name"], Status: r["status"], Server: r["server"]})
	}
	return out, nil
}
