// Package config loads ispbill settings from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime settings. Secrets must never be logged.
type Config struct {
	ListenAddr string
	DataDir    string
	BrandName  string
	TestMode   bool
	RouterURL  string
	// RouterURLSet is true when ROUTER_URL was given explicitly (container mode).
	RouterURLSet bool
	RouterUser   string
	RouterPass   string
	RouterMock   bool
	// RouterReadTimeout cuts one live read of a router (ROUTER_READ_TIMEOUT, default 8s: the tunnel can be slow).
	RouterReadTimeout time.Duration
	HotspotServer     string
	AdminUser         string
	AdminPass         string
	PublicBaseURL     string
	SupportPhone      string

	// Remote (VPS) mode. VerifyHotspotHost makes the public portal trust a
	// mac/ip pair only if the router currently has that device in
	// /ip/hotspot/host, so nobody on the internet can open a flat's page by
	// guessing its MAC. PublicIP is the VPS address the router whitelists.
	VerifyHotspotHost bool
	PublicIP          string

	MpesaEnv       string
	MpesaKey       string
	MpesaSecret    string
	MpesaShortcode string
	// MpesaC2BShortcode is the shortcode C2B URLs are registered for when it
	// differs from MpesaShortcode (the Daraja sandbox needs this). "" = same.
	MpesaC2BShortcode string
	// MpesaUnknownRef: "reject" (default) bounces a Paybill payment whose
	// account number matches no flat; "accept" takes it into Unmatched.
	MpesaUnknownRef string
	MpesaPasskey    string
	// MpesaCallbackToken is required once M-Pesa is configured (see Validate).
	// Daraja callback URLs carry ?t=<token> and callbacks without it are
	// refused (Daraja callbacks are unsigned, so this stops forged "payments").
	MpesaCallbackToken string
	// MpesaCallbackTokenPrev is the token this one replaced. It keeps working
	// until MpesaCallbackTokenPrevUntil, so payments Safaricom still sends to
	// the registered URLs are not lost. Set by the settings manager only.
	MpesaCallbackTokenPrev      string
	MpesaCallbackTokenPrevUntil time.Time

	// Multi-building (WireGuard) settings. WGPublicKey is the server's public
	// key (the installer generates the private key); routers peer with it.
	WGPublicKey           string
	WGEndpoint            string // host:port the routers dial; defaults to PUBLIC_IP:51820
	WGSubnet              string // tunnel subnet, server at .1
	WGDumpPath            string // written by the root status timer
	JoinTokenTTL          time.Duration
	AllowInsecureJoin     bool // permit minting join links when PUBLIC_BASE_URL is not https (bench only)
	JoinRequireMikrotikUA bool // only a RouterOS fetch User-Agent ("RouterOS 7.x" or "Mikrotik/6.x") consumes a join token
	SiteName              string

	// Metrics / QoE collector.
	WANInterface    string
	OLTInterface    string
	WANCapacityMbps float64
	OLTCapacityMbps float64
	FUPTB           float64
	NetflowAddr     string
	CustomerSubnet  string
	PingTargets     []string
	FlowEvery       time.Duration // hotspot sessions + queues
	PortEvery       time.Duration // interface counters
	PingEvery       time.Duration // ping + system resource
	DNSEvery        time.Duration // DNS cache refresh
	SaveEvery       time.Duration // per-site metrics file flush

	// Database. DatabaseURL empty = the JSON file in DataDir. DataKey is the
	// AES-256 key for the optional national-ID field (nil = feature off).
	DatabaseURL     string
	DataKey         []byte
	DBCommitTimeout time.Duration

	// Lifecycle (shown on the admin settings page).
	ReminderDays     int
	ReminderOnExpiry bool

	// Notifications. Secrets live in env only; thresholds and toggles are
	// edited on the admin Alerts page.
	SMTPHost, SMTPUser, SMTPPass, SMTPFrom string
	SMTPPort                               int
	AlertEmailTo                           []string
	WAToken, WAPhoneNumberID               string
	WAAdminNumbers                         []string // normalised 2547XXXXXXXX
	WAAPIVersion, WALang                   string
	WAAppSecret, WAVerifyToken             string // optional inbound webhook
	WADailyLimit                           int    // tenant messages per day (admin alerts are exempt)
	WAMinConfirmKES                        int    // smallest payment that gets a confirmation
	WAQuietFrom, WAQuietTo                 int    // tenant quiet hours, EAT: from 21 to 7 by default
	SMTPImplicit                           bool   // TLS from the first byte (port 465 always is)
}

