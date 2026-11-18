package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"ispbill/internal/alerts"
	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/fleet"
	"ispbill/internal/metrics"
	"ispbill/internal/notify"
	"ispbill/internal/payments"
	"ispbill/internal/settings"
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

// mpesaConf maps the effective configuration to the Daraja client's.
func mpesaConf(c config.Config) payments.MpesaConfig {
	return payments.MpesaConfig{
		BaseURL: c.MpesaBaseURL(), Key: c.MpesaKey, Secret: c.MpesaSecret, Shortcode: c.MpesaShortcode, C2BShortcode: c.MpesaC2BShortcode,
		Passkey: c.MpesaPasskey, PublicBaseURL: c.PublicBaseURL, CallbackToken: c.MpesaCallbackToken, PrevToken: c.MpesaCallbackTokenPrev, PrevUntil: c.MpesaCallbackTokenPrevUntil, UnknownRef: c.MpesaUnknownRef,
		Enabled: c.MpesaEnabled(),
	}
}

func payHintOf(c config.Config) string {
	if c.MpesaShortcode != "" {
		return "M-Pesa Paybill " + c.MpesaShortcode
	}
	return "M-Pesa"
}

// startOps starts the fleet monitor, the alert engine, the outbox and the
// tenant message scanners. Each saves its file when ctx ends; wg tracks them.
// Everything the Settings pages can change is applied through setm's appliers,
// so a save takes effect without a restart.
func startOps(ctx context.Context, wg *sync.WaitGroup, setm *settings.Manager, mp *payments.Mpesa, st *store.Store, svc *billing.Service, mgr *sites.Manager, log *slog.Logger) (web.Ops, error) {
	cfg := setm.Effective()
	box, err := notify.Open(notify.Options{Path: filepath.Join(cfg.DataDir, "outbox.json"), Senders: map[string]notify.Sender{}, Log: log, WADailyLimit: cfg.WADailyLimit})
	if err != nil {
		return web.Ops{}, err
	}
	prober := fleet.New(fleet.Options{Store: st, Net: mgr, Path: filepath.Join(cfg.DataDir, "fleet.json"), Log: log})
	backupPath := filepath.Join(cfg.DataDir, "backup-status.json")
	var live atomic.Pointer[config.Config]
	live.Store(&cfg)
	engine := alerts.New(alerts.Options{
		Path: filepath.Join(cfg.DataDir, "alerts.json"), In: alertInputs{st, mgr, prober}, Out: box, Log: log, Lang: cfg.WALang,
		Backup: func(now time.Time) string { return alerts.BackupProblem(backupPath, now) },
		Targets: func() alerts.Targets {
			c := *live.Load()
			return alerts.Targets{Email: emailTargets(c), WhatsApp: waTargets(c)}
		},
	})
	tenant := notify.NewTenant(notify.TenantOptions{
		Store: st, Box: box, Log: log, Lang: cfg.WALang, Policy: svc.Policy, PayHint: payHintOf(cfg), MinConfirmKES: cfg.WAMinConfirmKES,
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
	// The webhook handler always exists; it answers 404 until both secrets are set.
	inbound := notify.NewInbound(notify.InboundOptions{Store: st, Box: box, Log: log, AppSecret: cfg.WAAppSecret, VerifyToken: cfg.WAVerifyToken,
		SupportPhone: cfg.SupportPhone, PayHint: payHintOf(cfg)})

	// Appliers: run after every Save or Revert of the section, and once now.
	setm.OnApply(settings.SecMpesa, func(c config.Config) {
		mp.SetConfig(mpesaConf(c))
		tenant.SetPayHint(payHintOf(c))
		inbound.SetReplyInfo(payHintOf(c), c.SupportPhone)
	})
	setm.OnApply(settings.SecWhatsApp, func(c config.Config) {
		live.Store(&c)
		if c.WhatsAppConfigured() {
			box.SetSender(notify.ChanWhatsApp, &notify.WhatsApp{Token: c.WAToken, PhoneNumberID: c.WAPhoneNumberID, APIVersion: c.WAAPIVersion, Lang: c.WALang})
		} else {
			box.RemoveSender(notify.ChanWhatsApp)
		}
		box.SetWADailyLimit(c.WADailyLimit)
		notify.SetQuietHours(c.WAQuietFrom, c.WAQuietTo)
		engine.SetLang(c.WALang)
		tenant.SetLang(c.WALang)
		tenant.SetMinConfirmKES(c.WAMinConfirmKES)
		inbound.SetSecrets(c.WAAppSecret, c.WAVerifyToken)
	})
	setm.OnApply(settings.SecEmail, func(c config.Config) {
		live.Store(&c)
		if c.EmailConfigured() {
			box.SetSender(notify.ChanEmail, &notify.SMTP{Host: c.SMTPHost, Port: c.SMTPPort, User: c.SMTPUser, Pass: c.SMTPPass, From: c.SMTPFrom, Implicit: c.SMTPImplicit})
		} else {
			box.RemoveSender(notify.ChanEmail)
		}
	})
	setm.ApplyAll()

	run := func(f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(ctx) }()
	}
	run(box.Run)
	run(prober.Run)
	run(engine.Run)
	run(tenant.Run) // sends nothing while the WhatsApp channel has no sender
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
