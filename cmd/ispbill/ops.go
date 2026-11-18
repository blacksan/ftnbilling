package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"ispbill/internal/alerts"
	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/fleet"
	"ispbill/internal/metrics"
	"ispbill/internal/notify"
	"ispbill/internal/sites"
	"ispbill/internal/store"
	"ispbill/internal/web"
)

// alertInputs lets the alert engine read the fleet prober, the collectors and the store.
type alertInputs struct {
	st    *store.Store
	mgr   *sites.Manager
	fleet *fleet.Prober
}

func (a alertInputs) Sites() []store.Site {
	var out []store.Site
	a.st.View(func(d *store.Data) {
		for _, s := range d.SiteList() {
			if s.Runnable() && (s.Mode == store.SiteModeEnv || s.TunnelIP != "") {
				out = append(out, s)
			}
		}
	})
	return out
}

func (a alertInputs) Fleet(id string) fleet.SiteState { return a.fleet.State(id) }

func (a alertInputs) Live(id string) (metrics.Live, metrics.Config, bool) {
	col := a.mgr.Collector(id)
	if col == nil {
		return metrics.Live{}, metrics.Config{}, false
	}
	return col.Snapshot(), col.Config(), true
}

// startOps starts the fleet monitor, the alert engine, the outbox and the
// tenant message scanners. Each saves its file when ctx ends; wg tracks them.
func startOps(ctx context.Context, wg *sync.WaitGroup, cfg config.Config, st *store.Store, svc *billing.Service, mgr *sites.Manager, log *slog.Logger) (web.Ops, error) {
	senders := map[string]notify.Sender{}
	if cfg.EmailConfigured() {
		senders[notify.ChanEmail] = &notify.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.SMTPFrom}
	}
	if cfg.WhatsAppConfigured() {
		senders[notify.ChanWhatsApp] = &notify.WhatsApp{Token: cfg.WAToken, PhoneNumberID: cfg.WAPhoneNumberID, APIVersion: cfg.WAAPIVersion, Lang: cfg.WALang}
	}
	box, err := notify.Open(notify.Options{Path: filepath.Join(cfg.DataDir, "outbox.json"), Senders: senders, Log: log, WADailyLimit: cfg.WADailyLimit})
	if err != nil {
		return web.Ops{}, err
	}
	prober := fleet.New(fleet.Options{Store: st, Net: mgr, Path: filepath.Join(cfg.DataDir, "fleet.json"), Log: log})
	engine := alerts.New(alerts.Options{
		Path: filepath.Join(cfg.DataDir, "alerts.json"), In: alertInputs{st, mgr, prober}, Out: box, Log: log, Lang: cfg.WALang,
		Targets: func() alerts.Targets {
			return alerts.Targets{Email: emailTargets(cfg), WhatsApp: waTargets(cfg)}
		},
	})
	payHint := "M-Pesa"
	if cfg.MpesaShortcode != "" {
		payHint = "M-Pesa Paybill " + cfg.MpesaShortcode
	}
	tenant := notify.NewTenant(notify.TenantOptions{
		Store: st, Box: box, Log: log, Lang: cfg.WALang, Policy: svc.Policy, PayHint: payHint, MinConfirmKES: cfg.WAMinConfirmKES,
		Mode:      func() string { return engine.Settings().OutageNotice },
		AutoAfter: func() time.Duration { return time.Duration(engine.Settings().AutoMin) * time.Minute },
		OfflineFiredAt: func(id string) (time.Time, time.Time, bool) {
			a, ok := engine.Firing(alerts.RuleOffline, id)
			if !ok {
				return time.Time{}, time.Time{}, false
			}
			start := a.Since
			if o, has := prober.OpenOutage(id); has {
				start = o.Start
			}
			return start, a.FiredAt, true
		},
		OpenOutage: func(id string) (time.Time, bool) {
			o, ok := prober.OpenOutage(id)
			return o.Start, ok
		},
		OutageEnd: prober.OutageEnd,
	})
	svc.SetCreditHook(tenant.Kick)
	var inbound *notify.Inbound
	if cfg.WAAppSecret != "" && cfg.WAVerifyToken != "" && cfg.WhatsAppConfigured() {
		inbound = notify.NewInbound(notify.InboundOptions{Store: st, Box: box, Log: log, AppSecret: cfg.WAAppSecret, VerifyToken: cfg.WAVerifyToken,
			SupportPhone: cfg.SupportPhone, PayHint: payHint})
	}

	run := func(f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(ctx) }()
	}
	run(box.Run)
	run(prober.Run)
	run(engine.Run)
	if cfg.WhatsAppConfigured() {
		run(tenant.Run) // tenant messages need the WhatsApp channel
	}
	log.Info("monitoring started", "email", cfg.EmailConfigured(), "whatsapp", cfg.WhatsAppConfigured())
	return web.Ops{Fleet: prober, Alerts: engine, Box: box, Tenant: tenant, Inbound: inbound}, nil
}

func emailTargets(cfg config.Config) []string {
	if !cfg.EmailConfigured() {
		return nil
	}
	return cfg.AlertEmailTo
}

func waTargets(cfg config.Config) []string {
	if !cfg.WhatsAppConfigured() {
		return nil
	}
	return cfg.WAAdminNumbers
}
