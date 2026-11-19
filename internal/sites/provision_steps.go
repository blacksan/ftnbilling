package sites

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

const (
	pathAddrList = "/ip/firewall/address-list"
	pathFilter   = "/ip/firewall/filter"
	pathNAT      = "/ip/firewall/nat"
	masqComment  = "ftn: ipoe customer masquerade"
)

// build runs the steps after the classification and the preflight.
func (j *jobCtl) build(kind string, cl classification) error {
	m := j.m
	j.cust = cl.cust
	j.newWriter()
	j.flats = j.flatsOf()
	migrate := kind == KindMigrate
	if !migrate && j.cust.found {
		// switching off an existing hotspot or DHCP server needs the safety kit (review S4)
		if hs, dh, err := j.existingServers(); err == nil && len(hs)+len(dh) > 0 {
			if it, ok := j.checks.Item(CheckKit); !ok || it.Status != StatusOK {
				return j.park(JobNeedsAction, "this router still has a hotspot or DHCP server on the customer network and the safety kit is not installed: paste the join line again to install the safety kit")
			}
		}
	}
	// the lease wait can take up to an hour: it runs before the reconcile lock is taken
	if err := j.runStep(StepLeases, j.stepLeases); err != nil {
		j.restoreLeases(context.Background())
		return err
	}
	if p := m.provisioner(); p != nil {
		j.release = p.HoldSite(j.siteID)
	}
	if migrate {
		j.setState(JobArming)
	} else {
		j.setState(JobBuilding)
		j.ensureIPoEMode()
	}

	// backup and arm
	needArm := migrate
	if !migrate {
		needArm = j.freshNeedsArm()
	}
	if needArm {
		if err := j.runStep(StepBackup, j.stepBackup); err != nil {
			return err
		}
		if err := j.runStep(StepArm, j.stepArm); err != nil {
			return err
		}
	} else {
		j.step(StepBackup, StepSkipped, "nothing existing is switched off")
		j.step(StepArm, StepSkipped, "nothing existing is switched off")
	}
	for _, s := range []struct {
		key string
		f   func() (string, error)
	}{{StepNetwork, j.stepNetwork}, {StepDHCP, j.stepDHCP}, {StepFirewall, j.stepFirewall}} {
		if err := j.runStep(s.key, s.f); err != nil {
			return err
		}
	}
	if migrate {
		j.setState(JobSwitching)
	}
	if err := j.runStep(StepSwitch, j.stepSwitch); err != nil {
		return err
	}
	// telemetry never rolls anything back: the customers are already switched
	j.step(StepTelemetry, StepRunning, "")
	if d, err := j.stepTelemetry(); err != nil {
		j.step(StepTelemetry, StepFailed, err.Error())
		j.log("traffic reporting not set up: %v", err)
	} else {
		j.step(StepTelemetry, StepOK, d)
	}
	if err := j.runStep(StepVerify, j.stepVerify); err != nil {
		return err
	}
	// reconcile after the lock is released
	j.step(StepReconcile, StepRunning, "")
	j.release()
	if p := m.provisioner(); p != nil {
		if _, err := p.ReconcileSite(j.ctx, j.siteID); err != nil {
			j.step(StepReconcile, StepFailed, err.Error())
			j.log("first sync after the set-up: %v", err)
		} else {
			j.step(StepReconcile, StepOK, "")
		}
	} else {
		j.step(StepReconcile, StepSkipped, "")
	}
	j.job.LastError = ""
	j.setState(JobLive)
	switch kind {
	case KindMigrate:
		m.announce(j.siteID, fmt.Sprintf("Router %s is now on IPoE. %d of %d flats that were online are back on the new network.", j.site.Name, j.job.Evidence.FlatsBound, j.job.Evidence.FlatsRequired), j.dedupe("live"))
	default:
		m.announce(j.siteID, fmt.Sprintf("Router %s is set up and live.", j.site.Name), j.dedupe("live"))
	}
	return errParked
}

func (j *jobCtl) runStep(key string, f func() (string, error)) error {
	j.step(key, StepRunning, "")
	d, err := f()
	if err != nil {
		if fl, ok := err.(*failure); ok {
			j.step(key, StepFailed, fl.msg)
		} else {
			j.step(key, StepFailed, err.Error())
		}
		return err
	}
	j.step(key, StepOK, d)
	return nil
}

// ensureIPoEMode makes a fresh router an IPoE router (release plan D3).
func (j *jobCtl) ensureIPoEMode() {
	var cur string
	j.m.st.View(func(d *store.Data) { cur = d.Sites[j.siteID].AccessMode })
	if cur == store.AccessIPoE {
		return
	}
	_ = j.m.st.Update(func(d *store.Data) error {
		s := d.Sites[j.siteID]
		if billing.IPoE(s) && s.AccessMode == store.AccessIPoE {
			return store.ErrNoChange
		}
		s.AccessMode = store.AccessIPoE
		d.Sites[j.siteID] = s
		d.AddEvent(store.Event{Time: j.m.now(), SiteID: j.siteID, Kind: "audit", Msg: "router setup: access mode set to ipoe"})
		return nil
	})
	j.m.Reload(j.siteID)
}

// freshNeedsArm: a fresh set-up arms the kit only when it must switch off an
// existing hotspot or DHCP server, and only if the kit is installed.
func (j *jobCtl) freshNeedsArm() bool {
	if !j.cust.found {
		return false
	}
	if it, ok := j.checks.Item(CheckKit); !ok || it.Status != StatusOK {
		return false
	}
	hs, dh, _ := j.existingServers()
	return len(hs)+len(dh) > 0
}

// existingServers lists the enabled hotspot servers and old DHCP servers on the customer interface.
func (j *jobCtl) existingServers() (hotspots, dhcps []routeros.Row, err error) {
	if !j.cust.found {
		return nil, nil, nil
	}
	hrows, err := j.rt.Rows(j.ctx, "/ip/hotspot", nil)
	if err != nil {
		// no hotspot package: nothing to switch off
		hrows = nil
	}
	drows, err := j.rt.Rows(j.ctx, "/ip/dhcp-server", nil)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range hrows {
		if r["interface"] == j.cust.iface && !rowDisabled(r) {
			hotspots = append(hotspots, r)
		}
	}
	for _, r := range drows {
		if r["interface"] == j.cust.iface && r["name"] != billing.DHCPServerName && !rowDisabled(r) {
			dhcps = append(dhcps, r)
		}
	}
	return hotspots, dhcps, nil
}

// ---- kit markers ----------------------------------------------------------------------------

func (j *jobCtl) markerRows(list string) ([]routeros.Row, error) {
	return j.rt.Rows(j.ctx, pathAddrList, map[string]string{"list": list})
}

func (j *jobCtl) fileExists(name string) (bool, error) {
	rows, err := j.rt.Rows(j.ctx, "/file", map[string]string{"name": name})
	return len(rows) > 0, err
}

func (j *jobCtl) stepBackup() (string, error) {
	file := KitBackupFile + ".backup"
	if ok, err := j.fileExists(file); err != nil {
		return "", routerErr(err, "reading the router's files")
	} else if ok {
		return file + " is already on the router", nil
	}
	if rows, err := j.markerRows(KitListCmd); err != nil {
		return "", routerErr(err, "reading the kit markers")
	} else if len(rows) == 0 {
		if err := j.w.add(j.ctx, pathAddrList, map[string]string{"list": KitListCmd, "address": KitCmdAddress, "comment": KitCmdBackup}); err != nil {
			return "", routerErr(err, "asking the kit for a backup")
		}
	}
	deadline := time.Now().Add(j.tm.Backup)
	for {
		if ok, err := j.fileExists(file); err != nil {
			return "", routerErr(err, "reading the router's files")
		} else if ok {
			return "backup " + file + " made by the router", nil
		}
		if time.Now().After(deadline) {
			// take our own request back so a late kit does not make it after we gave up
			if rows, _ := j.markerRows(KitListCmd); len(rows) > 0 {
				for _, r := range rows {
					_ = j.w.remove(context.Background(), pathAddrList, r)
				}
			}
			return "", fail(JobFailed, "the router did not make its backup within %d seconds (is the safety kit's timer running?). Nothing was changed", int(j.tm.Backup.Seconds()))
		}
		select {
		case <-j.ctx.Done():
			return "", j.ctx.Err()
		case <-time.After(j.tm.Poll):
		}
	}
}

