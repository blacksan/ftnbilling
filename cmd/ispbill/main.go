// Command ispbill is a small internet-billing server with a captive portal
// for a MikroTik hotspot.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/metrics"
	"ispbill/internal/payments"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
	"ispbill/internal/web"
)

func main() {
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
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	var rt routeros.Client
	if cfg.RouterMock {
		rt = routeros.NewMock()
		log.Warn("ROUTER_MOCK=true: using an in-memory fake router")
	} else {
		rt = routeros.NewREST(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	}
	svc := billing.NewService(st, rt, log)
	svc.SetPolicy(billing.Policy{ReminderDays: cfg.ReminderDays, ReminderOnExpiry: cfg.ReminderOnExpiry})
	if err := svc.SeedPackages(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	syncCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := svc.SyncProfiles(syncCtx); err != nil {
		log.Warn("router profile sync failed; the sweeper will retry", "err", err)
	}
	cancel()

	mp := payments.NewMpesa(payments.MpesaConfig{
		BaseURL: cfg.MpesaBaseURL(), Key: cfg.MpesaKey, Secret: cfg.MpesaSecret, Shortcode: cfg.MpesaShortcode,
		Passkey: cfg.MpesaPasskey, PublicBaseURL: cfg.PublicBaseURL, CallbackToken: cfg.MpesaCallbackToken,
		Enabled: cfg.MpesaEnabled(),
	}, svc, st, log)
	providers := map[string]payments.Provider{
		"test":    &payments.Test{Enabled_: cfg.TestMode, Delay: 3 * time.Second, C: svc, Logf: log.Warn},
		"voucher": payments.Voucher{C: svc},
		"mpesa":   mp,
	}
	if cfg.TestMode {
		log.Warn("TEST_MODE=true: the test payment button grants free access")
	}
	log.Info("starting", "listen", cfg.ListenAddr, "data", cfg.DataDir, "router_mock", cfg.RouterMock,
		"test_mode", cfg.TestMode, "mpesa_enabled", cfg.MpesaEnabled(), "mpesa_env", cfg.MpesaEnv)

	subnet, _ := netip.ParsePrefix(cfg.CustomerSubnet)
	col := metrics.New(cfg.DataDir, metrics.Config{
		WAN: cfg.WANInterface, OLT: cfg.OLTInterface, WANCapMbps: cfg.WANCapacityMbps, OLTCapMbps: cfg.OLTCapacityMbps,
		FUPTB: cfg.FUPTB, PingTargets: cfg.PingTargets, Subnet: subnet.Masked(),
		FlowEvery: cfg.FlowEvery, PortEvery: cfg.PortEvery, PingEvery: cfg.PingEvery, DNSEvery: cfg.DNSEvery, SaveEvery: cfg.SaveEvery,
		NetflowAddr: cfg.NetflowAddr, Synthetic: cfg.RouterMock,
	}, rt, svc.Directory(), log)
	if cfg.RouterMock {
		col.DisableNetflow() // the fake router sends no traffic-flow
	}

	var wg sync.WaitGroup
	svc.ResyncAll()
	wg.Add(2)
	go func() { defer wg.Done(); svc.RunSweeper(ctx, 60*time.Second) }()
	go func() { defer wg.Done(); col.Run(ctx) }()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           web.New(cfg, st, svc, providers, mp, col, log).Handler(),
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
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shCtx, shCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer shCancel()
	err = srv.Shutdown(shCtx)
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
