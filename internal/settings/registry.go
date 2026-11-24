// Package settings holds the admin-editable settings: a registry of every key,
// the rules for each, and a Manager that lays database rows over the
// environment configuration and tells the running server what changed.
package settings

import (
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"

	"ispbill/internal/config"
)

// Sections are the settings pages that edit registry keys.
const (
	SecWhatsApp = "whatsapp"
	SecMpesa    = "mpesa"
	SecEmail    = "email"
	SecSupport  = "support"
)

// Kind tells the page which input to draw.
type Kind string

const (
	KText   Kind = "text"
	KNumber Kind = "number"
	KBool   Kind = "bool"
	KSelect Kind = "select"
	KArea   Kind = "area" // a list, one item per line or comma separated
)

// Def describes one setting.
type Def struct {
	Key, Env, Section string
	Label, Help       string
	Placeholder       string
	Secret            bool
	Kind              Kind
	Options           []string
	// Norm validates and normalises a submitted value ("" is always allowed
	// for non-secrets and means empty).
	Norm func(string) (string, error)
	// Get reads the value from a config; Set writes one into it.
	Get func(config.Config) string
	Set func(*config.Config, string)
}

func str(get func(*config.Config) *string) (func(config.Config) string, func(*config.Config, string)) {
	return func(c config.Config) string { return *get(&c) }, func(c *config.Config, v string) { *get(c) = v }
}

func num(get func(*config.Config) *int) (func(config.Config) string, func(*config.Config, string)) {
	return func(c config.Config) string { return strconv.Itoa(*get(&c)) },
		func(c *config.Config, v string) {
			if n, err := strconv.Atoi(v); err == nil {
				*get(c) = n
			}
		}
}

func flag(get func(*config.Config) *bool) (func(config.Config) string, func(*config.Config, string)) {
	return func(c config.Config) string { return strconv.FormatBool(*get(&c)) },
		func(c *config.Config, v string) { *get(c) = v == "true" }
}

func list(get func(*config.Config) *[]string) (func(config.Config) string, func(*config.Config, string)) {
	return func(c config.Config) string { return strings.Join(*get(&c), ",") },
		func(c *config.Config, v string) { *get(c) = config.SplitList(v) }
}

var (
	reDigits  = regexp.MustCompile(`^[0-9]+$`)
	reAPIVer  = regexp.MustCompile(`^v[0-9]{1,2}\.[0-9]{1,2}$`)
	reLang    = regexp.MustCompile(`^[a-z]{2,3}(_[A-Z]{2})?$`)
	reHost    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	reNoSpace = regexp.MustCompile(`^[^\s\x00-\x1f\x7f]+$`)
)

func trim(f func(string) (string, error)) func(string) (string, error) {
	return func(v string) (string, error) {
		v = strings.TrimSpace(v)
		if v == "" {
			return "", nil
		}
		return f(v)
	}
}

func token(min, max int, what string) func(string) (string, error) {
	return trim(func(v string) (string, error) {
		if !reNoSpace.MatchString(v) {
			return "", fmt.Errorf("%s must not contain spaces", what)
		}
		if len(v) < min || len(v) > max {
			return "", fmt.Errorf("%s must be %d to %d characters", what, min, max)
		}
		return v, nil
	})
}

func digits(min, max int, what string) func(string) (string, error) {
	return trim(func(v string) (string, error) {
		if !reDigits.MatchString(v) || len(v) < min || len(v) > max {
			return "", fmt.Errorf("%s must be %d to %d digits", what, min, max)
		}
		return v, nil
	})
}

func intRange(lo, hi int, what string) func(string) (string, error) {
	return func(v string) (string, error) {
		v = strings.TrimSpace(v)
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return "", fmt.Errorf("%s must be a whole number from %d to %d", what, lo, hi)
		}
		return strconv.Itoa(n), nil
	}
}

func oneOf(what string, opts ...string) func(string) (string, error) {
	return func(v string) (string, error) {
		v = strings.ToLower(strings.TrimSpace(v))
		for _, o := range opts {
			if v == o {
				return v, nil
			}
		}
		return "", fmt.Errorf("%s must be %s", what, strings.Join(opts, " or "))
	}
}

func boolNorm(v string) (string, error) {
	if config.ParseBool(v) {
		return "true", nil
	}
	return "false", nil
}

func waNumbers(v string) (string, error) {
	var out []string
	for _, n := range config.SplitList(v) {
		norm, err := config.NormalizeWANumber(n)
		if err != nil {
			return "", err
		}
		out = append(out, norm)
	}
	return strings.Join(out, ","), nil
}

