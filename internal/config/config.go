// Package config loads ispbill settings from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
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
	// RouterMock serves every router from one in-memory fake (development and tests).
	RouterMock bool
	// RouterReadTimeout cuts one live read of a router (ROUTER_READ_TIMEOUT, default 10s: the tunnel can be slow).
	RouterReadTimeout time.Duration
	AdminUser         string
	AdminPass         string
	PublicBaseURL     string
	// SupportPhone, SupportEmail and SupportWhatsApp are the contacts shown on every captive (walled) page.
	// An empty one is not shown. SUPPORT_* = off, none or - means empty (blank means the built-in default).
	SupportPhone    string
	SupportEmail    string
	SupportWhatsApp string // international digits, like 254100133133

	// GoogleMapsAPIKey (GOOGLE_MAPS_API_KEY) is the browser key for the Maps JavaScript API on the Network
	// page. It is not a secret (every visitor of the page sees it), so it is a plain setting: restrict it by
	// HTTP referrer to the admin domain and to "Maps JavaScript API" in Google Cloud Console. Empty = the
	// Network page shows its list view with "View on Google Maps" links.
	GoogleMapsAPIKey string

	// WebFigDomain (WEBFIG_DOMAIN) turns on remote WebFig access: the host name
	// served by a second listener (WebFigListen, loopback only) that the
	// reverse proxy sends to the app. Empty = the feature is off.
	WebFigDomain string
	WebFigListen string

	// PublicIP is the VPS address the routers reach (the walled flats' payment
	// page and the WireGuard endpoint).
	PublicIP string

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
	// Daraja callback URLs carry the token (/pay/cb/t/<token>/...) and callbacks without it are
	// refused (Daraja callbacks are unsigned, so this stops forged "payments").
	MpesaCallbackToken string
	// MpesaInitiator and MpesaSecurityCredential are the Daraja API operator
	// (initiator) name and its password encrypted with Safaricom's certificate
	// (base64). With them the server asks Daraja's Transaction Status API
	// whether a receipt is real before it credits it (MpesaConfirmReceipts).
	MpesaInitiator          string
	MpesaSecurityCredential string
	// MpesaConfirmReceipts: "off" (default: credit as soon as a callback
	// arrives), or "auto" / "on" (on when the initiator settings are present).
	// It is off until an owner has tested the initiator credentials: a wrong
	// credential would hold every real payment for 15 minutes.
	MpesaConfirmReceipts string
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
	MgmtSubnet            string // remote-WinBox subnet (the wg-mgmt interface, server at .1); routers accept WinBox from it
	WGDumpPath            string // written by the root status timer
	JoinTokenTTL          time.Duration
	AllowInsecureJoin     bool // permit minting join links when PUBLIC_BASE_URL is not https (bench only)
	JoinRequireMikrotikUA bool // only a RouterOS fetch User-Agent ("RouterOS 7.x" or "Mikrotik/6.x") consumes a join token

	// ONU management (TR-069 ACS built into the server). ACSListen is host:port ("off" = disabled); it must be a
	// private address (the WireGuard address of the server), never a public one. ACSTLSCert/Key are optional.
	ACSListen         string
	ACSTLSCert        string
	ACSTLSKey         string
	ACSInformInterval int   // seconds, PeriodicInformInterval set on the devices
	ACSCRPorts        []int // tcp ports the server may connect to on an ONU (connection requests)
	ACSMaxPerRouter   int   // most ONUs the ACS keeps per router
	// ACSCompatAuth lets CPEs that cannot do HTTP digest with qop=auth log in (RFC 2069 digest, HTTP Basic). Off by
	// default: those logins have no replay protection or send the password in the clear on the management VLAN.
	ACSCompatAuth bool
	// ACSResetCreds makes a new ACS login when the stored one cannot be opened (DATA_KEY lost). Off: ONU management
	// refuses to start instead of silently cutting every ONU off.
	ACSResetCreds bool
	// ACSRegisteredOnly (ACS_REGISTERED_ONLY, default on): the ACS takes a first check-in only from a serial that is in the
	// ONU inventory, and holds the first house secrets until the management MAC is near the registered WAN MAC.
	ACSRegisteredOnly bool
	// ACSAllowPrivate lets ACS_LISTEN be a private address that is neither the server's WireGuard address nor
	// loopback (ACS_LISTEN_ALLOW_PRIVATE=true). Off by default: a private NIC that is 1:1 NATed to a public address
	// would expose the ACS.
	ACSAllowPrivate bool

	// MACVendorLookup is "online" (default) or "off": whether an OUI that the bundled table does not know is looked
	// up at api.macvendors.com in the background. Only the first three bytes of a MAC ever leave the server.
	// MACVendorDaily is the most online requests a day (MAC_VENDOR_DAILY, default 500).
	MACVendorLookup string
	MACVendorDaily  int

	// Metrics / QoE collector.
	WANInterface    string
	OLTInterface    string
	WANCapacityMbps float64
	OLTCapacityMbps float64
	FUPTB           float64
	NetflowAddr     string
	CustomerSubnet  string
	PingTargets     []string
	FlowEvery       time.Duration // per-flat queue counters
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

