//go:build !nopg

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ispbill/internal/config"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func schemaURL(t *testing.T) string {
	base := storetest.RequirePostgres(t)
	schema := storetest.NewSchema(t, base)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func TestForkGuardRefusesJSONAfterImport(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ispbill.json.imported-20261118"), []byte("{}"), 0o600)
	_, err := openStore(context.Background(), config.Config{DataDir: dir}, quiet())
	if err == nil || !strings.Contains(err.Error(), "moved into Postgres") {
		t.Fatalf("want the fork guard, got %v", err)
	}
	// Without such a file the JSON store opens as before.
	st, err := openStore(context.Background(), config.Config{DataDir: t.TempDir()}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if st.Kind() != "json" {
		t.Fatal(st.Kind())
	}
	st.Close()
}

func TestAutoImportOnFirstStartAndForkGuard(t *testing.T) {
	u := schemaURL(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ispbill.json")
	os.WriteFile(path, []byte(storetest.V3JSON), 0o600)
	cfg := config.Config{DataDir: dir, DatabaseURL: u}

	st, err := openStore(context.Background(), cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if st.Kind() != "postgres" {
		t.Fatal(st.Kind())
	}
	snap := st.Snapshot()
	if len(snap.Accounts) != 4 || len(snap.ClosedAccounts) != 2 || len(snap.Customers) != 2 || len(snap.Payments) != 2 || len(snap.Equipment) != 4 {
		t.Fatalf("imported data: %d accounts %d closed %d customers %d payments %d equipment",
			len(snap.Accounts), len(snap.ClosedAccounts), len(snap.Customers), len(snap.Payments), len(snap.Equipment))
	}
	st.Close()
	// The file moved aside (mode 0600), the original name is gone.
	if _, err := os.Stat(path); err == nil {
		t.Fatal("ispbill.json must be renamed after the import")
	}
	moved, _ := filepath.Glob(path + ".imported-*")
	if len(moved) != 1 {
		t.Fatalf("moved files: %v", moved)
	}
	if fi, _ := os.Stat(moved[0]); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// A second start does not import again and keeps the data.
	st, err = openStore(context.Background(), cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Snapshot().Payments) != 2 {
		t.Fatal("data lost on restart")
	}
	st.Close()
	// Starting without DATABASE_URL now refuses instead of forking the data.
	if _, err := openStore(context.Background(), config.Config{DataDir: dir}, quiet()); err == nil {
		t.Fatal("the fork guard did not fire")
	}
}

func TestAutoImportRefusesInvalidJSONAndChangesNothing(t *testing.T) {
	u := schemaURL(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ispbill.json")
	bad := strings.Replace(storetest.V3JSON, `"code":"BBB333"`, `"code":"AAA222"`, 1)
	os.WriteFile(path, []byte(bad), 0o600)
	_, err := openStore(context.Background(), config.Config{DataDir: dir, DatabaseURL: u}, quiet())
	if err == nil || !strings.Contains(err.Error(), "AAA222") {
		t.Fatalf("want a plain-words problem, got %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != bad {
		t.Fatal("the JSON file must be untouched")
	}
}

func TestCLICommands(t *testing.T) {
	u := schemaURL(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ispbill.json")
	os.WriteFile(path, []byte(storetest.V3JSON), 0o600)
	env := func(k string) string {
		return map[string]string{"DATABASE_URL": u, "DATA_DIR": dir}[k]
	}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		handled, err := runCLI(args, env, &out)
		if !handled {
			t.Fatalf("%v not handled", args)
		}
		return out.String(), err
	}
	if out, err := run("migrate"); err != nil || !strings.Contains(out, "schema version") {
		t.Fatalf("migrate: %q %v", out, err)
	}
	out, err := run("import-json")
	if err != nil || !strings.Contains(out, "format 3") || !strings.Contains(out, "applied payments") {
		t.Fatalf("import-json: %q %v", out, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("import-json should move the default file aside")
	}
	if _, err := run("import-json", "-file", filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file must fail")
	}
	out, err = run("db-check")
	if err != nil || !strings.Contains(out, "payments") || !strings.Contains(out, "KES 100") {
		t.Fatalf("db-check: %q %v", out, err)
	}
	out, err = run("db-check", "-json")
	if err != nil || !strings.Contains(out, `"applied_kes":100`) {
		t.Fatalf("db-check -json: %q %v", out, err)
	}
	out, err = run("db-check", "-against", u)
	if err != nil || !strings.Contains(out, "OK:") {
		t.Fatalf("db-check -against: %q %v", out, err)
	}
	exp := filepath.Join(dir, "export.json")
	if _, err := run("export-json", "-out", exp); err != nil {
		t.Fatal(err)
	}
	d, from, err := store.ReadJSON(exp)
	if err != nil || from != store.FormatVersion || len(d.Payments) != 2 {
		t.Fatalf("export: %v %d", err, from)
	}
	if fi, _ := os.Stat(exp); fi.Mode().Perm() != 0o600 {
		t.Fatalf("export mode %v", fi.Mode())
	}
	if _, err := run("export-json"); err == nil {
		t.Fatal("export-json without -out must fail")
	}
	// Importing again needs -force.
	if _, err := run("import-json", "-file", exp); err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("want a hint about -force, got %v", err)
	}
	if _, err := run("import-json", "-file", exp, "-force"); err != nil {
		t.Fatal(err)
	}
	if handled, _ := runCLI(nil, env, io.Discard); handled {
		t.Fatal("no arguments means run the server")
	}
	if _, err := run("frobnicate"); err == nil {
		t.Fatal("unknown command must fail")
	}
}