func emails(v string) (string, error) {
	var out []string
	for _, e := range config.SplitList(v) {
		a, err := mail.ParseAddress(e)
		if err != nil || a.Address == "" || !strings.Contains(a.Address, "@") {
			return "", fmt.Errorf("%q is not an email address", e)
		}
		out = append(out, a.Address)
	}
	return strings.Join(out, ","), nil
}

func fromAddr(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if strings.ContainsAny(v, "\r\n") {
		return "", fmt.Errorf("the From address must be one line")
	}
	if _, err := mail.ParseAddress(v); err != nil {
		return "", fmt.Errorf("the From address must look like alerts@example.com or Name <alerts@example.com>")
	}
	return v, nil
}

func hostName(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if !reHost.MatchString(v) {
		return "", fmt.Errorf("the mail server must be a host name like smtp.example.com")
	}
	return v, nil
}

func regex(re *regexp.Regexp, msg string) func(string) (string, error) {
	return trim(func(v string) (string, error) {
		if !re.MatchString(v) {
			return "", fmt.Errorf("%s", msg)
		}
		return v, nil
	})
}

func plain(max int, what string) func(string) (string, error) {
	return func(v string) (string, error) {
		v = strings.TrimSpace(v)
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return "", fmt.Errorf("%s must be one line of text", what)
			}
		}
		if len(v) > max {
			return "", fmt.Errorf("%s is too long", what)
		}
		return v, nil
	}
}

// supportPhone accepts what people write on a door: digits, spaces, + and -.
func supportPhone(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	digits := 0
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == ' ' || r == '+' || r == '-':
		default:
			return "", fmt.Errorf("the support phone may hold only digits, spaces, + and -")
		}
	}
	if digits < 7 || digits > 15 || len(v) > 24 {
		return "", fmt.Errorf("the support phone must have 7 to 15 digits")
	}
	return v, nil
}

func supportEmail(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	a, err := mail.ParseAddress(v)
	if err != nil || a.Name != "" || !strings.Contains(a.Address, "@") || len(a.Address) > 120 {
		return "", fmt.Errorf("the support email must look like support@example.com")
	}
	return a.Address, nil
}

func supportWhatsApp(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	return config.NormalizeWANumber(v)
}

// Registry lists every editable setting, in display order.
var Registry = buildRegistry()

