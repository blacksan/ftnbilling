package sites

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// The walled-lists invariant.
//
// The IPoE walled garden rejects every address in ftn-unreg and ftn-suspended
// (rules installed before the switch). Those rows therefore must only be
// enabled while ftn-dhcp is enabled and serving. Whenever it is not (any job
// state before the switch, the building on the hotspot, after any rollback)
// every ftn-unreg and ftn-suspended row of ours is disabled:
//
//   - the job disables leftovers before it creates anything (stepDHCP) and
//     again before it installs the walled rules (stepFirewall); only the
//     switch enables them;
//   - the billing reconciler disables them while it waits for the cutover;
//   - the manager sweeps every building at start and every minute (below);
//   - both rollback scripts disable them (rollbackBase), for routers that run
//     an older kit the server-side sweep covers the same.
//
// Pre-switch firewall audit (no other ftn rule touches hotspot clients before
// the switch): the "ftn: ipoe" filter and NAT rules are all gated on the
// walled lists or on the customer network of flats; the customer DNS accept
// (comment "ftn: customer dns") only accepts and sits on the customer
// interface; the DNS redirect is gated on ftn-unreg/ftn-suspended membership;
// "isolate flats" and the block rules match flat addresses only; the
// masquerade rule matches the customer network as source. With the lists
// disabled none of them matches a hotspot client.

// ftnDHCPServing reports whether the DHCP servers hold an enabled ftn-dhcp.
func ftnDHCPServing(servers []routeros.Row) bool {
	for _, r := range servers {
		if r["name"] == billing.DHCPServerName && !rowDisabled(r) {
			return true
		}
	}
	return false
}

// enabledWalls returns the enabled ftn-unreg and ftn-suspended rows, split
// into the ones the server made (it may switch them off) and the rest.
func enabledWalls(lists []routeros.Row) (ours, foreign []routeros.Row) {
	for _, r := range lists {
		if (r["list"] != billing.UnregList && r["list"] != billing.SuspendedList) || rowDisabled(r) {
			continue
		}
		if strings.HasPrefix(r["comment"], ftnPrefix) {
			ours = append(ours, r)
		} else {
			foreign = append(foreign, r)
		}
	}
	return ours, foreign
}

// wallOff disables every enabled walled-list row of ours while ftn-dhcp is not
// enabled. It returns how many rows it turned off.
func (j *jobCtl) wallOff() (int, error) {
	servers, err := j.rows("/ip/dhcp-server")
	if err != nil {
		return 0, routerErr(err, "reading the DHCP servers")
	}
	if ftnDHCPServing(servers) {
		return 0, nil
	}
	lists, err := j.rows(pathAddrList)
	if err != nil {
		return 0, routerErr(err, "reading the address lists")
	}
	mine, _ := enabledWalls(lists)
	for _, r := range mine {
		if err := j.w.set(j.ctx, pathAddrList, r, map[string]string{"disabled": "yes"}); err != nil {
			return 0, routerErr(err, "switching off the walled list row "+r["address"])
		}
	}
	return len(mine), nil
}

// SweepWalls runs one pass of the invariant over every building without a
// running job. The lease sweeper calls it at start and every minute.
func (m *Manager) SweepWalls(ctx context.Context) {
	var ids []string
	m.st.View(func(d *store.Data) {
		for id, s := range d.Sites {
			if !s.Removed && s.Runnable() {
				ids = append(ids, id)
			}
		}
	})
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		m.sweepWalls(ctx, id)
	}
}

func (m *Manager) sweepWalls(ctx context.Context, id string) {
	m.prov.mu.Lock()
	if m.prov.runs[id] != nil || time.Now().Before(m.prov.wallNext[id]) {
		m.prov.mu.Unlock()
		return
	}
	m.prov.mu.Unlock()
	var site store.Site
	m.st.View(func(d *store.Data) { site = d.Sites[id] })
	rt := m.Router(id)
	j := &jobCtl{m: m, ctx: ctx, siteID: id, o: ProvOptions{Trigger: "walls-sweep", Actor: ActorSystem}, tm: m.timing(), rt: rt, release: func() {}}
	j.site = m.Effective(site)
	j.insp, _ = rt.(routeros.Inspector)
	j.plan, _ = PlanSubnet(j.site.CustomerSubnet)
	j.newWriter()
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	j.ctx = c
	n, err := j.wallOff()
	m.prov.mu.Lock()
	if m.prov.wallNext == nil {
		m.prov.wallNext, m.prov.wallTry = map[string]time.Time{}, map[string]int{}
	}
	if err != nil {
		k := m.prov.wallTry[id] + 1
		m.prov.wallTry[id] = k
		m.prov.wallNext[id] = time.Now().Add(30 * time.Second << min(k, 4))
	} else {
		delete(m.prov.wallNext, id)
		delete(m.prov.wallTry, id)
	}
	m.prov.mu.Unlock()
	if err == nil && n > 0 {
		m.chk.drop(id)
		_ = m.st.Update(func(d *store.Data) error {
			d.AddEvent(store.Event{Time: m.now(), SiteID: id, Kind: "audit",
				Msg: fmt.Sprintf("router setup: %d walled list row(s) switched off, the building is not on IPoE yet", n)})
			return nil
		})
	}
}