// ensureUndo writes one ftn-undo row (before the change it undoes). Idempotent.
func (j *jobCtl) ensureUndo(comment string) error {
	rows, err := j.markerRows(KitListUndo)
	if err != nil {
		return routerErr(err, "reading the kit markers")
	}
	used := map[string]bool{}
	for _, r := range rows {
		if r["comment"] == comment {
			return nil
		}
		used[r["address"]] = true
	}
	for n := 1; n < 1000; n++ {
		a := KitUndoAddress(n)
		if used[a] {
			continue
		}
		if err := j.w.add(j.ctx, pathAddrList, map[string]string{"list": KitListUndo, "address": a, "comment": comment}); err != nil {
			return routerErr(err, "writing an undo row")
		}
		return nil
	}
	return fail(JobFailed, "too many undo rows")
}

// undoComments are the rows the rollback needs for what the switch will change.
func (j *jobCtl) undoComments() ([]string, error) {
	if !j.cust.found {
		return nil, nil
	}
	hs, dh, err := j.existingServers()
	if err != nil {
		return nil, routerErr(err, "reading the old servers")
	}
	var out []string
	for _, r := range hs {
		if !ValidIface(r["name"]) {
			return nil, fail(JobFailed, "the hotspot server name %q is not a plain name", r["name"])
		}
		out = append(out, KitUndoHotspot(r["name"]))
	}
	for _, r := range dh {
		if !ValidIface(r["name"]) {
			return nil, fail(JobFailed, "the DHCP server name %q is not a plain name", r["name"])
		}
		out = append(out, KitUndoDHCP(r["name"]))
	}
	if !ValidIface(j.cust.iface) {
		return nil, fail(JobFailed, "the customer interface name %q is not a plain name", j.cust.iface)
	}
	kind, mode, found, err := j.arpModeOf(j.cust.iface)
	if err != nil {
		return nil, routerErr(err, "reading the interface's ARP mode")
	}
	if found {
		if mode == "" || mode == "reply-only" {
			mode = "enabled"
		}
		out = append(out, KitUndoARP(kind, j.cust.iface, mode))
	}
	return out, nil
}

// arpModeOf finds the interface in the bridge, vlan or ethernet menu.
func (j *jobCtl) arpModeOf(iface string) (kind, mode string, found bool, err error) {
	for _, k := range []string{"bridge", "vlan", "ethernet"} {
		rows, e := j.rt.Rows(j.ctx, "/interface/"+k, map[string]string{"name": iface})
		if e != nil {
			if k == "bridge" {
				return "", "", false, e
			}
			continue
		}
		if len(rows) > 0 {
			return k, rows[0]["arp"], true, nil
		}
	}
	return "", "", false, nil
}

func (j *jobCtl) stepArm() (string, error) {
	if _, err := j.cleanStrayNow(j.ctx); err != nil {
		return "", routerErr(err, "clearing a stray rollback marker")
	}
	armed, err := j.markerRows(KitListArmed)
	if err != nil {
		return "", routerErr(err, "reading the kit markers")
	}
	for _, r := range armed {
		if r["comment"] == KitArmedNow {
			return "", fail(JobFailed, "the router is rolling back right now; wait for it and run the set-up again")
		}
	}
	cs, err := j.undoComments()
	if err != nil {
		return "", err
	}
	for _, c := range cs { // every undo row exists before the marker, and before any change
		if err := j.ensureUndo(c); err != nil {
			return "", err
		}
	}
	j.job.Armed = true // from here on a failure sets the rollback marker
	j.save()
	if len(armed) == 0 {
		if err := j.w.add(j.ctx, pathAddrList, map[string]string{"list": KitListArmed, "address": KitArmedAddress, "comment": "0"}); err != nil {
			return "", routerErr(err, "arming the rollback")
		}
	}
	j.job.DeadlineAt = j.m.now().Add(KitArmTicks * 30 * time.Second)
	j.save()
	deadline := time.Now().Add(j.tm.Arm)
	for {
		rows, err := j.markerRows(KitListArmed)
		if err != nil {
			return "", routerErr(err, "reading the kit markers")
		}
		if len(rows) > 0 {
			if n, e := strconv.Atoi(strings.TrimSpace(rows[0]["comment"])); e == nil && n >= 1 {
				return fmt.Sprintf("armed: the router rolls itself back in 15 minutes unless this job keeps the change (%d undo row(s))", len(cs)), nil
			}
		}
		if time.Now().After(deadline) {
			return "", fail(JobFailed, "the safety kit did not tick within %d seconds, so the rollback timer is not running. Nothing was switched", int(j.tm.Arm.Seconds()))
		}
		select {
		case <-j.ctx.Done():
			return "", j.ctx.Err()
		case <-time.After(j.tm.Poll):
		}
	}
}

// rearm restarts the kit's countdown (the marker comment is the tick count).
func (j *jobCtl) rearm() error {
	rows, err := j.markerRows(KitListArmed)
	if err != nil {
		return routerErr(err, "reading the kit markers")
	}
	if len(rows) == 0 || rows[0]["comment"] == KitArmedNow {
		return fail(JobFailed, "the rollback marker is gone or already says now: nothing is switched")
	}
	if rows[0]["comment"] != "1" {
		if err := j.w.set(j.ctx, pathAddrList, rows[0], map[string]string{"comment": "1"}); err != nil {
			return routerErr(err, "restarting the rollback countdown")
		}
	}
	j.job.DeadlineAt = j.m.now().Add(KitArmTicks * 30 * time.Second)
	j.armDeadline = time.Now().Add(j.tm.countdown())
	j.save()
	return nil
}

// setNow sets the armed marker to "now": the router rolls back within 30 seconds.
func (j *jobCtl) setNow(ctx context.Context) error {
	if j.w == nil {
		j.newWriter()
	}
	armed, undo, err := kitMarkers(ctx, j.rt)
	if err != nil {
		return err
	}
	if len(armed) == 0 {
		// a marker is only ever created by a job that holds undo rows (between arming and the switch)
		if !j.job.Armed || len(undo) == 0 {
			return ErrNothingToRollBack
		}
		return j.w.add(ctx, pathAddrList, map[string]string{"list": KitListArmed, "address": KitArmedAddress, "comment": KitArmedNow})
	}
	if armed[0]["comment"] == KitArmedNow {
		return nil
	}
	return j.w.set(ctx, pathAddrList, armed[0], map[string]string{"comment": KitArmedNow})
}

