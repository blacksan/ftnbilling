package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/store"
	"ispbill/internal/wg"
)

// wipeConfirm is the word that must follow -confirm.
const wipeConfirm = "WIPE"

// runWipeCommand is "ispbill wipe-data": delete every business entry and keep
// the staff accounts and settings (see internal/store/wipe.go). Without
// -confirm WIPE it only prints what it would delete. Use
// deploy/ftnbilling-wipe, which takes a backup first, stops the service and
// starts it again.
func runWipeCommand(args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("wipe-data", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	confirm := fs.String("confirm", "", "type WIPE to delete the data; without it nothing is changed")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("wipe-data: %w\n\n%s", err, usage)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("wipe-data: unexpected argument %q\n\n%s", fs.Arg(0), usage)
	}
	if *confirm != "" && *confirm != wipeConfirm {
		return fmt.Errorf("wipe-data: -confirm must be exactly %s (nothing was changed)", wipeConfirm)
	}
	apply := *confirm == wipeConfirm
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	if apply && serviceAnswers(cfg.ListenAddr) {
		return errors.New("the service is running: it keeps its own copy of the data and would write it back. " +
			"Run `sudo ftnbilling-wipe`, which stops the service, wipes and starts it again")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var rep store.WipeReport
	kind := "the JSON file"
	if cfg.DatabaseURL != "" {
		kind = "Postgres"
		rep, err = wipePostgres(ctx, cfg, apply)
	} else {
		rep, err = store.WipeJSON(cfg.DataDir, apply)
	}
	if err != nil {
		return err
	}
	files := dataFiles(cfg.DataDir)
	if apply {
		if err := wipeDataFiles(cfg.DataDir, files); err != nil {
			return fmt.Errorf("the database was emptied, but the side files could not be cleaned: %w", err)
		}
	}
	printWipe(out, kind, rep, files, apply)
	return nil
}

func printWipe(out io.Writer, kind string, rep store.WipeReport, files wipeFiles, applied bool) {
	if applied {
		fmt.Fprintf(out, "wiped the business data in %s:\n", kind)
	} else {
		fmt.Fprintf(out, "would delete from %s (nothing is changed without -confirm %s):\n", kind, wipeConfirm)
	}
	for _, it := range rep.Delete {
		fmt.Fprintf(out, "  %-34s %d\n", it.Kind, it.Count)
	}
	fmt.Fprintf(out, "  %-34s %d\n", "metrics histories (files)", len(files.metrics))
	fmt.Fprintf(out, "  %-34s %d\n", "spooled M-Pesa callbacks (files)", len(files.spool))
	fmt.Fprintf(out, "  %-34s %d\n", "message queue, probe and alert state", files.state)
	fmt.Fprintf(out, "  %-34s %d\n", "old database copies (.bak, imported)", len(files.old))
	fmt.Fprintf(out, "kept: %d admin user(s), %d setting(s) (M-Pesa, branding, alerts), the schema version, DATA_KEY\n", rep.KeptAdminUser, rep.KeptSettings)
	if applied {
		fmt.Fprintln(out, "everybody must sign in again; add the routers again with the one-line bootstrap")
	}
}

// wipeFiles are the data files next to the database that hold business data.
type wipeFiles struct {
	metrics []string // DATA/metrics/*.json: usage history per router
	spool   []string // DATA/spool/*.json: M-Pesa callbacks waiting for the database
	state   int      // outbox.json, fleet.json, alerts.json that exist
	// old: DATA/ispbill.json.v<N>.bak and DATA/ispbill.json.imported-*: full copies of earlier data. This command
	// only counts them; deploy/ftnbilling-wipe moves them into the backup directory once the backup succeeded.
	old []string
}

func dataFiles(dir string) wipeFiles {
	var f wipeFiles
	f.metrics, _ = filepath.Glob(filepath.Join(dir, "metrics", "*.json"))
	f.spool, _ = filepath.Glob(filepath.Join(dir, "spool", "*.json"))
	for _, pat := range []string{"ispbill.json.v*.bak", "ispbill.json.imported-*"} {
		m, _ := filepath.Glob(filepath.Join(dir, pat))
		f.old = append(f.old, m...)
	}
	for _, n := range []string{"outbox.json", "fleet.json", "alerts.json"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			f.state++
		}
	}
	return f
}

// wipeDataFiles removes the usage history and spooled callbacks, the queued
// messages and the probe and alert state (the alert and message options stay),
// and writes an empty WireGuard peer list so ftnbilling-wg-sync drops the
// routers' peers from wg0.
func wipeDataFiles(dir string, f wipeFiles) error {
	for _, p := range append(append([]string{}, f.metrics...), f.spool...) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, "fleet.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// alerts.json also holds the alert options; the outbox keeps which channels are paused.
	if err := keepJSONKeys(filepath.Join(dir, "alerts.json"), "settings"); err != nil {
		return err
	}
	if err := keepJSONKeys(filepath.Join(dir, "outbox.json"), "paused"); err != nil {
		return err
	}
	return wg.WritePeers(filepath.Join(dir, "wg"), nil)
}

// keepJSONKeys rewrites a JSON object file with only the listed top-level keys.
// A missing file is fine; an unreadable one is removed (it would be ignored anyway).
func keepJSONKeys(path string, keep ...string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if json.Unmarshal(raw, &all) != nil {
		return os.Remove(path)
	}
	left := map[string]json.RawMessage{}
	for _, k := range keep {
		if v, ok := all[k]; ok {
			left[k] = v
		}
	}
	b, err := json.MarshalIndent(left, "", " ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(path, b)
}
