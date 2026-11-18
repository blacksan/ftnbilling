package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"ispbill/internal/authn"
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
	setupCode := fs.Bool("setup-code", false, "print a one-time code to enrol the authenticator app (admin-reset-password)")
	resetTOTP := fs.Bool("reset-totp", false, "remove the authenticator app and recovery codes (admin-reset-password)")
	revoke := fs.Bool("revoke-sessions", false, "sign every browser and phone out (admin-reset-password)")
	userName := fs.String("user", "", "the admin user (default: the owner)")
	asJSON := fs.Bool("json", false, "machine-readable output (admin-status)")
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

	au, err := authn.New(st, cfg.DataKey, "Fast Tech Net")
	if err != nil {
		return err
	}
	if cfg.DatabaseURL != "" {
		au.SetAudit(func(_ *store.Data, e store.Event) { fmt.Fprintln(os.Stderr, "audit: "+e.Msg+" (cli)") })
	}

	switch cmd {
	case "admin-status":
		owner, ok := au.Owner()
		res := struct {
			Owner        string `json:"owner"`
			TOTPEnrolled bool   `json:"totp_enrolled"`
		}{}
		if ok {
			res.Owner, res.TOTPEnrolled = owner.Username, authn.Enrolled(owner)
		}
		if *asJSON {
			return json.NewEncoder(out).Encode(res)
		}
		if !ok {
			fmt.Fprintln(out, "no admin owner exists yet (it is created when the service first starts, from ADMIN_PASS)")
			return nil
		}
		fmt.Fprintf(out, "owner: %s\nauthenticator app enrolled: %s\n", res.Owner, yesNo(res.TOTPEnrolled, "yes", "no"))
		return nil

	case "admin-reset-password":
		return adminReset(au, cfg, out, resetOpts{user: *userName, generate: *generate, setupCode: *setupCode,
			resetTOTP: *resetTOTP, revoke: *revoke})

	case "settings-check":
		owner, hasOwner := au.Owner()
		fmt.Fprintf(out, "database: %s\nDATA_KEY: %s\nadmin owner: %s\nauthenticator app: %s\n\n", st.Kind(), yesNo(len(cfg.DataKey) > 0, "set", "NOT set (secrets cannot be saved)"),
			yesNo(hasOwner, owner.Username, "not created yet (made from ADMIN_PASS at first start)"), yesNo(hasOwner && authn.Enrolled(owner), "enrolled", "not enrolled"))
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

type resetOpts struct {
	user                                   string
	generate, setupCode, resetTOTP, revoke bool
}

// adminReset changes the admin owner's sign-in. With no flag it sets the
// password back to ADMIN_PASS; -generate sets a random one. The other flags
// leave the password alone. Every change signs all browsers and phones out
// (the running service has to be restarted to see it, see restartNote).
func adminReset(au *authn.Service, cfg config.Config, out io.Writer, o resetOpts) error {
	only := o.setupCode || o.resetTOTP || o.revoke
	setPass := o.generate || !only
	var u store.AdminUser
	var ok bool
	if o.user != "" {
		u, ok = au.UserByName(o.user)
		if !ok {
			return fmt.Errorf("there is no admin user %q", o.user)
		}
	} else {
		u, ok = au.Owner()
	}
	if setPass {
		pw := cfg.AdminPass
		if o.generate {
			var err error
			if pw, err = settings.RandomPassword(); err != nil {
				return err
			}
		} else if err := cfg.CheckAdminPass(); err != nil {
			return fmt.Errorf("not resetting the admin password: ADMIN_PASS in the environment file is empty or \"admin\". "+
				"Use `admin-reset-password -generate` to set a new random password instead (%w)", err)
		}
		hash, err := settings.HashPassword(pw)
		if err != nil {
			return err
		}
		if !ok {
			if _, err := au.SeedOwner(cfg.AdminUser, hash); err != nil {
				return err
			}
			if u, ok = au.Owner(); !ok {
				return errors.New("could not create the admin owner")
			}
		} else if err := au.SetPassword(u.ID, hash, "cli", "", ""); err != nil {
			return err
		}
		if o.generate {
			fmt.Fprintf(out, "the admin password was reset. Log in as %q with:\n\n  %s\n\nChange it under Security after logging in.\n", u.Username, pw)
		} else {
			fmt.Fprintf(out, "the password of %q was reset to ADMIN_PASS from the environment file\n", u.Username)
		}
	}
	if !ok {
		return errors.New("no admin owner exists yet: run `admin-reset-password -generate` to create one")
	}
	if o.resetTOTP {
		if err := au.ResetTOTP(u.ID, "cli", "", ""); err != nil {
			return err
		}
		fmt.Fprintf(out, "the authenticator app and recovery codes of %q were removed; they enrol again at their next sign-in\n", u.Username)
		u, _ = au.UserByID(u.ID)
	}
	if o.revoke && !o.resetTOTP && !setPass {
		n, err := au.RevokeSessions(u.ID, "", "command line", "cli", "")
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "signed out %d browser session(s); phones must sign in again too\n", n)
	}
	if o.setupCode {
		u, _ = au.UserByID(u.ID)
		switch code, err := au.IssueSetupCode(u.ID, "cli", ""); {
		case errors.Is(err, authn.ErrAlreadyEnrolled):
			fmt.Fprintf(out, "%q already has an authenticator app, so no setup code was issued (use -reset-totp if the phone is lost)\n", u.Username)
		case err != nil:
			return err
		default:
			fmt.Fprintf(out, "\nsetup code (valid 14 days, works once, for the enrol page after the password):\n\n  %s\n", code)
		}
	}
	restartNote(out, cfg)
	return nil
}
