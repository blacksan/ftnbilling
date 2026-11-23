package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An older file kept the Wi-Fi choice on the ONU: loading moves it to the house, once, and never writes it back.
func TestLegacyDesiredMovesToTheHouse(t *testing.T) {
	dir := t.TempDir()
	raw := `{"format":9,"sites":{"s1":{"id":"s1","name":"A"}},"accounts":{"a1":{"id":"a1","site_id":"s1","code":"ABC234","flat":"1","unit":"1","mac":"AA:BB:CC:00:11:22","created_at":"2026-01-01T00:00:00Z"}},
"onus":{"o1":{"id":"o1","serial":"S1","account_id":"a1","link":"auto","profile":{},"desired":{"set":true,"same":true,"ssid":"Home","pass_enc":"c2VhbGVk","updated_at":"2026-02-02T00:00:00Z"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "ispbill.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.View(func(d *Data) {
		a := d.Accounts["a1"]
		if !a.HomeWiFi.Set || a.HomeWiFi.SSID != "Home" || a.HomeWiFi.PassEnc != "c2VhbGVk" {
			t.Fatalf("not moved: %+v", a.HomeWiFi)
		}
		if d.ONUs["o1"].LegacyDesired != nil {
			t.Fatal("legacy value kept on the ONU")
		}
	})
	if err := st.Update(func(d *Data) error { d.Settings["x"] = Setting{Key: "x", Value: "1"}; return nil }); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "ispbill.json"))
	if strings.Contains(string(b), `"desired"`) {
		t.Fatal("legacy field written back")
	}
	if !strings.Contains(string(b), `"home_wifi"`) {
		t.Fatal("home_wifi not written")
	}
}

func TestAccountWithoutWiFiHasNoField(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	_ = st.Update(func(d *Data) error {
		d.Sites["s1"] = Site{ID: "s1", Name: "A"}
		d.Accounts["a1"] = Account{ID: "a1", SiteID: "s1", Code: "ABC234", Flat: "1", MAC: "AA:BB:CC:00:11:22", CreatedAt: time.Now()}
		return nil
	})
	b, _ := os.ReadFile(filepath.Join(dir, "ispbill.json"))
	if strings.Contains(string(b), "home_wifi") {
		t.Fatal("an account with no Wi-Fi choice carries the field")
	}
}

// ReleaseONU unlinks the box, cancels its queued Wi-Fi, queues a factory reset and marks it returned.
func TestReleaseONUQueuesFactoryReset(t *testing.T) {
	d := &Data{}
	d.init()
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	d.ONUs["o1"] = ONU{ID: "o1", Serial: "S1", AccountID: "a1", Link: LinkInventory, State: ONUState{AppliedFor: "a1", AppliedAt: now}}
	w := d.QueueONUTask(d.ONUs["o1"], TaskWiFi, "x", now)
	rt, ok := d.ReleaseONU("o1", now, "owner")
	if !ok || rt.Kind != TaskFactoryReset || rt.Status != TaskPending {
		t.Fatalf("%+v %v", rt, ok)
	}
	o := d.ONUs["o1"]
	if o.AccountID != "" || o.Link != "" || !o.State.Returned || o.State.AppliedFor != "" {
		t.Fatalf("%+v", o)
	}
	if d.ONUTasks[w.ID].Status != TaskFailed {
		t.Fatal("queued Wi-Fi not cancelled")
	}
	// a second release does not queue a second reset
	rt2, _ := d.ReleaseONU("o1", now, "owner")
	if rt2.ID != rt.ID {
		t.Fatal("two resets queued")
	}
}

func TestTaskTTL(t *testing.T) {
	if TaskTTL(TaskReboot) != time.Hour || TaskTTL(TaskWiFi) != 24*time.Hour || TaskTTL(TaskFactoryReset) != 7*24*time.Hour {
		t.Fatal("expiry times")
	}
}
