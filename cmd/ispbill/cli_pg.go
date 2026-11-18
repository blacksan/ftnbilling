//go:build !nopg

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"time"

	"ispbill/internal/store"
	"ispbill/internal/store/pgstore"
)

func runDBCommand(cmd string, args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", getenv("DATABASE_URL"), "database URL")
	file := fs.String("file", "", "JSON file (import-json)")
	force := fs.Bool("force", false, "replace existing data (import-json)")
	outPath := fs.String("out", "", "output file (export-json)")
	asJSON := fs.Bool("json", false, "machine-readable output (db-check)")
	against := fs.String("against", "", "database URL to compare with (db-check)")
	check := fs.Bool("check", false, "only report the schema version, change nothing; fails when it is not the latest (migrate)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%s: %w\n\n%s", cmd, err, usage)
	}
	if *url == "" {
		return errors.New("DATABASE_URL is not set (or pass -url)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dataDir := getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
	switch cmd {
	case "migrate":
		if *check {
			b, err := pgstore.Open(ctx, *url, pgstore.Options{NoLock: true, NoMigrate: true})
			if err != nil {
				return err
			}
			defer b.Close()
			have, err := b.AppliedVersion(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "database is at schema version %d (this program: %d)\n", have, pgstore.SchemaVersion())
			if have != pgstore.SchemaVersion() {
				return fmt.Errorf("the schema is not the latest: run the installer, or: ispbill migrate")
			}
			return nil
		}
		b, err := pgstore.Open(ctx, *url, pgstore.Options{NoLock: true})
		if err != nil {
			return err
		}
		defer b.Close()
		fmt.Fprintf(out, "database is at schema version %d\n", pgstore.SchemaVersion())
		return nil

	case "import-json":
		path := *file
		defaultPath := filepath.Join(dataDir, "ispbill.json")
		if path == "" {
			path = defaultPath
		}
		d, from, err := store.ReadJSON(path)
		if err != nil {
			return err
		}
		b, err := pgstore.Open(ctx, *url, pgstore.Options{})
		if err != nil {
			return err
		}
		defer b.Close()
		counts, err := b.Import(ctx, d, pgstore.ImportOptions{From: path, Force: *force})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "imported %s (format %d)\n", path, from)
		printCounts(out, counts)
		if filepath.Clean(path) == filepath.Clean(defaultPath) {
			to, err := renameImported(path, time.Now())
			if err != nil {
				return fmt.Errorf("imported, but could not rename the file: %w", err)
			}
			fmt.Fprintf(out, "the JSON file was moved to %s so the server cannot fall back to it by mistake\n", filepath.Base(to))
		}
		return nil

	case "export-json":
		if *outPath == "" {
			return errors.New("export-json needs -out PATH")
		}
		b, err := pgstore.Open(ctx, *url, pgstore.Options{NoLock: true, NoMigrate: true})
		if err != nil {
			return err
		}
		defer b.Close()
		d, err := b.LoadAll(ctx)
		if err != nil {
			return err
		}
		if err := store.WriteJSON(*outPath, d); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s (mode 0600). It holds secrets (router passwords, encrypted national IDs): keep it private.\n", *outPath)
		return nil

	case "db-check":
		b, err := pgstore.Open(ctx, *url, pgstore.Options{NoLock: true, NoMigrate: true})
		if err != nil {
			return err
		}
		defer b.Close()
		counts, err := b.Counts(ctx)
		if err != nil {
			return err
		}
		if *against != "" {
			return compareDB(ctx, out, counts, *against)
		}
		if *asJSON {
			return json.NewEncoder(out).Encode(counts)
		}
		printCounts(out, counts)
		if counts.MACConflicts > 0 {
			d, err := b.LoadAll(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\nMAC conflicts: %d open flat(s) share a MAC with another flat (or have a malformed one). They are not provisioned until fixed:\n", counts.MACConflicts)
			for _, l := range store.Conflicts(d) {
				fmt.Fprintf(out, "  - %s\n", l)
			}
		}
		return nil
	}
	return nil
}

func printCounts(out io.Writer, c pgstore.Counts) {
	for _, t := range pgstore.Tables {
		fmt.Fprintf(out, "%-18s %d\n", t, c.Rows[t])
	}
	fmt.Fprintf(out, "%-18s %d\n", "closed accounts", c.Closed)
	fmt.Fprintf(out, "%-18s KES %d\n", "applied payments", c.AppliedKES)
}

// compareDB prints how a restored copy (counts) differs from the live
// database. The restored copy may legitimately be missing rows added since
// the dump; it must never hold more than the live database.
func compareDB(ctx context.Context, out io.Writer, restored pgstore.Counts, liveURL string) error {
	lb, err := pgstore.Open(ctx, liveURL, pgstore.Options{NoLock: true, NoMigrate: true})
	if err != nil {
		return err
	}
	defer lb.Close()
	live, err := lb.Counts(ctx)
	if err != nil {
		return err
	}
	var bad []string
	names := append([]string(nil), pgstore.Tables...)
	sort.Strings(names)
	for _, t := range pgstore.Tables {
		r, l := restored.Rows[t], live.Rows[t]
		note := ""
		switch {
		case r > l:
			bad = append(bad, fmt.Sprintf("%s: restored copy has %d rows but the live database has %d", t, r, l))
			note = "  MORE THAN LIVE"
		case r < l:
			note = fmt.Sprintf("  (%d added since the dump)", l-r)
		}
		fmt.Fprintf(out, "%-18s restored %-7d live %-7d%s\n", t, r, l, note)
	}
	note := ""
	if restored.AppliedKES > live.AppliedKES {
		bad = append(bad, fmt.Sprintf("applied payments: restored copy KES %d, live KES %d", restored.AppliedKES, live.AppliedKES))
		note = "  MORE THAN LIVE"
	}
	fmt.Fprintf(out, "%-18s restored KES %-9d live KES %-9d%s\n", "applied payments", restored.AppliedKES, live.AppliedKES, note)
	if len(bad) > 0 {
		return fmt.Errorf("the restored copy does not match: %v", bad)
	}
	fmt.Fprintln(out, "OK: the restored copy is consistent with the live database")
	return nil
}
