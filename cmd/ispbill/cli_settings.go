package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"ispbill/internal/config"
	"ispbill/internal/settings"
)

// runSettingsCommand runs admin-reset-password and settings-check. Both open
// the database directly, so the service must be stopped first (Postgres
// allows one writer; the JSON file has no lock at all).
func runSettingsCommand(cmd string, args []string, getenv func(string) string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fromStdin := fs.Bool("password-stdin", false, "read the new password from standard input (admin-reset-password)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%s: %w\n\n%s", cmd, err, usage)
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := openStore(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return fmt.Errorf("%w (is the service still running? stop it first: systemctl stop ftnbilling)", err)
	}
	defer st.Close()
	m, err := settings.New(st, cfg.DataKey, cfg, getenv)
	if err != nil {
		return err
	}

	switch cmd {
	case "admin-reset-password":
		pw := ""
		if *fromStdin {
			line, err := bufio.NewReader(stdin).ReadString('\n')
			if err != nil && line == "" {
				return errors.New("no password on standard input")
			}
			pw = strings.TrimRight(line, "\r\n")
			if err := settings.CheckNewPassword(pw, cfg.AdminUser); err != nil {
				return err
			}
		} else if pw, err = settings.RandomPassword(); err != nil {
			return err
		}
		if err := m.SetAdminPassword(pw, "cli"); err != nil {
			return err
		}
		if *fromStdin {
			fmt.Fprintln(out, "the admin password was changed; start the service again")
		} else {
			fmt.Fprintf(out, "the admin password was reset. Log in as %q with:\n\n  %s\n\nChange it in Settings after logging in, then start the service again.\n", cfg.AdminUser, pw)
		}
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

func yesNo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
