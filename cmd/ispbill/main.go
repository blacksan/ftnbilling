// Command ispbill is a small internet-billing server with a captive portal
// for a MikroTik hotspot.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/metrics"
	"ispbill/internal/payments"
	"ispbill/internal/settings"
	"ispbill/internal/sites"
	"ispbill/internal/store"
	"ispbill/internal/web"
)

func main() {
	if handled, err := runCLI(os.Args[1:], os.Getenv, os.Stdout); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	openCtx, openCancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	st, err := openStore(openCtx, cfg, log)
	openCancel()
	if err != nil {
		return err
	}
	defer st.Close()
	if len(cfg.DataKey) == 0 {
		return errors.New("DATA_KEY is not set: it seals the two-factor secrets. Generate one (openssl rand -base64 32) and put it in the environment file")
	}
	au, err := authn.New(st, cfg.DataKey, cfg.BrandName)
	if err != nil {
		return fmt.Errorf("DATA_KEY: %w", err)
	}
	if err := seedOwner(au, st, cfg, log); err != nil {
		return err
	}
	if cfg.RouterMock {
		log.Warn("ROUTER_MOCK=true: using an in-memory fake router")
	}
	if _, err := sites.Bootstrap(cfg, st, log); err != nil {
		return err
	}

	var svc *billing.Service
	mgr := sites.New(sites.Options{
		Cfg: cfg, Store: st, Log: log,
		Dir:      func(siteID string) metrics.Directory { return svc.Directory(siteID) },
		OnJoined: func(siteID string) { svc.MarkProfilesDirty(siteID) },
	})
	svc = billing.NewService(st, mgr, log)
	mgr.SetProvisioner(svc)
	// TODO(merge): mgr.SetJobs(mgr.Jobs()) once W9's sites.ProvisionJobs / SetJobs is on this branch.
	if err := svc.SetDataKey(cfg.DataKey); err != nil {
		return fmt.Errorf("DATA_KEY: %w", err)
	}
	svc.SetPolicy(billing.Policy{ReminderDays: cfg.ReminderDays, ReminderOnExpiry: cfg.ReminderOnExpiry})
	if err := svc.SeedPackages(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	mgr.Start(ctx)
	// Push the speed profiles to every building's router in the background, so
	// one unreachable building does not delay start-up. The sweeper retries.
	var wg sync.WaitGroup
	for _, id := range runnableSites(st) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			syncCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if err := svc.SyncProfiles(syncCtx, id); err != nil {
				log.Warn("router profile sync failed; the sweeper will retry", "site", id, "err", err)
			}
		}()
	}

	// Settings saved in the admin pages lie over the environment file.
	setm, err := settings.New(st, cfg.DataKey, cfg, os.Getenv)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	mp := payments.NewMpesa(mpesaConf(setm.Effective()), svc, st, log)
	// Callbacks the database could not take are kept on disk and replayed.
	mp.SetSpool(payments.NewSpool(filepath.Join(cfg.DataDir, "spool")))
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(60 * time.Second)
		defer tick.Stop()
		for first := true; ; first = false {
			if !first {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
			if st.Healthy() != nil {
				continue
			}
			if n, err := mp.Replay(ctx); n > 0 || err != nil {
				log.Info("replayed spooled payment callbacks", "applied", n, "err", err)
			}
		}
	}()
	providers := map[string]payments.Provider{
		"test":    &payments.Test{Enabled_: cfg.TestMode, Delay: 3 * time.Second, C: svc, Logf: log.Warn},
		"voucher": payments.Voucher{C: svc},
		"mpesa":   mp,
	}
	if cfg.TestMode {
		log.Warn("TEST_MODE=true: the test payment button grants free access")
	}
	log.Info("starting", "listen", cfg.ListenAddr, "data", cfg.DataDir, "router_mock", cfg.RouterMock,
		"test_mode", cfg.TestMode, "mpesa_enabled", setm.Effective().MpesaEnabled(), "mpesa_env", setm.Effective().MpesaEnv)

	svc.ResyncAll()
	wg.Add(1)
	go func() { defer wg.Done(); svc.RunSweeper(ctx, 60*time.Second) }()

	ops, err := startOps(ctx, &wg, setm, mp, st, svc, mgr, log)
	if err != nil {
		stop()
		wg.Wait()
		mgr.Wait()
		svc.Wait()
		return err
	}
	webSrv := web.New(cfg, st, svc, providers, mp, mgr, log)
	webSrv.SetSettings(setm)
	webSrv.SetOps(ops)
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(10 * time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				au.Prune()
			}
		}
	}()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           webSrv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		stop()
		wg.Wait()
		mgr.Wait()
		svc.Wait()
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shCtx, shCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer shCancel()
	err = srv.Shutdown(shCtx)
	wg.Wait()
	mgr.Wait()
	svc.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// seedOwner creates the first admin owner when there is none. The password
// comes from the admin.pass_hash setting (a password saved in the old Settings
// page) or else from ADMIN_PASS. After that the admin_users table is the only
// source: both are ignored.
func seedOwner(au *authn.Service, st *store.Store, cfg config.Config, log *slog.Logger) error {
	if _, ok := au.Owner(); ok {
		return nil
	}
	var hash string
	st.View(func(d *store.Data) { hash = d.Settings["admin.pass_hash"].Value })
	if hash == "" {
		if err := cfg.CheckAdminPass(); err != nil {
			return err
		}
		var err error
		if hash, err = settings.HashPassword(cfg.AdminPass); err != nil {
			return err
		}
	}
	created, err := au.SeedOwner(cfg.AdminUser, hash)
	if err != nil {
		return err
	}
	if created {
		log.Info("admin owner created; sign in and enrol the authenticator app (ispbill admin-reset-password -setup-code prints the setup code)", "user", cfg.AdminUser)
	}
	return nil
}

// runnableSites lists the buildings whose router can be reached right now.
func runnableSites(st *store.Store) []string {
	var ids []string
	st.View(func(d *store.Data) {
		for _, s := range d.SiteList() {
			if s.Runnable() {
				ids = append(ids, s.ID)
			}
		}
	})
	return ids
}