// disarm removes the markers: the change is kept.
func (j *jobCtl) disarm(ctx context.Context) error {
	for _, l := range []string{KitListArmed, KitListUndo, KitListCmd} {
		rows, err := j.rt.Rows(ctx, pathAddrList, map[string]string{"list": l})
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := j.w.remove(ctx, pathAddrList, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- network ----------------------------------------------------------------------------------

func (j *jobCtl) stepNetwork() (string, error) {
	if j.cust.found {
		j.w.sc.iface = j.cust.iface
		return "customer network found on " + j.cust.iface, nil
	}
	if j.job.Kind == KindMigrate {
		return "", fail(JobNeedsAction, "no interface carries %s; check the router's customer subnet", j.plan.GatewayIP)
	}
	ifs, err := j.rows("/interface")
	if err != nil {
		return "", routerErr(err, "reading the interfaces")
	}
	olt := j.site.OLTInterface
	have := false
	for _, r := range ifs {
		if r["name"] == olt {
			have = true
		}
	}
	if !have {
		return "", fail(JobNeedsAction, "the router has no interface called %s (the OLT port): check the router's settings", olt)
	}
	if err := j.guardFreshNetwork(olt); err != nil {
		return "", err
	}
	port := olt
	if j.site.VLANID > 0 {
		port = j.w.sc.vlan
		vrows, err := j.rows("/interface/vlan")
		if err != nil {
			return "", routerErr(err, "reading the VLANs")
		}
		if _, _, err := j.ensure("/interface/vlan", vrows, byName(port), map[string]string{
			"name": port, "vlan-id": strconv.Itoa(j.site.VLANID), "interface": olt, "comment": "ftn: customer VLAN"}); err != nil {
			return "", err
		}
	}
	brs, err := j.rows("/interface/bridge")
	if err != nil {
		return "", routerErr(err, "reading the bridges")
	}
	if _, _, err := j.ensure("/interface/bridge", brs, byName(HotspotBridge), map[string]string{"name": HotspotBridge, "comment": "ftn: customer bridge"}); err != nil {
		return "", err
	}
	ports, err := j.rows("/interface/bridge/port")
	if err != nil {
		return "", routerErr(err, "reading the bridge ports")
	}
	if _, _, err := j.ensure("/interface/bridge/port", ports, func(r routeros.Row) bool { return r["interface"] == port && r["bridge"] == HotspotBridge },
		map[string]string{"bridge": HotspotBridge, "interface": port, "comment": "ftn: customer port"}); err != nil {
		return "", err
	}
	addrs, err := j.rows("/ip/address")
	if err != nil {
		return "", routerErr(err, "reading the addresses")
	}
	if _, _, err := j.ensure("/ip/address", addrs, func(r routeros.Row) bool { return r["address"] == j.plan.GatewayIP && r["interface"] == HotspotBridge },
		map[string]string{"address": j.plan.GatewayIP, "interface": HotspotBridge, "comment": "ftn: customer gateway"}); err != nil {
		return "", err
	}
	j.setCustomer(HotspotBridge)
	// bench port and the rest of the automatic fixes now that the customer network exists
	if outs, err := j.m.ApplyAutoFixes(j.ctx, j.siteID); err == nil {
		for _, o := range outs {
			if len(o.Applied) > 0 {
				j.log("fix %s: %s", o.ID, strings.Join(o.Applied, "; "))
			}
		}
	}
	// look again now that the network exists: anything that went wrong stops the set-up before the DHCP server is built
	cs, err := j.m.Checks(j.ctx, j.siteID, true)
	if err != nil {
		return "", routerErr(err, "checking the router after the customer network was built")
	}
	skip := map[string]bool{CheckKit: true, CheckDMScheduler: true, CheckDNS: true, CheckCustDNS: true, CheckCustomerIf: true, CheckUnmanaged: true, CheckUnseen: true}
	var left []string
	for _, id := range cs.Blockers(false, false) {
		if !skip[id] {
			left = append(left, id)
		}
	}
	if len(left) > 0 {
		j.job.Blockers = left
		return "", fail(JobNeedsAction, "after the customer network was built these checks need attention: %s", strings.Join(left, ", "))
	}
	return "customer bridge " + HotspotBridge + " with " + j.plan.GatewayIP, nil
}

// guardFreshNetwork refuses to build the customer network when it could collide with something on the
// router: an address that overlaps the plan, an OLT port that belongs to the LAN or to another bridge.
func (j *jobCtl) guardFreshNetwork(olt string) error {
	sn := readSnapshot(j.ctx, j.rt, nil, []string{"/ip/address", "/interface/bridge/port", "/interface/vlan", "/interface/list/member",
		"/ip/dhcp-server", "/ip/dhcp-client", "/ip/hotspot", "/interface/bridge/host", "/interface/bridge"})
	for _, p := range []string{"/ip/address", "/interface/bridge/port", "/interface/list/member"} {
		if err := sn.errs[p]; err != nil {
			return routerErr(err, "reading "+p)
		}
	}
	pn, err := netip.ParsePrefix(j.plan.Network)
	if err != nil {
		return fail(JobNeedsAction, "the customer subnet %q is not valid", j.plan.Network)
	}
	for _, r := range sn.tbl("/ip/address") {
		if ours(r) && r["interface"] == HotspotBridge {
			continue
		}
		if p, e := netip.ParsePrefix(r["address"]); e == nil && p.Masked().Overlaps(pn) {
			return fail(JobNeedsAction, "the customer subnet %s overlaps the address %s on %s: pick another subnet in the router's settings", j.plan.Network, r["address"], r["interface"])
		}
	}
	port := olt
	if j.site.VLANID > 0 {
		port = j.w.sc.vlan
	}
	for _, r := range sn.tbl("/interface/list/member") {
		if strings.EqualFold(r["list"], "LAN") && !rowDisabled(r) && (r["interface"] == olt || r["interface"] == port) {
			return fail(JobNeedsAction, "%s is in the LAN interface list: it carries the building's own network, not the customers. Check the fibre port in the router's settings", r["interface"])
		}
	}
	if olt == "bridge" {
		return fail(JobNeedsAction, "the fibre port %q is the router's default bridge: pick the physical fibre port in the router's settings", olt)
	}
	a := assessPort(sn, j.site, olt)
	if len(a.hard) > 0 {
		return fail(JobNeedsAction, "the server will not use %s: %s", olt, strings.Join(a.hard, "; "))
	}
	if len(a.unknown) > 0 {
		return fail(JobNeedsAction, "%s could not be checked safely: %s", olt, strings.Join(a.unknown, "; "))
	}
	if len(a.rows) > 0 {
		br := a.rows[0]["bridge"]
		if j.site.VLANID > 0 {
			return fail(JobNeedsAction, "%s is a port of bridge %s and the building uses VLAN %d: RouterOS would not hand the VLAN traffic to the VLAN interface. Take the port out of the bridge first (the check Fibre port is not stuck in the bridge shows the command)", olt, br, j.site.VLANID)
		}
		return fail(JobNeedsAction, "%s is already a port of bridge %s (with no VLAN id set the customers would join that bridge's network). Take it out of the bridge, or set the building's VLAN id", olt, br)
	}
	return nil
}

// ---- DHCP, queues, lists -----------------------------------------------------------------------

func firstForeignQueue(rows []routeros.Row) string {
	for _, r := range rows {
		if !yes(r["dynamic"]) && !strings.HasPrefix(r["name"], "ftn-") {
			return r[routeros.RowID]
		}
	}
	return ""
}

func (j *jobCtl) stepDHCP() (string, error) {
	pl := j.plan
	// Staging starts from a clean wall: leftovers of an earlier cutover or rollback
	// (an enabled ftn-unreg range) must not wall hotspot clients before the switch.
	if _, err := j.wallOff(); err != nil {
		return "", err
	}
	pools, err := j.rows("/ip/pool")
	if err != nil {
		return "", routerErr(err, "reading the pools")
	}
	if _, _, err := j.ensure("/ip/pool", pools, byName(billing.UnregList), map[string]string{
		"name": billing.UnregList, "ranges": pl.UnregFrom + "-" + pl.UnregTo, "comment": "ftn: unregistered pool"}); err != nil {
		return "", err
	}
	servers, err := j.rows("/ip/dhcp-server")
	if err != nil {
		return "", routerErr(err, "reading the DHCP servers")
	}
	want := map[string]string{"name": billing.DHCPServerName, "interface": j.cust.iface, "address-pool": billing.UnregList, "lease-time": "10m",
		"authoritative": "yes", "add-arp": "yes", "comment": "ftn: IPoE DHCP"}
	found := false
	for _, r := range servers {
		if r["name"] == billing.DHCPServerName {
			found = true
		}
	}
	if !found {
		want["disabled"] = "yes" // enabled by the switch step
	}
	if _, _, err := j.ensure("/ip/dhcp-server", servers, byName(billing.DHCPServerName), want); err != nil {
		return "", err
	}
	nets, err := j.rows("/ip/dhcp-server/network")
	if err != nil {
		return "", routerErr(err, "reading the DHCP networks")
	}
	if _, _, err := j.ensure("/ip/dhcp-server/network", nets, func(r routeros.Row) bool { return r["address"] == pl.Network }, map[string]string{
		"address": pl.Network, "gateway": pl.Gateway, "dns-server": pl.Gateway, "comment": "ftn: IPoE DHCP network"}); err != nil {
		return "", err
	}
	lists, err := j.rows(pathAddrList)
	if err != nil {
		return "", routerErr(err, "reading the address lists")
	}
	unreg := map[string]string{"list": billing.UnregList, "address": pl.UnregFrom + "-" + pl.UnregTo, "comment": unregComment}
	if !hasRow(lists, func(r routeros.Row) bool { return r["list"] == billing.UnregList && r["comment"] == unregComment }) {
		unreg["disabled"] = "yes" // walled by the switch step
	}
	if _, _, err := j.ensure(pathAddrList, lists, func(r routeros.Row) bool { return r["list"] == billing.UnregList && r["comment"] == unregComment }, unreg); err != nil {
		return "", err
	}
	if j.job.Kind != KindMigrate {
		return "pool, DHCP server (off until the switch), network", nil
	}
	return j.stageFlats(lists)
}

const unregComment = "ftn: unregistered range"

func hasRow(rows []routeros.Row, f func(routeros.Row) bool) bool {
	for _, r := range rows {
		if f(r) {
			return true
		}
	}
	return false
}

// stageFlats writes the flats' lease, queue (off) and suspended entry (off), the way the cutover script did.
func (j *jobCtl) stageFlats(lists []routeros.Row) (string, error) {
	leases, err := j.rows("/ip/dhcp-server/lease")
	if err != nil {
		return "", routerErr(err, "reading the leases")
	}
	queues, err := j.rows("/queue/simple")
	if err != nil {
		return "", routerErr(err, "reading the queues")
	}
	before := firstForeignQueue(queues)
	for _, f := range j.flats {
		c := billing.AcctComment(f.Code, f.Flat)
		mine := func(r routeros.Row) bool {
			return strings.HasPrefix(r["comment"], billing.AcctPrefix+f.Code+" ") || r["comment"] == c
		}
		if _, _, err := j.ensure("/ip/dhcp-server/lease", leases, func(r routeros.Row) bool { return mine(r) && r["server"] == billing.DHCPServerName || mine(r) },
			map[string]string{"mac-address": f.MAC, "address": f.IP, "server": billing.DHCPServerName, "comment": c}); err != nil {
			return "", err
		}
		qw := map[string]string{"name": billing.QueueName(f.Code), "target": f.IP + "/32", "max-limit": f.Limit, "comment": c}
		if !hasRow(queues, func(r routeros.Row) bool { return r["name"] == qw["name"] }) {
			qw["disabled"] = "yes"
			if before != "" {
				qw["place-before"] = before
			}
		}
		if _, _, err := j.ensure("/queue/simple", queues, byName(qw["name"]), qw); err != nil {
			return "", err
		}
		if f.Suspended {
			sw := map[string]string{"list": billing.SuspendedList, "address": f.IP, "comment": c}
			if !hasRow(lists, func(r routeros.Row) bool { return r["list"] == billing.SuspendedList && r["address"] == f.IP }) {
				sw["disabled"] = "yes"
			}
			if _, _, err := j.ensure(pathAddrList, lists, func(r routeros.Row) bool { return r["list"] == billing.SuspendedList && r["address"] == f.IP }, sw); err != nil {
				return "", err
			}
		}
	}
	return fmt.Sprintf("pool, DHCP server (off until the switch), network, %d flat(s) staged off", len(j.flats)), nil
}

// ---- firewall and DNS -------------------------------------------------------------------------

func (j *jobCtl) stepFirewall() (string, error) {
	if _, err := fmtAddr(j.m.cfg.PublicIP); err != nil {
		return "", fail(JobNeedsAction, "PUBLIC_IP is not set on the server: walled flats could not reach the payment page")
	}
	// The walled rules reject ftn-unreg and ftn-suspended: those lists must be off until the switch.
	if _, err := j.wallOff(); err != nil {
		return "", err
	}
	rows, err := j.rows(pathFilter)
	if err != nil {
		return "", routerErr(err, "reading the firewall")
	}
	// 1. the permanent "customers may ask the router for DNS" accepts, before the ipoe rules exist (Addendum B)
	before, hasBefore := firstStaticInput(rows)
	for _, proto := range []string{"udp", "tcp"} {
		if hasRow(rows, func(r routeros.Row) bool {
			return r["comment"] == custDNSComment && r["protocol"] == proto && r["in-interface"] == j.cust.iface
		}) {
			continue
		}
		body := map[string]string{"chain": "input", "in-interface": j.cust.iface, "protocol": proto, "dst-port": "53", "action": "accept", "comment": custDNSComment}
		if hasBefore {
			body["place-before"] = before
		}
		if err := j.w.add(j.ctx, pathFilter, body); err != nil {
			return "", routerErr(err, "customer DNS rule")
		}
	}
	// 2. the walled garden
	fr, nr := ipoeRuleSet(j.plan, j.m.cfg.PublicIP, j.site.RESTPort)
	if err := j.ensureRules(pathFilter, fr); err != nil {
		return "", err
	}
	if err := j.ensureRules(pathNAT, nr); err != nil {
		return "", err
	}
	// 3. customer masquerade
	nrows, err := j.rows(pathNAT)
	if err != nil {
		return "", routerErr(err, "reading NAT")
	}
	if _, _, err := j.ensure(pathNAT, nrows, byComment(masqComment), map[string]string{
		"chain": "srcnat", "action": "masquerade", "src-address": j.plan.Network, "out-interface-list": j.site.WANList, "comment": masqComment}); err != nil {
		return "", err
	}
	// 4. DNS for customers, only behind a drop of DNS from the WAN
	d, err := j.ensureDNS()
	if err != nil {
		return "", err
	}
	return "walled garden, customer DNS rule, masquerade; " + d, nil
}

// ensureRules makes the table hold exactly these rules, in this order, at the top.
func (j *jobCtl) ensureRules(path string, want []ipoeRule) error {
	rows, err := j.rows(path)
	if err != nil {
		return routerErr(err, "reading "+path)
	}
	wantBy := map[string]ipoeRule{}
	for _, r := range want {
		wantBy[r.Comment] = r
	}
	haveBy := map[string]routeros.Row{}
	var stale []routeros.Row // rows of ours that are no longer wanted, and duplicates
	firstForeign := ""       // the first row of the table that is not ours: ours go above it
	orderOK := true
	next := 0 // index in want of the next rule expected in table order
	for _, r := range rows {
		c := r["comment"]
		if _, ok := wantBy[c]; ok {
			if _, dup := haveBy[c]; dup {
				stale = append(stale, r)
				continue
			}
			haveBy[c] = r
			idx := -1
			for i, w := range want {
				if w.Comment == c {
					idx = i
				}
			}
			if idx < next || firstForeign != "" {
				orderOK = false // out of order, or below a foreign rule
			} else {
				next = idx + 1
			}
		} else if strings.HasPrefix(c, "ftn: ipoe") && c != masqComment && !strings.HasPrefix(c, "ftn: ipoe v6") {
			stale = append(stale, r)
		} else if !yes(r["dynamic"]) && !strings.HasPrefix(c, ftnPrefix) && firstForeign == "" && r[routeros.RowID] != "" {
			firstForeign = r[routeros.RowID]
		}
	}
	if !orderOK {
		return j.rewriteRules(path, want, rows)
	}
	// repair rule by rule, last to first, so every added rule goes above the one after it and the table never loses a rule
	nextID := ""
	for i := len(want) - 1; i >= 0; i-- {
		body := want[i].Body(j.cust.iface)
		if r, ok := haveBy[want[i].Comment]; ok {
			if diff := diffRow(r, body); len(diff) > 0 {
				if err := j.w.set(j.ctx, path, r, diff); err != nil {
					return routerErr(err, "repairing a firewall rule")
				}
			}
			nextID = r[routeros.RowID]
			continue
		}
		switch {
		case nextID != "":
			body["place-before"] = nextID
		case firstForeign != "":
			body["place-before"] = firstForeign
		}
		id, err := j.w.addID(j.ctx, path, body)
		if err != nil {
			return routerErr(err, "adding a firewall rule")
		}
		nextID = id
	}
	for _, r := range stale {
		if err := j.w.remove(j.ctx, path, r); err != nil {
			return routerErr(err, "removing an old rule")
		}
	}
	return nil
}

// rewriteRules is the fallback when the rules are out of order: take them out and put them back at the top.
func (j *jobCtl) rewriteRules(path string, want []ipoeRule, rows []routeros.Row) error {
	wantBy := map[string]bool{}
	for _, r := range want {
		wantBy[r.Comment] = true
	}
	for _, r := range rows {
		c := r["comment"]
		if wantBy[c] || strings.HasPrefix(c, "ftn: ipoe") && c != masqComment && !strings.HasPrefix(c, "ftn: ipoe v6") {
			if err := j.w.remove(j.ctx, path, r); err != nil {
				return routerErr(err, "removing an old rule")
			}
		}
	}
	rows, err := j.rows(path)
	if err != nil {
		return routerErr(err, "reading "+path)
	}
	anchor := ""
	for _, r := range rows {
		if !yes(r["dynamic"]) && r[routeros.RowID] != "" {
			anchor = r[routeros.RowID]
			break
		}
	}
	for i := len(want) - 1; i >= 0; i-- {
		body := want[i].Body(j.cust.iface)
		if anchor != "" {
			body["place-before"] = anchor
		}
		id, err := j.w.addID(j.ctx, path, body)
		if err != nil {
			return routerErr(err, "adding a firewall rule")
		}
		anchor = id
	}
	return nil
}

func fmtAddr(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("empty")
	}
	return s, nil
}

func (j *jobCtl) ensureDNS() (string, error) {
	tables := append(append([]string(nil), dnsTables...), "/ipv6/firewall/filter", "/interface/pppoe-client", "/interface")
	sn := readSnapshot(j.ctx, j.rt, []string{"/ip/dns", "/ipv6/settings"}, tables)
	if err := sn.errs["/ip/dns"]; err != nil {
		return "", routerErr(err, "reading DNS")
	}
	if yes(sn.single("/ip/dns")["allow-remote-requests"]) {
		return "DNS for customers already on", nil
	}
	safeSet := map[string]bool{j.cust.iface: true}
	ok, why := dnsSafe(sn, j.site, safeSet)
	if !ok {
		if up, uwhy := uplinkInWAN(sn, j.site); !up {
			return "", fail(JobNeedsAction, "DNS for customers cannot be turned on safely: %s", uwhy)
		}
		rows := sn.tbl(pathFilter)
		before, hasBefore := firstStaticInput(rows)
		for _, proto := range []string{"udp", "tcp"} {
			if hasRow(rows, func(r routeros.Row) bool { return r["comment"] == dnsWANComment && r["protocol"] == proto }) {
				continue
			}
			body := map[string]string{"chain": "input", "action": "drop", "protocol": proto, "dst-port": "53", "in-interface-list": j.site.WANList, "comment": dnsWANComment}
			if hasBefore {
				body["place-before"] = before
			}
			if err := j.w.add(j.ctx, pathFilter, body); err != nil {
				return "", routerErr(err, "DNS drop rule")
			}
		}
		sn = readSnapshot(j.ctx, j.rt, []string{"/ip/dns", "/ipv6/settings"}, tables)
		if ok2, why2 := dnsSafe(sn, j.site, safeSet); !ok2 {
			return "", fail(JobNeedsAction, "DNS for customers cannot be turned on safely: %s (first: %s)", why2, why)
		}
	}
	if err := j.w.call(j.ctx, "/ip/dns/set", map[string]string{"allow-remote-requests": "yes"}); err != nil {
		return "", routerErr(err, "turning on DNS")
	}
	return "DNS for customers on (WAN DNS dropped)", nil
}

// ---- switch -----------------------------------------------------------------------------------

func (j *jobCtl) stepSwitch() (string, error) {
	if j.job.Kind == KindMigrate {
		// the building is an IPoE building from here on
		j.ensureIPoEMode()
	}
	// everything the switch changes has an undo row first
	if j.job.Armed {
		cs, err := j.undoComments()
		if err != nil {
			return "", err
		}
		for _, c := range cs {
			if err := j.ensureUndo(c); err != nil {
				return "", err
			}
		}
	}
	if j.job.Armed {
		// the evidence window can be long: the switch gets a fresh 15 minute countdown
		if err := j.rearm(); err != nil {
			return "", err
		}
	}
	if j.job.Kind == KindMigrate {
		if err := j.noteOnline(); err != nil { // the last look at who is online before the lights change
			return "", err
		}
		j.job.SwitchedAt = j.m.now()
		j.job.DeadlineAt = j.job.SwitchedAt.Add(j.tm.evidenceWindow(time.Duration(j.job.LongestLeaseSec) * time.Second))
		j.save()
	}
	hs, dh, err := j.existingServers()
	if err != nil {
		return "", routerErr(err, "reading the old servers")
	}
	n := 0
	for _, r := range hs {
		if err := j.w.set(j.ctx, "/ip/hotspot", r, map[string]string{"disabled": "yes"}); err != nil {
			return "", routerErr(err, "switching off the hotspot "+r["name"])
		}
		n++
	}
	for _, r := range dh {
		if err := j.w.set(j.ctx, "/ip/dhcp-server", r, map[string]string{"disabled": "yes"}); err != nil {
			return "", routerErr(err, "switching off the DHCP server "+r["name"])
		}
		n++
	}
	servers, err := j.rows("/ip/dhcp-server")
	if err != nil {
		return "", routerErr(err, "reading the DHCP servers")
	}
	for _, r := range servers {
		if r["name"] == billing.DHCPServerName && rowDisabled(r) {
			if err := j.w.set(j.ctx, "/ip/dhcp-server", r, map[string]string{"disabled": "no"}); err != nil {
				return "", routerErr(err, "switching on ftn-dhcp")
			}
		}
	}
	if j.job.Kind == KindMigrate {
		if err := j.switchFlats(); err != nil {
			return "", err
		}
	}
	// the unregistered range is walled
	lists, err := j.rows(pathAddrList)
	if err != nil {
		return "", routerErr(err, "reading the address lists")
	}
	for _, r := range lists {
		if r["list"] == billing.UnregList && r["comment"] == unregComment && rowDisabled(r) {
			if err := j.w.set(j.ctx, pathAddrList, r, map[string]string{"disabled": "no"}); err != nil {
				return "", routerErr(err, "walling the unregistered range")
			}
		}
	}
	// reply-only ARP: only addresses with a static entry (ours or the DHCP server's) can talk
	if kind, mode, found, err := j.arpModeOf(j.cust.iface); err != nil {
		return "", routerErr(err, "reading the ARP mode")
	} else if found && mode != "reply-only" {
		if j.job.Armed {
			if err := j.ensureUndo(KitUndoARP(kind, j.cust.iface, orStr(mode, "enabled"))); err != nil {
				return "", err
			}
		}
		rows, err := j.rt.Rows(j.ctx, "/interface/"+kind, map[string]string{"name": j.cust.iface})
		if err != nil || len(rows) == 0 {
			return "", routerErr(err, "reading the interface")
		}
		if err := j.w.set(j.ctx, "/interface/"+kind, rows[0], map[string]string{"arp": "reply-only"}); err != nil {
			return "", routerErr(err, "setting ARP to reply-only")
		}
	}
	return fmt.Sprintf("ftn-dhcp serving on %s; %d old server(s) switched off", j.cust.iface, n), nil
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// switchFlats enables what stageFlats wrote off, adds the static ARP entries and keeps unmanaged devices (keep_unknown).
func (j *jobCtl) switchFlats() error {
	arp, err := j.rows("/ip/arp")
	if err != nil {
		return routerErr(err, "reading ARP")
	}
	for _, f := range j.flats {
		c := billing.AcctComment(f.Code, f.Flat)
		if _, _, err := j.ensure("/ip/arp", arp, func(r routeros.Row) bool { return strings.HasPrefix(r["comment"], billing.AcctPrefix+f.Code+" ") },
			map[string]string{"address": f.IP, "mac-address": f.MAC, "interface": j.cust.iface, "comment": c}); err != nil {
			return err
		}
	}
	if j.job.KeepUnknown {
		keep, err := j.unmanaged()
		if err != nil {
			return routerErr(err, "finding the devices to keep")
		}
		leases, _ := j.rows("/ip/dhcp-server/lease")
		for _, k := range keep {
			if !hasRow(arp, func(r routeros.Row) bool {
				return r["comment"] == "ftn: keep" && strings.EqualFold(r["mac-address"], k.mac)
			}) {
				if err := j.w.add(j.ctx, "/ip/arp", map[string]string{"address": k.ip, "mac-address": k.mac, "interface": j.cust.iface, "comment": "ftn: keep"}); err != nil {
					return routerErr(err, "keeping a device")
				}
			}
			if !hasRow(leases, func(r routeros.Row) bool {
				return r["comment"] == "ftn: keep" && strings.EqualFold(r["mac-address"], k.mac)
			}) {
				if err := j.w.add(j.ctx, "/ip/dhcp-server/lease", map[string]string{"address": k.ip, "mac-address": k.mac, "server": billing.DHCPServerName, "comment": "ftn: keep"}); err != nil {
					return routerErr(err, "keeping a device")
				}
			}
		}
	}
	queues, err := j.rows("/queue/simple")
	if err != nil {
		return routerErr(err, "reading the queues")
	}
	for _, r := range queues {
		if strings.HasPrefix(r["comment"], billing.AcctPrefix) && rowDisabled(r) {
			if err := j.w.set(j.ctx, "/queue/simple", r, map[string]string{"disabled": "no"}); err != nil {
				return routerErr(err, "switching on a flat's queue")
			}
		}
	}
	lists, err := j.rows(pathAddrList)
	if err != nil {
		return routerErr(err, "reading the address lists")
	}
	for _, r := range lists {
		if r["list"] == billing.SuspendedList && strings.HasPrefix(r["comment"], billing.AcctPrefix) && rowDisabled(r) {
			if err := j.w.set(j.ctx, pathAddrList, r, map[string]string{"disabled": "no"}); err != nil {
				return routerErr(err, "walling an unpaid flat")
			}
		}
	}
	return nil
}

type keepDev struct{ mac, ip string }

// unmanaged finds the devices that would lose their connection under reply-only ARP:
// an ARP entry in the customer network with no flat and no lease, or a foreign static lease.
func (j *jobCtl) unmanaged() ([]keepDev, error) {
	leases, err := j.rows("/ip/dhcp-server/lease")
	if err != nil {
		return nil, err
	}
	arp, _ := j.rows("/ip/arp")
	flats := map[string]bool{}
	j.m.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID == j.siteID {
				if mac, ok := store.CanonMAC(a.MAC); ok {
					flats[mac] = true
				}
			}
		}
	})
	leased := map[string]bool{}
	var out []keepDev
	for _, r := range leases {
		mac, ok := store.CanonMAC(r["mac-address"])
		if !ok {
			continue
		}
		leased[mac] = true
		static := !yes(r["dynamic"])
		if static && r["server"] != billing.DHCPServerName && !strings.HasPrefix(r["comment"], ftnPrefix) && !flats[mac] {
			out = append(out, keepDev{mac, r["address"]})
		}
	}
	for _, r := range arp {
		mac, ok := store.CanonMAC(r["mac-address"])
		if !ok || flats[mac] || leased[mac] || strings.HasPrefix(r["comment"], ftnPrefix) || !j.plan.InNetwork(r["address"]) {
			continue
		}
		out = append(out, keepDev{mac, r["address"]})
	}
	return out, nil
}