// EmailConfigured and WhatsAppConfigured tell which
// notification channels have their secrets in the environment.
func (c Config) EmailConfigured() bool    { return c.SMTPHost != "" && c.SMTPFrom != "" }
func (c Config) WhatsAppConfigured() bool { return c.WAToken != "" && c.WAPhoneNumberID != "" }

// Load reads the configuration using getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		ListenAddr:         get("LISTEN_ADDR", ":8080"),
		DataDir:            get("DATA_DIR", "/data"),
		BrandName:          get("BRAND_NAME", "Fast Tech Net"),
		TestMode:           parseBool(getenv("TEST_MODE")),
		RouterURL:          strings.TrimRight(get("ROUTER_URL", "http://172.17.0.1"), "/"),
		RouterURLSet:       strings.TrimSpace(getenv("ROUTER_URL")) != "",
		RouterUser:         get("ROUTER_USER", "ispbill"),
		RouterPass:         getenv("ROUTER_PASS"),
		RouterMock:         parseBool(getenv("ROUTER_MOCK")),
		HotspotServer:      get("HOTSPOT_SERVER", "hotspot1"),
		AdminUser:          get("ADMIN_USER", "admin"),
		AdminPass:          getenv("ADMIN_PASS"),
		PublicBaseURL:      strings.TrimRight(get("PUBLIC_BASE_URL", ""), "/"),
		SupportPhone:       get("SUPPORT_PHONE", ""),
		VerifyHotspotHost:  get("VERIFY_HOTSPOT_HOST", "false") == "true",
		PublicIP:           get("PUBLIC_IP", ""),
		MpesaEnv:           strings.ToLower(get("MPESA_ENV", "sandbox")),
		MpesaKey:           get("MPESA_CONSUMER_KEY", ""),
		MpesaSecret:        get("MPESA_CONSUMER_SECRET", ""),
		MpesaShortcode:     get("MPESA_SHORTCODE", ""),
		MpesaPasskey:       get("MPESA_PASSKEY", ""),
		MpesaC2BShortcode:  get("MPESA_C2B_SHORTCODE", ""),
		MpesaUnknownRef:    strings.ToLower(get("MPESA_UNKNOWN_REF", "reject")),
		MpesaCallbackToken: get("MPESA_CALLBACK_TOKEN", ""),
	}
	var errs []error
	num := func(k string, def float64) float64 {
		v := get(k, "")
		if v == "" {
			return def
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive number", k))
			return def
		}
		return f
	}
	dur := func(k string, def time.Duration) time.Duration {
		v := get(k, "")
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			errs = append(errs, fmt.Errorf("%s must be a duration of at least 1s, e.g. 60s", k))
			return def
		}
		return d
	}
	c.RouterReadTimeout = dur("ROUTER_READ_TIMEOUT", 8*time.Second)
	c.WANInterface = get("WAN_INTERFACE", "ether1")
	c.OLTInterface = get("OLT_INTERFACE", "sfp-sfpplus1")
	c.WANCapacityMbps = num("WAN_CAPACITY_MBPS", 500)
	c.FUPTB = num("FUP_TB", 15)
	c.OLTCapacityMbps = num("OLT_CAPACITY_MBPS", 10000)
	c.NetflowAddr = get("NETFLOW_ADDR", ":2055")
	c.WGPublicKey = get("WG_PUBLIC_KEY", "")
	c.WGEndpoint = get("WG_ENDPOINT", "")
	if c.WGEndpoint == "" && c.PublicIP != "" {
		c.WGEndpoint = c.PublicIP + ":51820"
	}
	c.WGSubnet = get("WG_SUBNET", "10.99.0.0/22")
	// ADMIN_ALLOW_CIDR (the old wg-admin network) is no longer read: the admin
	// pages are public and protected by sign-in, so old env lines are ignored.
	c.WGDumpPath = get("WG_DUMP_PATH", "/run/ftnbilling/wg0.dump")
	c.JoinTokenTTL = dur("JOIN_TOKEN_TTL", 30*time.Minute)
	c.AllowInsecureJoin = parseBool(getenv("ALLOW_INSECURE_JOIN"))
	c.JoinRequireMikrotikUA = get("JOIN_REQUIRE_MIKROTIK_UA", "true") != "false"
	c.SiteName = get("SITE_NAME", "")
	if p, err := netip.ParsePrefix(c.WGSubnet); err != nil || !p.Addr().Is4() || p.Bits() > 29 {
		errs = append(errs, errors.New("WG_SUBNET must be an IPv4 CIDR no smaller than /29, e.g. 10.99.0.0/22"))
	}
	if c.WGPublicKey != "" {
		if b, err := base64.StdEncoding.DecodeString(c.WGPublicKey); err != nil || len(b) != 32 {
			errs = append(errs, errors.New("WG_PUBLIC_KEY must be a 44-character WireGuard public key"))
		}
	}
	c.DatabaseURL = get("DATABASE_URL", "")
	c.DBCommitTimeout = dur("DB_COMMIT_TIMEOUT", 5*time.Second)
	if k := get("DATA_KEY", ""); k != "" {
		b, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(b) != 32 {
			errs = append(errs, errors.New("DATA_KEY must be base64 of exactly 32 random bytes (generate one with: openssl rand -base64 32)"))
		} else {
			c.DataKey = b
		}
	}
	c.CustomerSubnet = get("CUSTOMER_SUBNET", "10.5.50.0/24")
	if _, _, err := net.ParseCIDR(c.CustomerSubnet); err != nil {
		errs = append(errs, fmt.Errorf("CUSTOMER_SUBNET: %v", err))
	}
	for _, t := range strings.Split(get("QOE_PING_TARGETS", "8.8.8.8,1.1.1.1"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			c.PingTargets = append(c.PingTargets, t)
		}
	}
	c.ReminderDays = 3
	if v := get("REMINDER_DAYS", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 60 {
			errs = append(errs, errors.New("REMINDER_DAYS must be a whole number from 0 to 60 (0 turns the pre-expiry reminder off)"))
		} else {
			c.ReminderDays = n
		}
	}
	c.ReminderOnExpiry = get("REMINDER_ON_EXPIRY", "true") != "false"
	c.SMTPHost, c.SMTPUser, c.SMTPPass, c.SMTPFrom = get("SMTP_HOST", ""), get("SMTP_USER", ""), getenv("SMTP_PASS"), get("SMTP_FROM", "")
	c.SMTPPort = 587
	if v := get("SMTP_PORT", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			errs = append(errs, errors.New("SMTP_PORT must be a port number (587 for STARTTLS, 465 for TLS)"))
		} else {
			c.SMTPPort = n
		}
	}
	c.AlertEmailTo = splitList(get("ALERT_EMAIL_TO", ""))
	c.WAToken, c.WAPhoneNumberID = get("WA_TOKEN", ""), get("WA_PHONE_NUMBER_ID", "")
	c.WAAPIVersion, c.WALang = get("WA_API_VERSION", "v23.0"), get("WA_LANG", "en")
	c.WAAppSecret, c.WAVerifyToken = get("WA_APP_SECRET", ""), get("WA_VERIFY_TOKEN", "")
	for _, n := range splitList(get("WA_ADMIN_NUMBERS", "")) {
		norm, err := NormalizeWANumber(n)
		if err != nil {
			errs = append(errs, fmt.Errorf("WA_ADMIN_NUMBERS: %v", err))
			continue
		}
		c.WAAdminNumbers = append(c.WAAdminNumbers, norm)
	}
	c.WADailyLimit, c.WAMinConfirmKES = 240, 250
	for k, dst := range map[string]*int{"WA_DAILY_LIMIT": &c.WADailyLimit, "WA_MIN_CONFIRM_KES": &c.WAMinConfirmKES} {
		if v := get(k, ""); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				errs = append(errs, fmt.Errorf("%s must be a whole number, 0 or more", k))
				continue
			}
			*dst = n
		}
	}
	c.WAQuietFrom, c.WAQuietTo = 21, 7
	for k, dst := range map[string]*int{"WA_QUIET_FROM": &c.WAQuietFrom, "WA_QUIET_TO": &c.WAQuietTo} {
		if v := get(k, ""); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > 23 {
				errs = append(errs, fmt.Errorf("%s must be an hour from 0 to 23", k))
				continue
			}
			*dst = n
		}
	}
	c.SMTPImplicit = parseBool(getenv("SMTP_IMPLICIT"))
	c.FlowEvery = dur("METRICS_FLOW_EVERY", 60*time.Second)
	c.PortEvery = dur("METRICS_PORT_EVERY", 30*time.Second)
	c.PingEvery = dur("METRICS_PING_EVERY", 60*time.Second)
	c.DNSEvery = dur("METRICS_DNS_EVERY", 60*time.Second)
	c.SaveEvery = dur("METRICS_SAVE_EVERY", 15*time.Minute)
	if err := c.Validate(); err != nil {
		errs = append(errs, err)
	}
	return c, errors.Join(errs...)
}

