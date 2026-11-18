package netplan

import "testing"

func TestPlanSubnetRanges(t *testing.T) {
	p, err := PlanSubnet("10.5.50.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if p.Gateway != "10.5.50.1" || p.GatewayIP != "10.5.50.1/24" || p.PoolFrom != "10.5.50.10" || p.PoolTo != "10.5.50.254" ||
		p.UnregFrom != "10.5.50.10" || p.UnregTo != "10.5.50.59" || p.StaticFrom != "10.5.50.60" || p.StaticTo != "10.5.50.254" {
		t.Fatalf("%+v", p)
	}
	if !p.InStatic("10.5.50.60") || !p.InStatic("10.5.50.254") || p.InStatic("10.5.50.59") || p.InStatic("10.5.51.60") || p.InStatic("x") {
		t.Fatal("InStatic")
	}
	if !p.InNetwork("10.5.50.5") || p.InNetwork("10.5.51.5") {
		t.Fatal("InNetwork")
	}
	used := map[string]bool{"10.5.50.60": true, "10.5.50.61": true}
	if ip, ok := p.FirstFree(func(s string) bool { return used[s] }); !ok || ip != "10.5.50.62" {
		t.Fatalf("%s %v", ip, ok)
	}
	if _, ok := p.FirstFree(func(string) bool { return true }); ok {
		t.Fatal("a full range has no free address")
	}
	// A /20 keeps the same offsets.
	if q, err := PlanSubnet("10.8.16.0/20"); err != nil || q.StaticFrom != "10.8.16.60" || q.StaticTo != "10.8.31.254" {
		t.Fatalf("%+v %v", q, err)
	}
	for _, bad := range []string{"10.5.50.0/25", "10.5.50.0/19", "nonsense", "fd00::/64"} {
		if _, err := PlanSubnet(bad); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
}