func buildRegistry() []Def {
	var r []Def
	add := func(d Def, get func(config.Config) string, set func(*config.Config, string)) {
		d.Get, d.Set = get, set
		r = append(r, d)
	}
	// ---- WhatsApp
	g, s := str(func(c *config.Config) *string { return &c.WAToken })
	add(Def{Key: "wa.token", Env: "WA_TOKEN", Section: SecWhatsApp, Label: "Access token", Secret: true, Kind: KText,
		Help: "A permanent system-user token from Meta Business (not the 24-hour test token).", Norm: token(20, 1000, "the access token")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.WAPhoneNumberID })
	add(Def{Key: "wa.phone_number_id", Env: "WA_PHONE_NUMBER_ID", Section: SecWhatsApp, Label: "Phone number ID", Kind: KText,
		Help: "The long number under WhatsApp > API setup, not the phone number itself.", Placeholder: "1234567890123456", Norm: digits(8, 24, "the phone number ID")}, g, s)
	g, s = list(func(c *config.Config) *[]string { return &c.WAAdminNumbers })
	add(Def{Key: "wa.admin_numbers", Env: "WA_ADMIN_NUMBERS", Section: SecWhatsApp, Label: "Admin numbers", Kind: KArea,
		Help: "Who gets alerts. One per line, like 0712345678 or 254712345678.", Norm: waNumbers}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.WAAPIVersion })
	add(Def{Key: "wa.api_version", Env: "WA_API_VERSION", Section: SecWhatsApp, Label: "Graph API version", Kind: KText,
		Placeholder: "v23.0", Norm: regex(reAPIVer, "the API version looks like v23.0")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.WALang })
	add(Def{Key: "wa.lang", Env: "WA_LANG", Section: SecWhatsApp, Label: "Template language", Kind: KText,
		Help: "The language code the ftn_* templates were approved in.", Placeholder: "en", Norm: regex(reLang, "the language is a code like en, sw or en_GB")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.WAAppSecret })
	add(Def{Key: "wa.app_secret", Env: "WA_APP_SECRET", Section: SecWhatsApp, Label: "App secret", Secret: true, Kind: KText,
		Help: "From the Meta app (Settings > Basic). It signs every webhook event.", Norm: token(16, 200, "the app secret")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.WAVerifyToken })
	add(Def{Key: "wa.verify_token", Env: "WA_VERIFY_TOKEN", Section: SecWhatsApp, Label: "Webhook verify token", Secret: true, Kind: KText,
		Help: "Any text you invent; paste the same text into Meta when you set the webhook.", Norm: token(8, 100, "the verify token")}, g, s)
	g, s = num(func(c *config.Config) *int { return &c.WADailyLimit })
	add(Def{Key: "wa.daily_limit", Env: "WA_DAILY_LIMIT", Section: SecWhatsApp, Label: "Tenant messages per day", Kind: KNumber,
		Help: "0 means no cap. Admin alerts are never held back by it.", Norm: intRange(0, 100000, "the daily limit")}, g, s)
	g, s = num(func(c *config.Config) *int { return &c.WAMinConfirmKES })
	add(Def{Key: "wa.min_confirm_kes", Env: "WA_MIN_CONFIRM_KES", Section: SecWhatsApp, Label: "Smallest payment that gets a confirmation (KES)", Kind: KNumber,
		Norm: intRange(0, 100000, "the minimum amount")}, g, s)
	g, s = num(func(c *config.Config) *int { return &c.WAQuietFrom })
	add(Def{Key: "wa.quiet_from", Env: "WA_QUIET_FROM", Section: SecWhatsApp, Label: "Quiet hours start (EAT hour)", Kind: KNumber,
		Help: "Plan reminders are not sent between the two hours. Equal hours turn quiet hours off.", Norm: intRange(0, 23, "the hour")}, g, s)
	g, s = num(func(c *config.Config) *int { return &c.WAQuietTo })
	add(Def{Key: "wa.quiet_to", Env: "WA_QUIET_TO", Section: SecWhatsApp, Label: "Quiet hours end (EAT hour)", Kind: KNumber,
		Norm: intRange(0, 23, "the hour")}, g, s)

	// ---- M-Pesa
	g, s = str(func(c *config.Config) *string { return &c.MpesaEnv })
	add(Def{Key: "mpesa.env", Env: "MPESA_ENV", Section: SecMpesa, Label: "Environment", Kind: KSelect, Options: []string{"sandbox", "production"},
		Help: "Sandbox is for testing with Safaricom's fake money. Switch to production only after Safaricom has approved go-live.", Norm: oneOf("the environment", "sandbox", "production")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.MpesaKey })
	add(Def{Key: "mpesa.key", Env: "MPESA_CONSUMER_KEY", Section: SecMpesa, Label: "Consumer key", Secret: true, Kind: KText, Norm: token(8, 200, "the consumer key")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.MpesaSecret })
	add(Def{Key: "mpesa.secret", Env: "MPESA_CONSUMER_SECRET", Section: SecMpesa, Label: "Consumer secret", Secret: true, Kind: KText, Norm: token(8, 200, "the consumer secret")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.MpesaShortcode })
	add(Def{Key: "mpesa.shortcode", Env: "MPESA_SHORTCODE", Section: SecMpesa, Label: "Paybill / shortcode", Kind: KText,
		Help: "Your Paybill number. The sandbox one is 174379.", Placeholder: "174379", Norm: digits(4, 10, "the shortcode")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.MpesaC2BShortcode })
	add(Def{Key: "mpesa.c2b_shortcode", Env: "MPESA_C2B_SHORTCODE", Section: SecMpesa, Label: "C2B shortcode (optional)", Kind: KText,
		Help: "Only when the C2B shortcode differs from the one above, as in the sandbox. Leave empty otherwise.", Norm: digits(4, 10, "the C2B shortcode")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.MpesaPasskey })
	add(Def{Key: "mpesa.passkey", Env: "MPESA_PASSKEY", Section: SecMpesa, Label: "Lipa na M-Pesa passkey", Secret: true, Kind: KText, Norm: token(16, 400, "the passkey")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.MpesaCallbackToken })
	add(Def{Key: "mpesa.callback_token", Env: "MPESA_CALLBACK_TOKEN", Section: SecMpesa, Label: "Callback token", Secret: true, Kind: KText,
		Help: "A secret inside the callback URLs. Generated for you; use Rotate to replace it.", Norm: token(16, 200, "the callback token")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.MpesaInitiator })
	add(Def{Key: "mpesa.initiator", Env: "MPESA_INITIATOR_NAME", Section: SecMpesa, Label: "Initiator name (receipt check)", Kind: KText,
		Help: "The Daraja API operator. With the security credential below, every payment is confirmed with Daraja before it is credited.", Norm: token(2, 100, "the initiator name")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.MpesaSecurityCredential })
	add(Def{Key: "mpesa.security_credential", Env: "MPESA_SECURITY_CREDENTIAL", Section: SecMpesa, Label: "Security credential (receipt check)", Secret: true, Kind: KText,
		Help: "The initiator's password encrypted with Safaricom's certificate, as base64 (openssl rsautl -encrypt -certin -inkey cert.cer -pkcs, then base64).", Norm: token(40, 1000, "the security credential")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.MpesaConfirmReceipts })
	add(Def{Key: "mpesa.confirm_receipts", Env: "MPESA_CONFIRM_RECEIPTS", Section: SecMpesa, Label: "Confirm receipts with Daraja", Kind: KSelect, Options: []string{"auto", "on", "off"},
		Help: "Off (the default) credits a payment as soon as Daraja calls back. Auto or on holds each payment until Daraja confirms the receipt (needs the initiator name and credential); a receipt it does not confirm goes to Unmatched payments. Test the credentials with the Test receipt confirmation button first: a wrong credential holds every payment for 15 minutes.", Norm: oneOf("the choice", "auto", "on", "off")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.MpesaUnknownRef })
	add(Def{Key: "mpesa.unknown_ref", Env: "MPESA_UNKNOWN_REF", Section: SecMpesa, Label: "Payment with an unknown account number", Kind: KSelect, Options: []string{"reject", "accept"},
		Help: "Reject bounces it at the Paybill prompt. Accept takes the money and puts it in Unmatched payments.", Norm: oneOf("the choice", "reject", "accept")}, g, s)

	// ---- Email
	g, s = str(func(c *config.Config) *string { return &c.SMTPHost })
	add(Def{Key: "smtp.host", Env: "SMTP_HOST", Section: SecEmail, Label: "Mail server", Kind: KText, Placeholder: "smtp.gmail.com", Norm: hostName}, g, s)
	g, s = num(func(c *config.Config) *int { return &c.SMTPPort })
	add(Def{Key: "smtp.port", Env: "SMTP_PORT", Section: SecEmail, Label: "Port", Kind: KNumber, Help: "587 for STARTTLS, 465 for TLS from the first byte.", Norm: intRange(1, 65535, "the port")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.SMTPUser })
	add(Def{Key: "smtp.user", Env: "SMTP_USER", Section: SecEmail, Label: "User name", Kind: KText, Norm: plain(200, "the user name")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.SMTPPass })
	add(Def{Key: "smtp.pass", Env: "SMTP_PASS", Section: SecEmail, Label: "Password", Secret: true, Kind: KText, Norm: plain(400, "the password")}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.SMTPFrom })
	add(Def{Key: "smtp.from", Env: "SMTP_FROM", Section: SecEmail, Label: "From address", Kind: KText, Placeholder: "alerts@example.com", Norm: fromAddr}, g, s)
	g, s = flag(func(c *config.Config) *bool { return &c.SMTPImplicit })
	add(Def{Key: "smtp.implicit", Env: "SMTP_IMPLICIT", Section: SecEmail, Label: "TLS from the first byte", Kind: KBool,
		Help: "Tick for port 465. Port 465 always uses it, whatever this says.", Norm: boolNorm}, g, s)
	g, s = list(func(c *config.Config) *[]string { return &c.AlertEmailTo })
	add(Def{Key: "alert.email_to", Env: "ALERT_EMAIL_TO", Section: SecEmail, Label: "Send alerts to", Kind: KArea, Help: "One address per line.", Norm: emails}, g, s)

	// ---- Support contacts (the captive pages)
	g, s = str(func(c *config.Config) *string { return &c.SupportPhone })
	add(Def{Key: "support.phone", Env: "SUPPORT_PHONE", Section: SecSupport, Label: "Support phone", Kind: KText, Placeholder: "0100133133",
		Help: "Shown as a call link on the pages a walled device sees. A router can have its own number (Routers > settings). Empty hides it.", Norm: supportPhone}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.SupportEmail })
	add(Def{Key: "support.email", Env: "SUPPORT_EMAIL", Section: SecSupport, Label: "Support email", Kind: KText, Placeholder: "support@example.com",
		Help: "Shown as a mail link. Empty hides it.", Norm: supportEmail}, g, s)
	g, s = str(func(c *config.Config) *string { return &c.SupportWhatsApp })
	add(Def{Key: "support.whatsapp", Env: "SUPPORT_WHATSAPP", Section: SecSupport, Label: "Support WhatsApp number", Kind: KText, Placeholder: "254100133133",
		Help: "International digits (0100133133 is turned into 254100133133). The \"WhatsApp us\" button opens a chat with it. Empty hides the button.", Norm: supportWhatsApp}, g, s)

	return r
}

var byKey = func() map[string]*Def {
	m := map[string]*Def{}
	for i := range Registry {
		m[Registry[i].Key] = &Registry[i]
	}
	return m
}()

// Lookup finds a definition by key.
func Lookup(key string) (*Def, bool) { d, ok := byKey[key]; return d, ok }

// SectionDefs lists a section's definitions in display order.
func SectionDefs(section string) []Def {
	var out []Def
	for _, d := range Registry {
		if d.Section == section {
			out = append(out, d)
		}
	}
	return out
}
