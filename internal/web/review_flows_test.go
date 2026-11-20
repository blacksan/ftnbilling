package web_test

import (
	"testing"

	"ispbill/internal/store"
)

// Review: the exact body the new Android form sends for a phone-only save (no customer_ports, all four base_*),
// after the Ports tab changed the customer ports while the form was open.
func TestReviewAndroidShapeUnrelatedSaveAfterPortsTab(t *testing.T) {
	r, _, path := portsRig(t)
	id := r.A.ID
	if res := r.call(r.owner.Access, "PATCH", path+"/ether4", map[string]any{"role": "customer"}, nil); res.Status != 200 {
		t.Fatalf("ports tab: %d %s", res.Status, res.Raw)
	}
	var st store.Site
	r.st.View(func(d *store.Data) { st = d.Sites[id] })
	body := map[string]any{"name": st.Name, "wan_mode": "dhcp", "support_phone": "0700000009",
		"base_lan_ports": st.LANPorts, "base_customer_ports": []string{}, "base_olt_interface": st.OLTInterface, "base_vlan_id": st.VLANID}
	res := r.call(r.owner.Access, "PATCH", "/routers/"+id, body, nil)
	r.st.View(func(d *store.Data) { st = d.Sites[id] })
	t.Logf("status %d body %s ports %q phone %q", res.Status, res.Raw, st.CustomerPorts, st.SupportPhone)
	if res.Status != 200 {
		t.Fatalf("an unrelated save (phone only) from the Android form is refused: %d", res.Status)
	}
}