// Validate refuses unsafe or incomplete configurations.
func (c Config) Validate() error {
	var errs []error
	// ADMIN_PASS is no longer required here: the admin password may live in
	// the database (Settings > General). main calls CheckAdminPass when the
	// database holds no password.
	// ROUTER_PASS is no longer required here: buildings joined over WireGuard
	// carry their own credentials. main refuses to start only when a legacy
	// env-mode building exists and ROUTER_PASS is empty.
	if c.MpesaUnknownRef != "reject" && c.MpesaUnknownRef != "accept" {
		errs = append(errs, fmt.Errorf("MPESA_UNKNOWN_REF must be reject or accept, got %q", c.MpesaUnknownRef))
	}
	if c.MpesaEnv != "sandbox" && c.MpesaEnv != "production" {
		errs = append(errs, fmt.Errorf("MPESA_ENV must be sandbox or production, got %q", c.MpesaEnv))
	}
	if c.MpesaKey != "" && c.MpesaSecret != "" && c.MpesaShortcode != "" && c.MpesaCallbackToken == "" {
		errs = append(errs, errors.New("MPESA_CALLBACK_TOKEN must be set when M-Pesa is configured: Daraja callbacks are unsigned, "+
			"so without a secret in the callback URL anyone could post a fake payment. "+
			"Generate one (for example: openssl rand -hex 24), put it in the env file, and register the three callback URLs with ?t=<token> appended"))
	}
	return errors.Join(errs...)
}

