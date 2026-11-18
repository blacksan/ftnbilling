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
	RouterURLSet  bool
	RouterUser    string
	RouterPass    string
	RouterMock    bool
	HotspotServer string
	AdminUser     string
	AdminPass     string
	PublicBaseURL string
	SupportPhone  string

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
	MpesaPasskey   string
	// MpesaCallbackToken is optional. When set, Daraja callback URLs carry
	// ?t=<token> and callbacks without it are refused (Daraja callbacks are
	// unsigned, so this stops forged "payments").
	MpesaCallbackToken string

	// Multi-building (WireGuard) settings. WGPublicKey is the server's public
	// key (the installer generates the private key); routers peer with it.
	WGPublicKey           string
	WGEndpoint            string // host:port the routers dial; defaults to PUBLIC_IP:51820
	WGSubnet              string // tunnel subnet, server at .1
	WGDumpPath            string // written by the root status timer
	JoinTokenTTL          time.Duration
	AllowInsecureJoin     bool // permit minting join links when PUBLIC_BASE_URL is not https (bench only)
	JoinRequireMikrotikUA bool // only a User-Agent starting with "Mikrotik" consumes a join token
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

	// Lifecycle (shown on the admin settings page).
	ReminderDays     int
	ReminderOnExpiry bool
}

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
	if c.AdminPass == "" || c.AdminPass == "admin" {
		errs = append(errs, errors.New("ADMIN_PASS must be set and must not be \"admin\""))
	}
	// ROUTER_PASS is no longer required here: buildings joined over WireGuard
	// carry their own credentials. main refuses to start only when a legacy
	// env-mode building exists and ROUTER_PASS is empty.
	if c.MpesaEnv != "sandbox" && c.MpesaEnv != "production" {
		errs = append(errs, fmt.Errorf("MPESA_ENV must be sandbox or production, got %q", c.MpesaEnv))
	}
	return errors.Join(errs...)
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

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
