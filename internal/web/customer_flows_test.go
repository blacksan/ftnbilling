package web_test

import (
	"testing"
)

// F2-W1: the same "customer with a package, not activated" through three doors.
func TestCustomerFlow_CreateWithPackageNoActivate_ThreeDoors(t *testing.T) {
	r := newAPIRig(t)
	hour := r.pkgID(t, "1 Hour")
	apt := aptOf(t, r.st, r.B.ID)
	x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": apt, "site_id": r.B.ID, "unit": "P1", "mac": "AA:BB:CC:00:81:01", "package_id": hour}, nil)
	if x.Status != 201 {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}

	y := r.call(r.oper.Access, "POST", "/unregistered/create", map[string]any{"apartment_id": apt, "site_id": r.B.ID, "unit": "P2", "mac": "AA:BB:CC:00:81:02", "package_id": hour}, nil)
	if y.Status != 201 {
		t.Fatalf("%d %s", y.Status, y.Raw)
	}
	if y.JSON(t)["expires_at"] != nil {
		t.Errorf("a free period was granted: %v", y.JSON(t)["expires_at"])
	}
	// a package that does not exist is refused, not swallowed, and nothing is made
	z := r.call(r.oper.Access, "POST", "/unregistered/create", map[string]any{"apartment_id": apt, "site_id": r.B.ID, "unit": "P3", "mac": "AA:BB:CC:00:81:03", "package_id": "nope"}, nil)
	if z.Status != 422 && z.Status != 400 {
		t.Errorf("a bad package answered %d %s", z.Status, z.Raw)
	}
	// activate grants the first period, explicitly
	w := r.call(r.oper.Access, "POST", "/unregistered/create", map[string]any{"apartment_id": apt, "site_id": r.B.ID, "unit": "P4", "mac": "AA:BB:CC:00:81:04", "package_id": hour, "activate": true}, nil)
	if w.Status != 201 || w.JSON(t)["expires_at"] == nil {
		t.Errorf("activate:true: %d %s", w.Status, w.Raw)
	}
	if x.JSON(t)["state"] != y.JSON(t)["state"] {
		t.Errorf("same input, different outcome: /accounts=%v /unregistered/create=%v (free time granted by one door only)", x.JSON(t)["state"], y.JSON(t)["state"])
	}
}

// F2-W2: package change with no keep_time in the body (default) on a flat with paid time left.
func TestCustomerFlow_PackageChangeDefaultKeepTime(t *testing.T) {
	r := newAPIRig(t)
	a := r.acct(t, r.A, "K1", "AA:BB:CC:00:82:01")
	month := r.pkgID(t, "Monthly 20 Mbps")
	day := r.pkgID(t, "1 Day")
	if x := r.call(r.oper.Access, "POST", "/accounts/"+a.ID+"/extend", map[string]any{"package_id": month}, nil); x.Status != 200 {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	before := r.call(r.oper.Access, "GET", "/accounts/"+a.ID, nil, nil).JSON(t)["expires_at"]
	x := r.call(r.oper.Access, "POST", "/accounts/"+a.ID+"/package", map[string]any{"package_id": day}, nil)
	after := x.JSON(t)["expires_at"]
	if before != after {
		t.Errorf("POST /accounts/{id}/package without keep_time silently threw away paid time")
	}
}

// F2-W3: PATCH = move, then edit; the edit fails after the move went through.
func TestCustomerFlow_PatchMoveThenBadPhone(t *testing.T) {
	r := newAPIRig(t)
	a := r.acct(t, r.A, "M1", "AA:BB:CC:00:83:01")
	aptB := aptOf(t, r.st, r.B.ID)
	x := r.call(r.oper.Access, "PATCH", "/accounts/"+a.ID, map[string]any{"apartment_id": aptB, "site_id": r.B.ID, "phone": "notaphone"}, nil)
	cur := r.call(r.oper.Access, "GET", "/accounts/"+a.ID, nil, nil).JSON(t)
	if x.Status == 200 || cur["site_id"] != r.A.ID {
		t.Errorf("PATCH answered %d with a bad phone: nothing may be saved, but the customer is on %v", x.Status, cur["site_id"])
	}
}
