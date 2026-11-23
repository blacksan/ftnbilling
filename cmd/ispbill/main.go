// Command ispbill is a small internet-billing server for ISPs that serve
// flats over IPoE behind MikroTik routers: it runs the payment page, the admin
// and the router reconciler.
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

	"ispbill/internal/acs"
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
	var svc *billing.Service
	mgr := sites.New(sites.Options{
		Cfg: cfg, Store: st, Log: log, DataKey: cfg.DataKey,
		Dir: func(siteID string) metrics.Directory { return svc.Directory(siteID) },
	})
	svc = billing.NewService(st, mgr, log)
	mgr.SetProvisioner(svc)
	svc.SetSweepHook(mgr.SweepSite)
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
	var wg sync.WaitGroup

	// Settings saved in the admin pages lie over the environment file.
	setm, err := settings.New(st, cfg.DataKey, cfg, os.Getenv)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	mp := payments.NewMpesa(mpesaConf(setm.Effective()), svc, st, log)
	// Callbacks the database could not take are kept on disk and replayed.
	mp.SetSpool(payments.NewSpool(filepath.Join(cfg.DataDir, "spool")))
	mp.SetLegacyQueryUntil(payments.LegacyQueryDeadline(cfg.DataDir, time.Now(), log.Warn))
	defer mp.Close()
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
			if n, err := svc.ExpireOrders(); n > 0 || err != nil { // waiting checkouts end as "expired", not "failed"
				log.Info("expired unpaid portal orders", "count", n, "err", err)
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

	// Reconcile every runnable building in the background, so one unreachable
	// router does not delay start-up. The sweeper retries.
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
	errc := make(chan error, 3)
	webSrv := web.New(cfg, st, svc, providers, mp, mgr, log)
	webSrv.SetSettings(setm)
	mp.SetAlert(webSrv.PaymentAlert)
	webSrv.SetOps(ops)
	if cfg.ACSEnabled() && len(cfg.DataKey) == 0 {
		// ACS_LISTEN defaults on; an older install without DATA_KEY keeps running with ONU management off.
		log.Warn("ONU management (TR-069 ACS) is off: it needs DATA_KEY to seal Wi-Fi passwords and logins")
	} else if cfg.ACSEnabled() {
		// The built-in TR-069 ACS (ONU management). It listens on the tunnel address only.
		acsSvc, err := acs.New(acs.Options{Store: st, Cfg: cfg, Log: log, DataKey: cfg.DataKey, Lease: svc.ONUMgmtLease,
			Alert: webSrv.ONUAlert})
		if err != nil {
			// Billing keeps running; only ONU management stays off (the message says why and what to do).
			log.Error("ONU management (TR-069 ACS) is off", "err", err)
		} else {
			webSrv.SetACS(acsSvc)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := acsSvc.Run(ctx); err != nil {
					log.Error("ACS stopped", "err", err)
					errc <- err
				}
			}()
		}
	}
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
	go func() { errc <- srv.ListenAndServe() }()

	var lockErr error
	// Remote WebFig (WEBFIG_DOMAIN): its own loopback listener behind the reverse proxy.
	var wfSrv *http.Server
	if h := webSrv.WebFigHandler(); h != nil {
		wfSrv = &http.Server{
			Addr:              cfg.WebFigListen,
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
			// No read or write timeout: WebFig uploads and long requests run as long
			// as the session; the proxy ends them at the session's deadline.
		}
		log.Info("remote WebFig on", "domain", cfg.WebFigDomain, "listen", cfg.WebFigListen)
		go func() { errc <- wfSrv.ListenAndServe() }()
		wg.Add(1)
		go func() { defer wg.Done(); webSrv.RunWebFig(ctx) }()
	}

	select {
	case err := <-errc:
		stop()
		wg.Wait()
		mgr.Wait()
		svc.Wait()
		return err
	case <-st.LockLost(): // nil channel (JSON store) blocks for ever
		lockErr = st.Healthy()
		stop() // end every loop (sweeper, spool, ops, manager) so run() returns and the service manager restarts the app
		log.Error("the database writer lock was lost; exiting so that the service manager restarts the app", "err", lockErr)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shCtx, shCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer shCancel()
	if wfSrv != nil {
		_ = wfSrv.Shutdown(shCtx)
	}
	err = srv.Shutdown(shCtx)
	wg.Wait()
	mgr.Wait()
	svc.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if lockErr != nil {
		return lockErr
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