func (j *jobCtl) stepTelemetry() (string, error) {
	res, err := j.m.applyFix(j.ctx, j.siteID, CheckTrafficFlow, false, ActorSystem, false)
	if err != nil {
		return "", err
	}
	if len(res.Applied) == 0 {
		return "traffic reporting already set", nil
	}
	return strings.Join(res.Applied, "; "), nil
}

// ---- verify (release plan A1) -----------------------------------------------------------------

// evidenceCall gives every router read its own time limit: one slow answer must not starve the reads after it.
func (j *jobCtl) evidenceCall(f func(ctx context.Context)) {
	c, cancel := context.WithTimeout(j.ctx, 6*time.Second)
	defer cancel()
	f(c)
}

// evidence reads what the A1 rule needs. A read that times out or fails leaves its part unproven
// ("unknown"): the job keeps polling until the window ends, it is never a verdict by itself.
func (j *jobCtl) evidence(required []flatRow) (ProvEvidence, bool) {
	ev := ProvEvidence{FlatsRequired: len(required)}
	unknown := func(what string) { ev.Unknown = append(ev.Unknown, what) }
	pinged := false
	j.evidenceCall(func(c context.Context) { pinged = j.rt.Ping(c) == nil })
	ev.TunnelUp = pinged
	if ev.TunnelUp {
		j.evidenceCall(func(c context.Context) {
			if r, err := j.rt.PingHost(c, ServerTunnelIP(j.m.pfx), 2); err == nil && r.Received == 0 {
				ev.TunnelUp = false
			} else if err != nil {
				unknown("the ping to the server")
			}
		})
		j.evidenceCall(func(c context.Context) {
			if r, err := j.rt.PingHost(c, "1.1.1.1", 2); err == nil && r.Received > 0 {
				ev.InternetOK = true
			} else if err != nil {
				unknown("the ping to the internet")
			}
		})
		// the customers' own path: a ping that leaves from the customer gateway address
		if sp, ok := j.rt.(routeros.SourcePinger); ok && j.plan.Gateway != "" {
			j.evidenceCall(func(c context.Context) {
				if r, err := sp.PingFrom(c, "1.1.1.1", j.plan.Gateway, 2); err == nil && r.Received > 0 {
					ev.CustomerPath = true
				} else if err != nil {
					unknown("the ping from the customer network")
				}
			})
		}
	} else {
		unknown("the tunnel")
	}
	servingOK := false
	j.evidenceCall(func(c context.Context) {
		if rows, err := j.rt.Rows(c, "/ip/dhcp-server", map[string]string{"name": billing.DHCPServerName}); err == nil && len(rows) > 0 && !rowDisabled(rows[0]) {
			servingOK = true
		} else if err != nil {
			unknown("the DHCP server")
		}
	})
	var leases []routeros.Row
	leasesRead := false
	j.evidenceCall(func(c context.Context) {
		if ls, err := j.rt.Rows(c, "/ip/dhcp-server/lease", nil); err == nil {
			leases, leasesRead = ls, true
		} else {
			unknown("the leases")
		}
	})
	if leasesRead {
		for _, f := range required {
			bound := false
			for _, l := range leases {
				if l["server"] == billing.DHCPServerName && boundLease(l) && strings.EqualFold(l["mac-address"], f.MAC) && l["address"] == f.IP {
					bound = true
				}
			}
			if bound {
				ev.FlatsBound++
			} else {
				ev.Missing = append(ev.Missing, f.Flat)
			}
		}
	}
	base := servingOK && ev.TunnelUp && ev.InternetOK && ev.CustomerPath && leasesRead && ev.FlatsBound == ev.FlatsRequired
	if base { // the dearer reads only once everything cheap holds
		ev.ConnSeen = len(required) == 0
		if !ev.ConnSeen {
			j.evidenceCall(func(c context.Context) {
				seen, err := j.connectionSeen(c, required)
				ev.ConnSeen = seen
				if err != nil {
					unknown("the connection table")
				}
			})
		}
		var dnsErr bool
		ev.DNSOK, ev.OLTOK, dnsErr = j.dnsAndOLT()
		if dnsErr {
			unknown("the DNS and fibre port checks")
		}
	}
	pass := base && ev.ConnSeen && ev.DNSOK && ev.OLTOK
	return ev, pass
}