// The contacts shown on the captive pages when nothing else is set.
const (
	DefaultSupportPhone    = "0100133133"
	DefaultSupportEmail    = "support@ftw.co.ke"
	DefaultSupportWhatsApp = "254100133133"
)

// supportValue lets the environment say "no such contact" with off, none or -.
func supportValue(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "none", "-":
		return ""
	}
	return strings.TrimSpace(v)
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
		RouterMock:         parseBool(getenv("ROUTER_MOCK")),
		AdminUser:          get("ADMIN_USER", "admin"),
		AdminPass:          getenv("ADMIN_PASS"),
		PublicBaseURL:      strings.TrimRight(get("PUBLIC_BASE_URL", ""), "/"),
		SupportPhone:       supportValue(get("SUPPORT_PHONE", DefaultSupportPhone)),
		SupportEmail:       supportValue(get("SUPPORT_EMAIL", DefaultSupportEmail)),
		SupportWhatsApp:    supportValue(get("SUPPORT_WHATSAPP", DefaultSupportWhatsApp)),
		PublicIP:           get("PUBLIC_IP", ""),
		GoogleMapsAPIKey:   strings.TrimSpace(get("GOOGLE_MAPS_API_KEY", "")),
		MpesaEnv:           strings.ToLower(get("MPESA_ENV", "sandbox")),
		MpesaKey:           get("MPESA_CONSUMER_KEY", ""),
		MpesaSecret:        get("MPESA_CONSUMER_SECRET", ""),
		MpesaShortcode:     get("MPESA_SHORTCODE", ""),
		MpesaPasskey:       get("MPESA_PASSKEY", ""),
		MpesaC2BShortcode:  get("MPESA_C2B_SHORTCODE", ""),
		MpesaUnknownRef:    strings.ToLower(get("MPESA_UNKNOWN_REF", "reject")),
		MpesaCallbackToken: get("MPESA_CALLBACK_TOKEN", ""),

		MpesaInitiator:          get("MPESA_INITIATOR_NAME", ""),
		MpesaSecurityCredential: get("MPESA_SECURITY_CREDENTIAL", ""),
		MpesaConfirmReceipts:    strings.ToLower(get("MPESA_CONFIRM_RECEIPTS", "off")),
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
	c.RouterReadTimeout = dur("ROUTER_READ_TIMEOUT", 10*time.Second)
	c.WANInterface = get("WAN_INTERFACE", "ether1")
	c.OLTInterface = get("OLT_INTERFACE", "sfp-sfpplus1")
	c.WANCapacityMbps = num("WAN_CAPACITY_MBPS", 500)
	c.FUPTB = num("FUP_TB", 15)
	c.OLTCapacityMbps = num("OLT_CAPACITY_MBPS", 10000)
	c.NetflowAddr = get("NETFLOW_ADDR", ":2055")
	c.WebFigDomain = strings.ToLower(get("WEBFIG_DOMAIN", ""))
	c.WebFigListen = get("WEBFIG_LISTEN", "127.0.0.1:8082")
	if c.WebFigDomain != "" {
		if err := checkWebFigDomain(c.WebFigDomain, c.PublicBaseURL); err != nil {
			errs = append(errs, err)
		}
		if h, _, err := net.SplitHostPort(c.WebFigListen); err != nil {
			errs = append(errs, errors.New("WEBFIG_LISTEN must be host:port, e.g. 127.0.0.1:8082"))
		} else if ip, err := netip.ParseAddr(h); err != nil || !ip.IsLoopback() {
			errs = append(errs, errors.New("WEBFIG_LISTEN must be a loopback address (127.0.0.1:8082): only the reverse proxy may reach it"))
		}
	}
	c.WGPublicKey = get("WG_PUBLIC_KEY", "")
	c.WGEndpoint = get("WG_ENDPOINT", "")
	if c.WGEndpoint == "" && c.PublicIP != "" {
		c.WGEndpoint = c.PublicIP + ":51820"
	}
	c.WGSubnet = get("WG_SUBNET", "10.99.0.0/22")
	c.MgmtSubnet = get("MGMT_SUBNET", "10.98.0.0/24")
	// ADMIN_ALLOW_CIDR (the old wg-admin network) is no longer read: the admin
	// pages are public and protected by sign-in, so old env lines are ignored.
	c.WGDumpPath = get("WG_DUMP_PATH", "/run/ftnbilling/wg0.dump")
	c.loadACS(get, &errs)
	c.loadMACVendor(get, &errs)
	c.JoinTokenTTL = dur("JOIN_TOKEN_TTL", 30*time.Minute)
	c.AllowInsecureJoin = parseBool(getenv("ALLOW_INSECURE_JOIN"))
	c.JoinRequireMikrotikUA = get("JOIN_REQUIRE_MIKROTIK_UA", "true") != "false"
	if p, err := netip.ParsePrefix(c.WGSubnet); err != nil || !p.Addr().Is4() || p.Bits() > 29 {
		errs = append(errs, errors.New("WG_SUBNET must be an IPv4 CIDR no smaller than /29, e.g. 10.99.0.0/22"))
	}
	if mp, err := netip.ParsePrefix(c.MgmtSubnet); err != nil || !mp.Addr().Is4() || !mp.Addr().IsPrivate() || mp.Bits() < 16 || mp.Bits() > 29 {
		errs = append(errs, errors.New("MGMT_SUBNET must be a private IPv4 prefix from /16 to /29 (10/8, 172.16/12 or 192.168/16), e.g. 10.98.0.0/24"))
	} else if wp, err := netip.ParsePrefix(c.WGSubnet); err == nil && wp.Overlaps(mp) {
		errs = append(errs, errors.New("MGMT_SUBNET must not overlap WG_SUBNET: they are separate WireGuard networks"))
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
	if c.MpesaUnknownRef != "reject" && c.MpesaUnknownRef != "accept" {
		errs = append(errs, fmt.Errorf("MPESA_UNKNOWN_REF must be reject or accept, got %q", c.MpesaUnknownRef))
	}
	switch c.MpesaConfirmReceipts {
	case "auto", "on", "off":
	default:
		errs = append(errs, fmt.Errorf("MPESA_CONFIRM_RECEIPTS must be auto, on or off, got %q", c.MpesaConfirmReceipts))
	}
	if c.MpesaEnv != "sandbox" && c.MpesaEnv != "production" {
		errs = append(errs, fmt.Errorf("MPESA_ENV must be sandbox or production, got %q", c.MpesaEnv))
	}
	if c.MpesaKey != "" && c.MpesaSecret != "" && c.MpesaShortcode != "" && c.MpesaCallbackToken == "" {
		errs = append(errs, errors.New("MPESA_CALLBACK_TOKEN must be set when M-Pesa is configured: Daraja callbacks are unsigned, "+
			"so without a secret in the callback URL anyone could post a fake payment. "+
			"Generate one (for example: openssl rand -hex 24), put it in the env file, and register the three callback URLs as <PUBLIC_BASE_URL>/pay/cb/t/<token>/stk, /c2b/validate and /c2b/confirm"))
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

// MpesaConfirm tells whether M-Pesa receipts are confirmed with Daraja's
// Transaction Status API before they are credited, and why not when they are
// not. It is off by default. MPESA_CONFIRM_RECEIPTS=auto (or on) turns it on
// exactly when the initiator name and security credential are set; without
// them it cannot work, so it is reported as off.
func (c Config) MpesaConfirm() (on bool, why string) {
	mode := c.MpesaConfirmReceipts
	if mode == "" {
		mode = "off"
	}
	switch {
	case mode == "off":
		return false, "MPESA_CONFIRM_RECEIPTS=off (the default): payments are credited as soon as a callback arrives, so a leaked callback token can credit a fake payment. To turn it on: set MPESA_INITIATOR_NAME and MPESA_SECURITY_CREDENTIAL, press \"Test receipt confirmation\" in Settings > M-Pesa with a real receipt until Daraja confirms it, then set MPESA_CONFIRM_RECEIPTS=auto"
	case c.MpesaInitiator == "" || c.MpesaSecurityCredential == "":
		return false, "the Daraja initiator name (MPESA_INITIATOR_NAME) and security credential (MPESA_SECURITY_CREDENTIAL) are not set, so receipts cannot be confirmed with Daraja; callbacks are credited unchecked"
	}
	return true, ""
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

// checkWebFigDomain: a plain DNS name (not an address), different from the
// admin's own host, so the two never share cookies.
func checkWebFigDomain(d, base string) error {
	if len(d) > 253 || !strings.Contains(d, ".") {
		return errors.New("WEBFIG_DOMAIN must be a full host name such as tech.example.co.ke")
	}
	if _, err := netip.ParseAddr(d); err == nil {
		return errors.New("WEBFIG_DOMAIN must be a host name, not an IP address")
	}
	for _, lab := range strings.Split(d, ".") {
		if lab == "" || len(lab) > 63 || lab[0] == '-' || lab[len(lab)-1] == '-' {
			return errors.New("WEBFIG_DOMAIN is not a valid host name")
		}
		for _, c := range lab {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return errors.New("WEBFIG_DOMAIN is not a valid host name (letters, digits, dots and hyphens only)")
			}
		}
	}
	if base != "" {
		if u, err := url.Parse(base); err == nil && strings.EqualFold(u.Hostname(), d) {
			return errors.New("WEBFIG_DOMAIN must differ from the admin's host name (PUBLIC_BASE_URL): WebFig needs its own origin")
		}
	}
	return nil
}

// DefaultACSPort is the tcp port of the ACS and the usual TR-069 connection-request port.
const DefaultACSPort = 7547

// loadACS reads the ACS_* settings. The default listen address is the server's tunnel address (the first
// host of WG_SUBNET) on tcp/7547.
func (c *Config) loadACS(get func(k, def string) string, errs *[]error) {
	def := ""
	if p, err := netip.ParsePrefix(c.WGSubnet); err == nil && p.Addr().Is4() {
		def = net.JoinHostPort(p.Masked().Addr().Next().String(), strconv.Itoa(DefaultACSPort))
	}
	c.ACSListen = get("ACS_LISTEN", def)
	c.ACSTLSCert, c.ACSTLSKey = get("ACS_TLS_CERT", ""), get("ACS_TLS_KEY", "")
	if (c.ACSTLSCert == "") != (c.ACSTLSKey == "") {
		*errs = append(*errs, errors.New("ACS_TLS_CERT and ACS_TLS_KEY go together: set both or neither"))
	}
	c.ACSInformInterval = 300
	if v := get("ACS_INFORM_INTERVAL", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 60 || n > 86400 {
			*errs = append(*errs, errors.New("ACS_INFORM_INTERVAL must be a number of seconds from 60 to 86400"))
		} else {
			c.ACSInformInterval = n
		}
	}
	c.ACSMaxPerRouter = 200
	if v := get("ACS_MAX_PER_ROUTER", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5000 {
			*errs = append(*errs, errors.New("ACS_MAX_PER_ROUTER must be a number from 1 to 5000"))
		} else {
			c.ACSMaxPerRouter = n
		}
	}
	c.ACSCompatAuth = parseBool(get("ACS_COMPAT_AUTH", ""))
	c.ACSAllowPrivate = parseBool(get("ACS_LISTEN_ALLOW_PRIVATE", ""))
	c.ACSResetCreds = parseBool(get("ACS_RESET_CREDENTIALS", ""))
	c.ACSRegisteredOnly = true
	if v := get("ACS_REGISTERED_ONLY", ""); v != "" {
		c.ACSRegisteredOnly = parseBool(v)
	}
	c.ACSCRPorts = []int{7547, 30005}
	if v := get("ACS_CR_PORTS", ""); v != "" {
		var ports []int
		for _, f := range SplitList(v) {
			n, err := strconv.Atoi(f)
			if err != nil || n < 1 || n > 65535 {
				*errs = append(*errs, fmt.Errorf("ACS_CR_PORTS: %q is not a tcp port", f))
				return
			}
			ports = append(ports, n)
		}
		if len(ports) > 0 {
			c.ACSCRPorts = ports
		}
	}
	if c.ACSListen != "" && !strings.EqualFold(c.ACSListen, "off") {
		if err := c.CheckACSListenAddr(); err != nil {
			*errs = append(*errs, err)
		}
	}
}

// ACSEnabled says whether the ACS listener is on.
func (c Config) ACSEnabled() bool {
	return c.ACSListen != "" && !strings.EqualFold(c.ACSListen, "off")
}

// CheckACSListenAddr is CheckACSListen plus the rule that the address is the server's WireGuard address (the first
// host of WG_SUBNET) or loopback, unless ACS_LISTEN_ALLOW_PRIVATE is set.
func (c Config) CheckACSListenAddr() error {
	if err := CheckACSListen(c.ACSListen); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(c.ACSListen)
	ip, _ := netip.ParseAddr(host)
	if ip.IsLoopback() || c.ACSAllowPrivate {
		return nil
	}
	sub := c.WGSubnet
	if sub == "" {
		sub = "10.99.0.0/22"
	}
	if p, err := netip.ParsePrefix(sub); err == nil && p.Addr().Is4() && p.Masked().Addr().Next() == ip {
		return nil
	}
	return fmt.Errorf("ACS_LISTEN %s is not the server's WireGuard address nor loopback: another private address can be reachable from outside (a private NIC with a public 1:1 NAT). Use the WireGuard address, or set ACS_LISTEN_ALLOW_PRIVATE=true if you know this address is only reachable from the router tunnels", ip)
}

// CheckACSListen refuses an ACS address that is not private: the ACS must never be reachable from the internet.
// A wildcard address (":7547", "0.0.0.0") is refused too, since it would answer on every interface.
func CheckACSListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("ACS_LISTEN must be host:port, e.g. 10.99.0.1:7547")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("ACS_LISTEN has a bad port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Is4() {
		return errors.New("ACS_LISTEN must name one private IPv4 address (the server's WireGuard address), not a host name or a wildcard")
	}
	if ip.IsUnspecified() || !(ip.IsPrivate() || ip.IsLoopback()) {
		return fmt.Errorf("ACS_LISTEN %s is not a private address: the ACS must never be reachable from the internet (use the WireGuard address, e.g. 10.99.0.1:7547)", ip)
	}
	return nil
}

// loadMACVendor reads MAC_VENDOR_LOOKUP (online|off) and MAC_VENDOR_DAILY (1..100000).
func (c *Config) loadMACVendor(get func(k, def string) string, errs *[]error) {
	c.MACVendorLookup, c.MACVendorDaily = "online", 500
	switch v := strings.ToLower(get("MAC_VENDOR_LOOKUP", "online")); v {
	case "online", "off":
		c.MACVendorLookup = v
	default:
		*errs = append(*errs, errors.New("MAC_VENDOR_LOOKUP must be online or off"))
	}
	if v := get("MAC_VENDOR_DAILY", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100000 {
			*errs = append(*errs, errors.New("MAC_VENDOR_DAILY must be a number from 1 to 100000"))
		} else {
			c.MACVendorDaily = n
		}
	}
}