// CheckAdminPass reports whether the ADMIN_PASS environment value may be used
// as the admin password. It is only consulted while no password is stored in
// the database.
func (c Config) CheckAdminPass() error {
	if c.AdminPass == "" || c.AdminPass == "admin" {
		return errors.New("no admin password is set: put ADMIN_PASS (not \"admin\") in the environment file, " +
			"or run `ispbill admin-reset-password -generate` to set one in the database")
	}
	return nil
}

// MpesaEnabled is true only when every Daraja setting is present.
func (c Config) MpesaEnabled() bool {
	return c.MpesaKey != "" && c.MpesaSecret != "" && c.MpesaShortcode != "" &&
		c.MpesaPasskey != "" && c.PublicBaseURL != ""
}

// MpesaBaseURL returns the Daraja API root for the selected environment.
func (c Config) MpesaBaseURL() string {
	if c.MpesaEnv == "production" {
		return "https://api.safaricom.co.ke"
	}
	return "https://sandbox.safaricom.co.ke"
}

// NormalizeWANumber turns 0712345678, +254 712 345 678 and 254712345678 into
// 254712345678, or says why the text is not an international number.
func NormalizeWANumber(n string) (string, error) {
	raw := n
	n = strings.TrimPrefix(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(n)), "+")
	if len(n) == 10 && n[0] == '0' {
		n = "254" + n[1:]
	}
	if len(n) < 11 || len(n) > 15 || strings.Trim(n, "0123456789") != "" {
		return "", fmt.Errorf("%q is not an international number like 254712345678", raw)
	}
	return n, nil
}

// SplitList splits on commas, semicolons and newlines and trims each item.
func SplitList(s string) []string { return splitList(s) }

// ParseBool reads 1, true, yes or on (any case) as true; anything else is false.
func ParseBool(s string) bool { return parseBool(s) }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
