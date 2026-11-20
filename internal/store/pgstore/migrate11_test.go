package pgstore

import (
	"context"
	"testing"
)

// Migration 0011 adds the ports policy: a router that exists before it has no policy (the first sweep after the
// upgrade records what is in service) and no enabled ports; both values round-trip.
func TestMigration0011PortsPolicy(t *testing.T) {
	url, schema := newTestSchema(t)
	ctx := context.Background()
	old, err := Open(ctx, url, Options{Schema: schema, maxVersion: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.pool.Exec(ctx, `INSERT INTO sites (id, name, mode, created_at) VALUES ('s1','Before','wg','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	b, err := Open(ctx, url, Options{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	d, err := b.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := d.Sites["s1"]; s.PortsPolicy != "" || s.EnabledPorts != "" {
		t.Fatalf("an existing router must start with no policy: %+v", s)
	}
	if _, err := b.pool.Exec(ctx, `UPDATE sites SET ports_policy = 'off', enabled_ports = 'ether3,sfp-sfpplus1' WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}
	d, err = b.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := d.Sites["s1"]; s.PortsPolicy != "off" || s.EnabledPorts != "ether3,sfp-sfpplus1" {
		t.Fatalf("%+v", s)
	}
}
