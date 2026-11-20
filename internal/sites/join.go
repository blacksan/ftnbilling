package sites

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
	"ispbill/internal/wg"
)

// ErrJoinInvalid is the one error for a token that is unknown, expired, used,
// or presented by something that is not a MikroTik. The web layer answers all
// of them with the same 404.
var ErrJoinInvalid = errors.New("join token is not valid")

// ErrInsecureBase is returned when a join link is requested while
// PUBLIC_BASE_URL is not https (and ALLOW_INSECURE_JOIN is off).
var ErrInsecureBase = errors.New("join links need PUBLIC_BASE_URL to start with https:// (the script carries the router's keys). For a bench test only, set ALLOW_INSECURE_JOIN=true")

// HashToken is the stored form of a join token: hex SHA-256.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// NewToken makes 32 random bytes, base64url-encoded without padding.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomAlnum returns n random letters and digits (rejection sampling, no bias).
func randomAlnum(n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, 64)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, c := range buf {
			if int(c) < 62*4 && len(out) < n { // 248 = 62*4
				out = append(out, alnum[int(c)%62])
			}
		}
	}
	return string(out), nil
}

// routerUserAgent reports whether a request comes from RouterOS /tool fetch.
// RouterOS 7 sends "User-Agent: RouterOS <version>"; RouterOS 6 sent
// "Mikrotik/6.x Fetch". The check only stops link previewers and browsers
// from burning a one-time token; it is not authentication.
func routerUserAgent(ua string) bool {
	ua = strings.ToLower(strings.TrimSpace(ua))
	return strings.HasPrefix(ua, "routeros") || strings.HasPrefix(ua, "mikrotik")
}

// ErrCannotRender wraps the reasons the bootstrap script cannot be made
// (no PUBLIC_IP, a bad layout field, no DATA_KEY for the PPPoE password...).
// The text is meant for the admin, so the web layer shows it.
var ErrCannotRender = errors.New("cannot make the set-up script")