// connectionSeen: one of the flats has a connection through the router to a public address that was answered.
// Only the fields it needs are read. err is set when the table could not be read (the answer is then unknown).
func (j *jobCtl) connectionSeen(ctx context.Context, required []flatRow) (bool, error) {
	rows, err := j.rt.Rows(ctx, "/ip/firewall/connection", map[string]string{".proplist": "src-address,dst-address,seen-reply,replied,repl-packets,repl-bytes"})
	if err != nil {
		return false, err
	}
	ips := map[string]bool{}
	for _, f := range required {
		ips[f.IP] = true
	}
	for _, r := range rows {
		src := hostOf(r["src-address"])
		if !ips[src] || !publicAddr(hostOf(r["dst-address"])) {
			continue
		}
		if yes(r["seen-reply"]) || yes(r["replied"]) || atoiRow(r["repl-packets"]) > 0 || atoiRow(r["repl-bytes"]) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func atoiRow(v string) int { n, _ := strconv.Atoi(strings.TrimSpace(v)); return n }

// hostOf strips the port from "10.5.50.60:51234".
func hostOf(a string) string {
	if i := strings.LastIndexByte(a, ':'); i >= 0 && strings.Count(a, ":") == 1 {
		return a[:i]
	}
	return a
}

func publicAddr(h string) bool {
	a, err := netip.ParseAddr(h)
	return err == nil && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast()
}

// dnsAndOLT: customers can use the router's DNS (the allow-remote-requests switch and the customer DNS rule,
// both checked on the router now), and the OLT port is not stuck in a bridge.
func (j *jobCtl) dnsAndOLT() (dnsOK, oltOK, unread bool) {
	c, cancel := context.WithTimeout(j.ctx, 20*time.Second)
	defer cancel()
	if j.insp != nil {
		r, err := j.insp.One(c, "/ip/dns")
		if err != nil {
			return false, false, true
		}
		if !yes(r["allow-remote-requests"]) {
			return false, false, false
		}
	}
	cs, err := j.m.Checks(c, j.siteID, true)
	if err != nil {
		return false, false, true
	}
	if it, ok := cs.Item(CheckCustDNS); ok && it.Status == StatusOK {
		dnsOK = true
	}
	oltOK = true
	if it, ok := cs.Item(CheckOLT); ok && it.Status != StatusOK && it.Status != StatusInfo {
		oltOK = false
	}
	return dnsOK, oltOK, false
}

func (ev ProvEvidence) text() string {
	var b []string
	if !ev.TunnelUp {
		b = append(b, "the tunnel is down")
	}
	if !ev.InternetOK {
		b = append(b, "the router cannot reach the internet (1.1.1.1)")
	}
	if ev.TunnelUp && !ev.CustomerPath {
		b = append(b, "a ping from the customer network does not get out (forwarding or masquerade)")
	}
	if ev.FlatsBound != ev.FlatsRequired {
		b = append(b, fmt.Sprintf("only %d of %d flats that were online came back (missing: %s)", ev.FlatsBound, ev.FlatsRequired, strings.Join(ev.Missing, ", ")))
	}
	if ev.FlatsRequired > 0 && ev.FlatsBound == ev.FlatsRequired && ev.CustomerPath && !ev.ConnSeen {
		b = append(b, "no flat has a connection to the internet that was answered")
	}
	if ev.CustomerPath && ev.FlatsBound == ev.FlatsRequired && !ev.DNSOK {
		b = append(b, "customers cannot use the router's DNS")
	}
	if ev.CustomerPath && ev.FlatsBound == ev.FlatsRequired && ev.DNSOK && !ev.OLTOK {
		b = append(b, "the fibre port is stuck in a bridge")
	}
	if len(ev.Unknown) > 0 {
		b = append(b, "could not be read in time: "+strings.Join(ev.Unknown, ", "))
	}
	if len(b) == 0 {
		return "ftn-dhcp is not serving"
	}
	return strings.Join(b, "; ")
}

// stepVerify decides: keep the change (remove the markers) or make the router roll back.
func (j *jobCtl) stepVerify() (string, error) {
	j.setState(JobVerifying)
	migrate := j.job.Kind == KindMigrate
	var required []flatRow
	window := 10 * j.tm.Poll
	if migrate {
		required = j.requiredFlats(j.job.SwitchedAt)
		window = j.tm.evidenceWindow(time.Duration(j.job.LongestLeaseSec) * time.Second)
	}
	deadline := time.Now().Add(window)
	for {
		ev, pass := j.evidence(required)
		j.job.Evidence = ev
		j.save()
		if pass {
			break
		}
		if time.Now().After(deadline) {
			return "", fail(JobFailed, "the evidence did not hold: %s", ev.text())
		}
		select {
		case <-j.ctx.Done():
			return "", j.ctx.Err()
		case <-time.After(j.tm.Poll):
		}
	}
	// keep it. The decision is persisted BEFORE the markers go: a crash in between must
	// never make a restart roll a confirmed router back.
	j.job.Armed = false
	j.save()
	// keep trying until the kit's own countdown would have run out
	dl := j.armDeadline
	if dl.IsZero() {
		dl = time.Now().Add(j.tm.countdown())
	}
	var last error
	for {
		c, cancel := context.WithTimeout(j.ctx, 10*time.Second)
		last = j.disarm(c)
		cancel()
		if last == nil {
			break
		}
		if time.Now().After(dl) {
			break
		}
		select {
		case <-j.ctx.Done():
			return "", j.ctx.Err()
		case <-time.After(j.tm.Poll):
		}
	}
	if last != nil {
		// The markers could not be removed before the kit's countdown ended, so the router has rolled itself back
		// (or is about to). The server follows it: hotspot mode, bypass rows, and the operators are told.
		return "", j.assumeRolledBack(fmt.Sprintf("the evidence held but the rollback markers could not be removed (%v)", shortErr(last)))
	}
	rc, rcancel := context.WithTimeout(j.ctx, 15*time.Second)
	j.restoreLeases(rc)
	rcancel()
	_ = j.m.st.Update(func(d *store.Data) error {
		now := j.m.now()
		billing.SetSiteMarkLocked(d, j.siteID, billing.MarkCutoverConfirmed, true, now)
		billing.SetSiteMarkLocked(d, j.siteID, billing.MarkCutoverSeen, true, now)
		d.AddEvent(store.Event{Time: now, SiteID: j.siteID, Kind: "audit", Msg: fmt.Sprintf("router setup: kept on evidence (%d of %d flats bound, tunnel up, internet ok)", j.job.Evidence.FlatsBound, j.job.Evidence.FlatsRequired)})
		return nil
	})
	if p := j.m.provisioner(); p != nil {
		p.SetHotspotBypass(j.siteID, false)
	}
	return fmt.Sprintf("kept: tunnel up, internet ok, %d of %d flats bound", j.job.Evidence.FlatsBound, j.job.Evidence.FlatsRequired), nil
}

// ---- rollback ---------------------------------------------------------------------------------

// assumeRolledBack is the end of a keep decision that could not be carried out: the markers outlived the kit's
// countdown, so the router's own timer has gone back to the old setup. The server's side is put back to match.
func (j *jobCtl) assumeRolledBack(reason string) *failure {
	if j.job.Kind == KindMigrate {
		j.m.revertToHotspotMode(j.siteID)
	}
	if p := j.m.provisioner(); p != nil && j.job.Kind == KindMigrate {
		p.SetHotspotBypass(j.siteID, true)
	}
	j.job.Armed = false
	msg := reason + ": the router's own timer rolls it back to its old setup"
	j.m.announce(j.siteID, fmt.Sprintf("Router %s: %s. Paying flats are on the hotspot with bypass so nobody loses internet.", j.site.Name, msg), j.dedupe("rolled_back"))
	return &failure{state: JobRolledBack, unreachable: true, msg: msg}
}

// rollbackTail follows a rollback the marker set off: it waits until the router
// has gone back, then puts the server's side back (hotspot mode, bypass rows)
// and tells the operators. The armed marker is "now" before this is called.
func (j *jobCtl) rollbackTail(reason string) {
	j.setState(JobRollingBack)
	deadline := time.Now().Add(j.tm.Rollback)
	back := false
	for {
		c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		armed, e1 := j.rt.Rows(c, pathAddrList, map[string]string{"list": KitListArmed})
		srv, e2 := j.rt.Rows(c, "/ip/dhcp-server", map[string]string{"name": billing.DHCPServerName})
		cancel()
		if e1 == nil && e2 == nil && len(armed) == 0 && (len(srv) == 0 || rowDisabled(srv[0])) {
			back = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		base := j.m.ctx
		if base == nil {
			base = context.Background()
		}
		select {
		case <-base.Done():
			j.job.LastError = "stopped while waiting for the router to roll back: " + reason
			j.setState(JobFailed)
			return
		case <-time.After(j.tm.Poll):
		}
	}
	if !back {
		j.job.LastError = "waiting for the router to roll back: " + reason
		j.setState(JobFailed)
		j.m.announce(j.siteID, fmt.Sprintf("Router %s: %s. The rollback was asked for but not seen yet; the router's own timer rolls it back within 15 minutes.", j.site.Name, reason), j.dedupe("rollback-wait"))
		return
	}
	j.job.Armed = false
	{
		rc, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		j.restoreLeases(rc)
		rcancel()
	}
	{ // back on the hotspot: nobody is walled (the router script did it too; the sweep repeats it)
		saved := j.ctx
		wc, wcancel := context.WithTimeout(context.Background(), 15*time.Second)
		j.ctx = wc
		if _, err := j.wallOff(); err != nil {
			j.log("walled lists after the rollback: %v", err)
		}
		j.ctx = saved
		wcancel()
	}
	if j.job.Kind == KindMigrate {
		j.m.revertToHotspotMode(j.siteID)
		if p := j.m.provisioner(); p != nil {
			p.SetHotspotBypass(j.siteID, true)
		}
	}
	j.release()
	if p := j.m.provisioner(); p != nil {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, _ = p.ReconcileSite(c, j.siteID) // hotspot mode: writes the bypass rows
		cancel()
	}
	j.job.LastError = reason
	j.setState(JobRolledBack)
	j.log("rolled back: %s", reason)
	j.m.announce(j.siteID, fmt.Sprintf("Router %s was rolled back to its old setup: %s. Paying flats are on the hotspot with bypass so nobody lost internet.", j.site.Name, reason), j.dedupe("rolled_back"))
}

// revertToHotspotMode puts an IPoE building back on the hotspot access mode
// (the router rolled back) and forgets the cutover marks.
func (m *Manager) revertToHotspotMode(siteID string) {
	changed := false
	_ = m.st.Update(func(d *store.Data) error {
		s, ok := d.Sites[siteID]
		if !ok || !billing.IPoE(s) {
			return store.ErrNoChange
		}
		s.AccessMode = store.AccessHotspot
		d.Sites[siteID] = s
		billing.ClearCutoverMarks(d, siteID)
		d.AddEvent(store.Event{Time: m.now(), SiteID: siteID, Kind: "audit", Msg: "router setup: rolled back, access mode set back to hotspot"})
		changed = true
		return nil
	})
	if changed {
		m.Reload(siteID)
	}
}

// ErrNothingToRollBack: the router has no armed rollback with undo rows, so there is nothing to go back to.
var ErrNothingToRollBack = errors.New("there is nothing to roll back: the router holds no pending change (no ftn-armed with ftn-undo rows)")

// kitMarkers reads the armed and undo rows.
func kitMarkers(ctx context.Context, rt routeros.Client) (armed, undo []routeros.Row, err error) {
	if armed, err = rt.Rows(ctx, pathAddrList, map[string]string{"list": KitListArmed}); err != nil {
		return
	}
	undo, err = rt.Rows(ctx, pathAddrList, map[string]string{"list": KitListUndo})
	return
}

// cleanStrayNow removes an armed marker that says "now" when no undo rows exist:
// it would only make the kit run a rollback script with nothing to undo, and a
// later change would be rolled back the moment the kit is armed again.
func (j *jobCtl) cleanStrayNow(ctx context.Context) (bool, error) {
	armed, undo, err := kitMarkers(ctx, j.rt)
	if err != nil || len(undo) > 0 {
		return false, err
	}
	removed := false
	for _, r := range armed {
		if r["comment"] == KitArmedNow {
			if j.w == nil {
				j.newWriter()
			}
			if err := j.w.remove(ctx, pathAddrList, r); err != nil {
				return removed, err
			}
			removed = true
		}
	}
	return removed, nil
}

// RollbackNow is the emergency "Roll back now": it stops a running job, sets the
// rollback marker (the router goes back within 30 seconds) and follows the
// rollback in the background. It returns once the marker is set. It never
// creates a marker: with nothing armed (a router that is live and confirmed) it
// returns ErrNothingToRollBack.
func (m *Manager) RollbackNow(siteID, actor string) error {
	var site store.Site
	var found bool
	m.st.View(func(d *store.Data) { site, found = d.Sites[siteID] })
	if !found || site.Removed {
		return billing.ErrNoSite
	}
	m.prov.mu.Lock()
	cur := m.prov.runs[siteID]
	following := cur != nil && cur.state == JobRollingBack
	m.prov.mu.Unlock()
	if following { // a rollback is already under way: asking again changes nothing
		return nil
	}
	rt := m.Router(siteID)
	j := &jobCtl{m: m, ctx: context.Background(), siteID: siteID, o: ProvOptions{Trigger: "rollback", Actor: actor}, tm: m.timing(), rt: rt, release: func() {}}
	j.site = m.Effective(site)
	j.insp, _ = rt.(routeros.Inspector)
	j.plan, _ = PlanSubnet(j.site.CustomerSubnet)
	j.newWriter()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	armed, undo, err := kitMarkers(c, rt)
	if err != nil {
		return err
	}
	if len(armed) == 0 || len(undo) == 0 {
		if _, e := j.cleanStrayNow(c); e != nil {
			return e
		}
		return ErrNothingToRollBack
	}
	// reserve the run entry before stopping anything, so no other job can start in between
	var run *provRun
	for try := 0; try < 50; try++ {
		m.prov.mu.Lock()
		if m.prov.runs == nil {
			m.prov.runs = map[string]*provRun{}
		}
		cur := m.prov.runs[siteID]
		if cur == nil {
			run = &provRun{cancel: func() {}, done: make(chan struct{}), state: JobRollingBack}
			m.prov.runs[siteID] = run
			m.prov.mu.Unlock()
			break
		}
		if cur.state == JobRollingBack {
			m.prov.mu.Unlock()
			return nil
		}
		cur.cancel()
		done := cur.done
		m.prov.mu.Unlock()
		<-done
	}
	if run == nil {
		return ErrJobRunning
	}
	release := func() {
		m.prov.mu.Lock()
		delete(m.prov.runs, siteID)
		m.prov.mu.Unlock()
		close(run.done)
	}
	// The job is stopped now. Look again: it may have decided to keep the change while we waited.
	prev, _ := m.ProvJobOf(siteID)
	j.job = prev
	armed2, undo2, err := kitMarkers(c, rt)
	if err != nil {
		release()
		return err
	}
	kept := !prev.StartedAt.IsZero() && !prev.Armed && (prev.State == JobVerifying || prev.State == JobLive)
	if len(armed2) == 0 || len(undo2) == 0 || kept {
		if kept && (len(armed2) > 0 || len(undo2) > 0) {
			_ = j.disarm(c) // the keep decision was made: finish removing the markers it left
		}
		release()
		return ErrNothingToRollBack
	}
	if j.job.StartedAt.IsZero() {
		j.job.StartedAt = m.now()
		j.job.Steps = newSteps("")
	}
	j.job.Armed = true // the armed row exists (just read): setNow only changes its comment
	if err := j.setNow(c); err != nil {
		release()
		return err
	}
	j.job.Trigger = "rollback"
	j.job.StartNow = false
	j.job.State = JobRollingBack
	j.save()
	j.log("rollback requested by %s", actor)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer release()
		if p := m.provisioner(); p != nil {
			j.release = p.HoldSite(siteID)
			defer func() { j.release() }()
		}
		j.rollbackTail("rolled back by " + actor)
	}()
	return nil
}
