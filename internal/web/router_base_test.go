package web_test

import (
	"testing"

	"ispbill/internal/store"
)

// The Android router form sends only the fields that changed plus the base_* layout it was shown. A Ports-tab
// change made while the form was open is not undone by an unrelated save, and a stale port edit is a 409.
func TestAPIRouterPatchBaseLayout(t *testing.T) {
	r, _, path := portsRig(t)
	id := r.A.ID
	ports := func() string {
		var cp string
		r.st.View(func(d *store.Data) { cp = d.Sites[id].CustomerPorts })
		return cp
	}
	// the Ports tab makes ether4 a customer port while the form (loaded with customer_ports = []) is open
	if res := r.call(r.owner.Access, "PATCH", path+"/ether4", map[string]any{"role": "customer"}, nil); res.Status != 200 {
		t.Fatalf("ports tab: %d %s", res.Status, res.Raw)
	}
	if ports() != "ether4" {
		t.Fatalf("setup: %q", ports())
	}
	// 1. a form that sends the old list with the layout it was shown: stale, refused, nothing changes
	res := r.call(r.owner.Access, "PATCH", "/routers/"+id, map[string]any{"support_phone": "0700000001", "customer_ports": []string{}, "base_customer_ports": []string{}}, nil)
	if res.Status != 200 && res.Status != 409 {
		t.Fatalf("unexpected %d %s", res.Status, res.Raw)
	}
	if ports() != "ether4" {
		t.Fatalf("the stale form removed ether4 (status %d)", res.Status)
	}
	// ... an unchanged port field that carries its base keeps the stored value, and the phone is saved
	if res.Status != 200 {
		t.Fatalf("an unchanged customer_ports with its base must not be stale: %d %s", res.Status, res.Raw)
	}
	// 2. a real edit of customer_ports from a stale base is a 409
	res = r.call(r.owner.Access, "PATCH", "/routers/"+id, map[string]any{"customer_ports": []string{"ether5"}, "base_customer_ports": []string{}}, nil)
	if res.Status != 409 {
		t.Fatalf("a stale edit of customer_ports: %d %s", res.Status, res.Raw)
	}
	if ports() != "ether4" {
		t.Fatalf("ports now %q", ports())
	}
	// 3. the same edit from the current base goes through
	res = r.call(r.owner.Access, "PATCH", "/routers/"+id, map[string]any{"customer_ports": []string{"ether4", "ether5"}, "base_customer_ports": []string{"ether4"}}, nil)
	if res.Status != 200 || ports() != "ether4,ether5" {
		t.Fatalf("a current edit: %d %s ports=%q", res.Status, res.Raw, ports())
	}
	// 4. only the changed field, no ports at all: the ports are left alone
	res = r.call(r.owner.Access, "PATCH", "/routers/"+id, map[string]any{"support_phone": "0700000002"}, nil)
	if res.Status != 200 || ports() != "ether4,ether5" {
		t.Fatalf("a phone-only save: %d %s ports=%q", res.Status, res.Raw, ports())
	}
}