// mintToken makes and stores a new token for a router.
func (m *Manager) mintToken(siteID string) (token string, expires time.Time, err error) {
	if !strings.HasPrefix(m.cfg.PublicBaseURL, "https://") && !m.cfg.AllowInsecureJoin {
		return "", time.Time{}, ErrInsecureBase
	}
	if !wg.ValidKey(m.cfg.WGPublicKey) || m.cfg.WGEndpoint == "" {
		return "", time.Time{}, errors.New("WireGuard is not set up on this server: WG_PUBLIC_KEY and WG_ENDPOINT are missing (re-run deploy/install.sh)")
	}
	token, err = NewToken()
	if err != nil {
		return "", time.Time{}, err
	}
	now := m.now()
	expires = now.Add(m.cfg.JoinTokenTTL)
	err = m.st.Update(func(d *store.Data) error {
		s, ok := d.Sites[siteID]
		if !ok || s.Removed {
			return billing.ErrNoSite
		}
		s.Join = store.JoinToken{Hash: HashToken(token), CreatedAt: now, ExpiresAt: expires}
		d.Sites[siteID] = s
		return nil
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// MintJoin creates a new single-use join token for a router, replacing any
// earlier one, and returns the plaintext token (shown once; only its hash is
// stored). It is refused unless PUBLIC_BASE_URL is https, or ALLOW_INSECURE_JOIN is set.
func (m *Manager) MintJoin(siteID string) (token string, expires time.Time, err error) {
	return m.mintToken(siteID)
}

// RevokeJoin discards a router's pending token.
func (m *Manager) RevokeJoin(siteID string) error {
	return m.st.Update(func(d *store.Data) error {
		s, ok := d.Sites[siteID]
		if !ok || s.Removed {
			return billing.ErrNoSite
		}
		if s.Join.Hash == "" {
			return store.ErrNoChange
		}
		s.Join.Hash, s.Join.ExpiresAt = "", time.Time{}
		d.Sites[siteID] = s
		return nil
	})
}

// ConsumeJoin turns a token into the router's bootstrap script. The token is
// consumed only when its hash matches, it has not expired or been used, and,
// if JOIN_REQUIRE_MIKROTIK_UA is on, the User-Agent starts with "Mikrotik" (so
// a chat app's link preview cannot use it up). In one store update it
// generates the router's keypair and a new REST password, stores the router's
// public key and the password, allocates the tunnel address if needed and
// marks the token used. Afterwards it rewrites the WireGuard peers and
// restarts the router's runtime. The router's private key exists only in the
// returned script. noCheck: the router fetched without certificate checking,
// so the script's report skips it too.
func (m *Manager) ConsumeJoin(token, userAgent, from string, noCheck bool) (script []byte, siteID string, err error) {
	if token == "" || len(token) > 128 {
		return nil, "", ErrJoinInvalid
	}
	if m.cfg.JoinRequireMikrotikUA && !routerUserAgent(userAgent) {
		m.noteRefusedUA(token, userAgent) // a valid token turned away for its User-Agent shows on the router page
		return nil, "", ErrJoinInvalid
	}
	script, siteID, err = m.consume(token, ROSVersionFromUA(userAgent), from, noCheck)
	if errors.Is(err, ErrCannotRender) {
		m.noteRenderFail(token, err)
	}
	return script, siteID, err
}

func (m *Manager) consume(token, rosVersion, from string, noCheck bool) (script []byte, siteID string, err error) {
	want := HashToken(token)
	now := m.now()
	err = m.st.Update(func(d *store.Data) error {
		var site store.Site
		found := false
		for _, s := range d.Sites {
			// Compare every stored hash in constant time; do not stop at the first hit.
			if s.Join.Hash != "" && subtle.ConstantTimeCompare([]byte(want), []byte(s.Join.Hash)) == 1 && !s.Removed {
				site, found = s, true
			}
		}
		if !found || !site.Join.UsedAt.IsZero() || !now.Before(site.Join.ExpiresAt) {
			return ErrJoinInvalid
		}
		priv, pub, err := wg.GenerateKeypair()
		if err != nil {
			return err
		}
		pass, err := randomAlnum(32)
		if err != nil {
			return err
		}
		site.Mode, site.WGPublicKey, site.RESTPass = store.SiteModeWG, pub, pass
		site.AccessMode = store.AccessIPoE
		site.RESTUser, site.RESTPort = nonEmpty(site.RESTUser, DefaultUser), nonZeroInt(site.RESTPort, DefaultREST)
		if site.TunnelIP == "" {
			ip, err := d.NextTunnelIP(m.pfx)
			if err != nil {
				return err
			}
			site.TunnelIP = ip
		}
		pppoe, err := m.OpenPPPoEPass(site)
		if err != nil {
			return fmt.Errorf("%w: the PPPoE password: %v", ErrCannotRender, err)
		}
		in := BootstrapInput{
			Site: ApplyDefaults(m.cfg, site), PPPoEPass: pppoe, PrivateKey: priv, RESTPass: pass, ServerPub: m.cfg.WGPublicKey,
			Endpoint: m.cfg.WGEndpoint, WGSubnet: m.pfx, MgmtSubnet: m.mgmt, PublicBase: m.cfg.PublicBaseURL, PublicIP: m.cfg.PublicIP, Generated: now, NoCheck: noCheck,
			ROSVersion: rosVersion, Token: token,
		}
		if w, ok := m.flowWant(""); ok {
			fmt.Sscanf(w.port, "%d", &in.NetflowPort)
		}
		script, err = RenderBootstrap(in)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCannotRender, err)
		}
		m.refused.clear(site.ID)
		m.renderFail.clear(site.ID)
		site.Join.UsedAt, site.Join.UsedFrom = now, from
		site.JoinedAt = time.Time{} // the next successful ping marks it joined again
		d.Sites[site.ID] = site
		delete(d.Settings, JoinReportKey(site.ID)) // the new run reports afresh
		d.AddEvent(store.Event{Time: now, SiteID: site.ID, Kind: "audit", Msg: "set-up script handed out"})
		siteID = site.ID
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if err := m.ApplyPeers(); err != nil {
		m.log.Warn("could not write the WireGuard peers file", "site", siteID, "err", err)
	}
	m.Reload(siteID)
	m.log.Info("set-up script handed out", "site", siteID, "from", from)
	return script, siteID, nil
}

// ResetStub serves the fixed reset script (the second file of the paste line)
// for ResetWindow after the setup script was fetched with the same token. It
// does not consume anything and may be fetched again within the window.
func (m *Manager) ResetStub(token, userAgent string) ([]byte, error) {
	if token == "" || len(token) > 128 {
		return nil, ErrJoinInvalid
	}
	if m.cfg.JoinRequireMikrotikUA && !routerUserAgent(userAgent) {
		return nil, ErrJoinInvalid
	}
	want := HashToken(token)
	now := m.now()
	ok := false
	m.st.View(func(d *store.Data) {
		for _, s := range d.Sites {
			if s.Join.Hash != "" && subtle.ConstantTimeCompare([]byte(want), []byte(s.Join.Hash)) == 1 && !s.Removed &&
				!s.Join.UsedAt.IsZero() && !now.After(s.Join.UsedAt.Add(ResetWindow)) {
				ok = true
			}
		}
	})
	if !ok {
		return nil, ErrJoinInvalid
	}
	return []byte(ResetStubScript), nil
}

// ErrRouterLive: the router is joined and its tunnel is up, so a new script (new
// keys) would cut it off from the server. force overrides it.
var ErrRouterLive = errors.New("this router is joined and its tunnel is up: a new script makes new keys and the router would drop off until the script is imported. Send force=true to do it anyway")

// DownloadScript is the file download for a router that cannot take the paste
// line (API only). It mints a token and consumes it on the server, and returns
// the same script for the owner to put on the router by hand (Files, then
// /import ftn-setup.rsc). Every download makes new keys: the previous script
// stops working, and so would a live router's tunnel; unless force is set it
// is refused (ErrRouterLive) for a router that is joined and whose tunnel is up.
// The script runs only on a blank router (it refuses anything already built).
func (m *Manager) DownloadScript(siteID, from string, force bool) ([]byte, error) {
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || site.Removed {
		return nil, billing.ErrNoSite
	}
	if !force && !site.JoinedAt.IsZero() {
		if up, _ := m.tunnelState(site, m.now()); up {
			return nil, ErrRouterLive
		}
	}
	token, _, err := m.mintToken(siteID)
	if err != nil {
		return nil, err
	}
	script, _, err := m.consume(token, "7.13", from, false)
	return script, err
}

func nonZeroInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// TokenState describes a building's join token for the admin page.
type TokenState string

const (
	TokenNone    TokenState = "none"
	TokenPending TokenState = "pending"
	TokenUsed    TokenState = "used"
	TokenExpired TokenState = "expired"
)

// SiteStatus is what the building page polls while the admin pastes the command.
type SiteStatus struct {
	Token        TokenState `json:"token"`
	TokenExpires time.Time  `json:"token_expires,omitempty"`
	UsedFrom     string     `json:"used_from,omitempty"`
	WGKnown      bool       `json:"wg_known"`  // the status job's dump exists
	Handshake    bool       `json:"handshake"` // the peer has completed a handshake
	HandshakeAge int64      `json:"handshake_age_s"`
	RESTOK       bool       `json:"rest_ok"`
	RESTErr      string     `json:"rest_err,omitempty"`
	Joined       bool       `json:"joined"`
	// RefusedUA / RefusedAt: a fetch with this building's valid join token was
	// turned away because its User-Agent is not a RouterOS fetch's.
	// RenderError: the router fetched its script and the server could not make it (the token is still valid).
	RenderError string    `json:"render_error,omitempty"`
	RefusedUA   string    `json:"refused_ua,omitempty"`
	RefusedAt   time.Time `json:"refused_at,omitempty"`
}

// TokenStateOf classifies a stored token.
func TokenStateOf(j store.JoinToken, now time.Time) TokenState {
	switch {
	case j.Hash == "" && j.UsedAt.IsZero():
		return TokenNone
	case !j.UsedAt.IsZero():
		return TokenUsed
	case !now.Before(j.ExpiresAt):
		return TokenExpired
	}
	return TokenPending
}
