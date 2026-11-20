//go:build !nopg

package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"ispbill/internal/store"
	"ispbill/internal/store/pgstore"
	"ispbill/internal/store/storetest"
)

// P5: the monthly restore drill fails (and raises a "Backup problem" alert)
// whenever the app legitimately deleted a row after the dump was taken: an
// expired admin session, a spent setup code, a pruned revoked phone. compareDB
// treats "restored has more rows than live" as corruption for every table.
func TestP5DrillFalseAlarmAfterNormalDelete(t *testing.T) {
	u := schemaURL(t)
	ctx := context.Background()
	b, err := pgstore.Open(ctx, u, storetest.PGOptions(""))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenWith(b)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// a setup code exists when the 00:17 dump runs ...
	if err := st.Update(func(d *store.Data) error {
		d.Settings["admin.setup.u1"] = store.Setting{Key: "admin.setup.u1", Value: "h", UpdatedAt: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dumpCounts, err := b.Counts(ctx) // what the dump holds
	if err != nil {
		t.Fatal(err)
	}
	// ... and the owner finishes enrolment before the 03:40 drill (authn.ConfirmEnrol deletes it)
	if err := st.Update(func(d *store.Data) error { delete(d.Settings, "admin.setup.u1"); return nil }); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = compareDB(ctx, &out, dumpCounts, u)
	t.Logf("compareDB: err=%v\n%s", err, out.String())
	if err != nil {
		t.Fatalf("drill would fail on a normal delete: %v", err)
	}
}
