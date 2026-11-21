package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

func TestParseGPS(t *testing.T) {
	ok := []struct {
		in       string
		lat, lng float64
	}{
		{"-1.2921, 36.8219", -1.2921, 36.8219},
		{"-1.2921,36.8219", -1.2921, 36.8219},
		{"  -1.2921 36.8219 ", -1.2921, 36.8219},
		{"-1.2921; 36.8219", -1.2921, 36.8219},
		{"+1.5, -179.9", 1.5, -179.9},
		{"0.0,0.0", 0, 0},
		{"90.0, 180.0", 90, 180},
		{"https://www.google.com/maps/@-1.2921,36.8219,15z", -1.2921, 36.8219},
		{"https://www.google.com/maps?q=-1.2921,36.8219", -1.2921, 36.8219},
		{"https://www.google.com/maps/search/?api=1&query=-1.2921%2C36.8219", -1.2921, 36.8219},
		{"https://maps.google.com/?q=-1.2921+36.8219", -1.2921, 36.8219},
		{"https://www.google.com/maps/place/MG+One/@-1.30,36.80,17z/data=!3m1!4b1!4m6!3m5!1s0x0:0x1!8m2!3d-1.2921!4d36.8219", -1.2921, 36.8219},
		{"https://www.google.com/maps/dir//-1.2921,36.8219/@-1.3,36.8,14z", -1.3, 36.8},
	}
	for _, c := range ok {
		lat, lng, err := ParseGPS(c.in)
		if err != nil || lat == nil || lng == nil || *lat != c.lat || *lng != c.lng {
			t.Errorf("ParseGPS(%q) = %v %v %v, want %v,%v", c.in, lat, lng, err, c.lat, c.lng)
		}
	}
	if lat, lng, err := ParseGPS("  "); lat != nil || lng != nil || err != nil {
		t.Errorf("empty input must mean no point: %v %v %v", lat, lng, err)
	}
	for _, in := range []string{"garbage", "-1.29", "1,2,3", "36.8219, -1.2921x", "91, 10", "-91, 10", "10, 181", "10, -181",
		"https://maps.app.goo.gl/abc123", "https://example.com/?q=hello", "javascript:alert(1)", "NaN, 1", "1e2, 3", "1,5", "-1,29", "1, 2", "-1.2921, 36", "-1, 36.8219"} {
		if lat, lng, err := ParseGPS(in); err == nil || lat != nil || lng != nil {
			t.Errorf("ParseGPS(%q) accepted: %v %v", in, lat, lng)
		}
	}
	for _, in := range []string{"1,5", "-1,29", "0,0", "-1.2921, 36", "90, 180"} {
		if _, _, err := ParseGPS(in); err != ErrGPSDecimals {
			t.Errorf("ParseGPS(%q) error = %v, want the dot-decimals error", in, err)
		}
	}
	if _, _, err := ParseGPS("garbage"); err != ErrBadGPS {
		t.Errorf("garbage error = %v", err)
	}
}

func TestValidGPS(t *testing.T) {
	good := [][2]*float64{{nil, nil}, {f64(0), f64(0)}, {f64(-90), f64(180)}, {f64(90), f64(-180)}}
	for _, g := range good {
		if err := ValidGPS(g[0], g[1]); err != nil {
			t.Errorf("%v %v: %v", g[0], g[1], err)
		}
	}
	bad := [][2]*float64{{f64(1), nil}, {nil, f64(1)}, {f64(90.0001), f64(0)}, {f64(0), f64(-180.1)}}
	for _, g := range bad {
		if err := ValidGPS(g[0], g[1]); err == nil {
			t.Errorf("%v %v accepted", g[0], g[1])
		}
	}
}

func TestMapURLAndFormat(t *testing.T) {
	a := Apartment{Lat: f64(-1.2921), Lng: f64(36.8219)}
	if got, want := a.MapURL(), "https://www.google.com/maps/search/?api=1&query=-1.2921%2C36.8219"; got != want {
		t.Errorf("MapURL = %q, want %q", got, want)
	}
	if (Apartment{}).MapURL() != "" || (Apartment{}).HasGPS() {
		t.Error("an apartment without GPS has no map link")
	}
	if !(Apartment{Lat: f64(0), Lng: f64(0)}).HasGPS() {
		t.Error("0,0 is a point")
	}
	if got := FormatGPS(-1.29210004, 36.8219); got != "-1.2921,36.8219" {
		t.Errorf("FormatGPS = %q", got)
	}
	if !SameApartment(Apartment{Name: "A", Lat: f64(1), Lng: f64(2)}, Apartment{Name: "A", Lat: f64(1), Lng: f64(2)}) ||
		SameApartment(Apartment{Lat: f64(1), Lng: f64(2)}, Apartment{Lat: f64(1), Lng: f64(3)}) ||
		SameApartment(Apartment{Lat: f64(0), Lng: f64(0)}, Apartment{}) {
		t.Error("SameApartment must compare values, and 0,0 differs from unset")
	}
}

