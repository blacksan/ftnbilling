package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

// runSettingsCommand runs admin-reset-password and settings-check. With
// Postgres they work while the service runs (they touch settings rows only).
// The JSON file has no lock, so there they refuse while the service answers.
func runSettingsCommand(cmd string, args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	generate := fs.Bool("generate", false, "set a random password and print it once (admin-reset-password)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%s: %w\n\n%s", cmd, err, usage)
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var st *store.Store
	if cfg.DatabaseURL != "" {
		st, err = openPostgresShared(ctx, cfg)
	} else {
		if serviceAnswers(cfg.ListenAddr) {
			return errors.New("the service is running and the JSON database is not shared: stop it first (systemctl stop ftnbilling), then run this again")
		}
		st, err = store.Open(cfg.DataDir)
	}
	if err != nil {
		return err
	}
	defer st.Close()
	m, err := settings.New(st, cfg.DataKey, cfg, getenv)
	if err != nil {
		return err
	}
	if cfg.DatabaseURL != "" {
		// The service numbers its audit events itself; a row written here
		// would collide with its next one. Log to stderr (the journal) instead.
		m.NoteAuditTo(func(msg string) { fmt.Fprintln(os.Stderr, "audit: "+msg+" (cli)") })
	}

	switch cmd {
	case "admin-reset-password":
		if !*generate {
			// Without a usable ADMIN_PASS, removing the stored hash would leave
			// no admin password and the service would refuse to start.
			if err := cfg.CheckAdminPass(); err != nil {
				return fmt.Errorf("not removing the stored admin password: ADMIN_PASS in the environment file is empty or \"admin\", "+
					"so the service could not start without it. Use `admin-reset-password -generate` to set a new random password instead (%w)", err)
			}
			if err := m.ClearAdminPassword("cli"); err != nil {
				return err
			}
			fmt.Fprintln(out, "the stored admin password was removed: ADMIN_PASS from the environment file works again")
			restartNote(out, cfg)
			return nil
		}
		pw, err := settings.RandomPassword()
		if err != nil {
			return err
		}
		if err := m.SetAdminPassword(pw, "cli"); err != nil {
			return err
		}
		fmt.Fprintf(out, "the admin password was reset. Log in as %q with:\n\n  %s\n\nChange it in Settings after logging in.\n", cfg.AdminUser, pw)
		restartNote(out, cfg)
		return nil

	case "settings-check":
		fmt.Fprintf(out, "database: %s\nDATA_KEY: %s\nadmin password: %s\n\n", st.Kind(), yesNo(len(cfg.DataKey) > 0, "set", "NOT set (secrets cannot be saved)"),
			yesNo(m.HasAdminPassword(), "stored in the database", "from ADMIN_PASS in the environment"))
		for _, sec := range []string{settings.SecWhatsApp, settings.SecMpesa, settings.SecEmail} {
			fmt.Fprintf(out, "[%s]\n", sec)
			for _, f := range m.Fields(sec) {
				mark := ""
				if f.Secret {
					mark = " (secret, " + yesNo(f.Set, "set", "empty") + ")"
				}
				fmt.Fprintf(out, "  %-28s %s%s\n", f.Key, f.Source, mark)
			}
		}
		if p := m.Problems(); len(p) > 0 {
			fmt.Fprintln(out, "\nstored values that are ignored:")
			for _, x := range p {
				fmt.Fprintln(out, "  "+x)
			}
			return fmt.Errorf("%d stored setting(s) could not be used", len(p))
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// restartNote tells the admin the running service has not seen the change.
// With Postgres the service keeps working but still honours the old password
// until it restarts (it remembers verified logins for 12 hours); with the JSON
// file the service was stopped, and must be started again.
func restartNote(out io.Writer, cfg config.Config) {
	if cfg.DatabaseURL != "" {
		fmt.Fprintln(out, "\nRESTART REQUIRED: the running service still accepts the old password until it restarts:\n  sudo systemctl restart ftnbilling\n(deploy/ftnbilling-admin-reset does this for you.)")
		return
	}
	fmt.Fprintln(out, "\nStart the service again: sudo systemctl start ftnbilling")
}

// serviceAnswers reports whether something answers /healthz on the listen address.
func serviceAnswers(listen string) bool {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func yesNo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