// GPS is additive: format 9 files without the fields still load (no point), a point
// survives a write and read, 0,0 included, and unset fields are not written at all.
func TestApartmentGPSJSONFormat(t *testing.T) {
	if FormatVersion != 9 {
		t.Fatalf("GPS was designed without a format bump; format is now %d", FormatVersion)
	}
	dir := t.TempDir()
	old := filepath.Join(dir, "old.json")
	if err := os.WriteFile(old, []byte(`{"format":9,"apartments":{"a1":{"id":"a1","name":"Old","caretaker":{},"landlord":{},"created_at":"2026-01-01T00:00:00Z"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d, from, err := ReadJSON(old)
	if err != nil || from != 9 || d.Apartments["a1"].HasGPS() {
		t.Fatalf("old file: %v from=%d %+v", err, from, d.Apartments["a1"])
	}
	d.Apartments["a2"] = Apartment{ID: "a2", Name: "Null island", Lat: f64(0), Lng: f64(0)}
	d.Apartments["a3"] = Apartment{ID: "a3", Name: "Nairobi", Lat: f64(-1.2921), Lng: f64(36.8219)}
	p := filepath.Join(dir, "new.json")
	if err := WriteJSON(p, d); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	var generic struct {
		Apartments map[string]map[string]any `json:"apartments"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if _, has := generic.Apartments["a1"]["lat"]; has {
		t.Error("an unset point must not be written")
	}
	if v, has := generic.Apartments["a2"]["lat"]; !has || v != float64(0) {
		t.Errorf("0,0 must be written as a point: %v", generic.Apartments["a2"])
	}
	back, _, err := ReadJSON(p)
	if err != nil {
		t.Fatal(err)
	}
	if a := back.Apartments["a3"]; !a.HasGPS() || *a.Lat != -1.2921 || *a.Lng != 36.8219 {
		t.Errorf("a3: %+v", a)
	}
	if a := back.Apartments["a2"]; !a.HasGPS() || *a.Lat != 0 || *a.Lng != 0 {
		t.Errorf("a2: %+v", a)
	}
	if back.Apartments["a1"].HasGPS() {
		t.Error("a1 gained a point")
	}
}

func TestValidateReportsBadApartmentGPS(t *testing.T) {
	d := &Data{}
	d.init()
	d.Apartments["a1"] = Apartment{ID: "a1", Name: "Half", Lat: f64(1)}
	d.Apartments["a2"] = Apartment{ID: "a2", Name: "Far", Lat: f64(100), Lng: f64(0)}
	d.Apartments["a3"] = Apartment{ID: "a3", Name: "Fine", Lat: f64(-1), Lng: f64(36)}
	var n int
	for _, p := range Validate(d) {
		if strings.Contains(p, "GPS") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d GPS problems, want 2: %v", n, Validate(d))
	}
}

// A file written before the "WhatsApp is the same as the phone" flag existed gets the flag where the WhatsApp
// number equals the phone (migration 0013 of the Postgres store, for the JSON file); other rows are untouched.
func TestJSONBackfillsWhatsAppSameAsPhone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.json")
	raw := `{"format":9,
"accounts":{"a1":{"id":"a1","phone":"254712345678","whatsapp":"254712345678"},"a2":{"id":"a2","phone":"254712345678","whatsapp":"254722000111"},"a3":{"id":"a3","phone":"254712345678"}},
"customers":{"c1":{"id":"c1","name":"X","phone":"254733000111","whatsapp":"254733000111"},"c2":{"id":"c2","name":"Y","phone":"254733000111","whatsapp":"254700000000"}}}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	d, _, err := ReadJSON(p)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Accounts["a1"].WASamePhone || d.Accounts["a2"].WASamePhone || d.Accounts["a3"].WASamePhone || !d.Customers["c1"].WASamePhone || d.Customers["c2"].WASamePhone {
		t.Fatalf("backfill: %+v %+v", d.Accounts, d.Customers)
	}
}
