package sites

import (
	"context"
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

// MintJoin creates a new single-use join token for a building, replacing any
// earlier one, and returns the plaintext token (shown once; only its hash is
// stored). It is refused unless PUBLIC_BASE_URL is https, or ALLOW_INSECURE_JOIN is set.
func (m *Manager) MintJoin(siteID string) (token string, expires time.Time, err error) {
	return m.MintJoinStage(siteID, "")
}

// stagePrefix binds a token to its stage: a cutover command cannot be made to
// run the join (which rotates the router's keys), nor the other way round.
// routerUserAgent reports whether a request comes from RouterOS /tool fetch.
// RouterOS 7 sends "User-Agent: RouterOS <version>"; RouterOS 6 sent
// "Mikrotik/6.x Fetch". The check only stops link previewers and browsers
// from burning a one-time token; it is not authentication.
func routerUserAgent(ua string) bool {
	ua = strings.ToLower(strings.TrimSpace(ua))
	return strings.HasPrefix(ua, "routeros") || strings.HasPrefix(ua, "mikrotik")
}

func stagePrefix(stage string) string {
	switch stage {
	case StageCutover:
		return "cut."
	case StageCleanup:
		return "cln."
	}
	return ""
}

// MintJoinStage is MintJoin for a stage: the token only works for that stage.
func (m *Manager) MintJoinStage(siteID, stage string) (token string, expires time.Time, err error) {
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
	token = stagePrefix(stage) + token
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

// RevokeJoin discards a building's pending token.
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

// ConsumeJoin turns a token into the router script. The token is consumed
// only when its hash matches, it has not expired or been used, and, if
// JOIN_REQUIRE_MIKROTIK_UA is on, the User-Agent starts with "Mikrotik" (so a
// chat app's link preview cannot use it up). In one store update it generates
// the router's keypair and a new REST password, stores the router's public key
// and the password, allocates the tunnel address if needed and marks the token
// used. Afterwards it rewrites the WireGuard peers and restarts the building's
// runtime. The router's private key exists only in the returned script.
func (m *Manager) ConsumeJoin(token, userAgent, from string) (script []byte, siteID string, err error) {
	return m.ConsumeJoinOpts(token, userAgent, from, false)
}

// ConsumeJoinOpts is ConsumeJoin with noCheck: the router fetched the script
// without certificate checking, so the script's own downloads skip it too.
func (m *Manager) ConsumeJoinOpts(token, userAgent, from string, noCheck bool) (script []byte, siteID string, err error) {
	return m.ConsumeJoinStage(token, userAgent, from, noCheck, "")
}

// ConsumeJoinStage is ConsumeJoinOpts for a stage of the IPoE rollout (see
// ScriptInput.Stage). "" and "join" make the router's keys and password, like
// the old join; "cutover" and "cleanup" only render what the database holds
// (the router is already joined, so no key is touched and none is in the script).
func (m *Manager) ConsumeJoinStage(token, userAgent, from string, noCheck bool, stage string) (script []byte, siteID string, err error) {
	return m.ConsumeJoinForce(token, userAgent, from, noCheck, stage, false)
}

// ConsumeJoinForce is ConsumeJoinStage with force: the cutover is rendered
// even though some paying flats have not been seen on the router (see
// UnseenEntitled).
func (m *Manager) ConsumeJoinForce(token, userAgent, from string, noCheck bool, stage string, force bool) (script []byte, siteID string, err error) {
	switch stage {
	case "", StageJoin, StageCutover, StageCleanup:
	default:
		return nil, "", ErrJoinInvalid
	}
	if token == "" || len(token) > 128 {
		return nil, "", ErrJoinInvalid
	}
	if m.cfg.JoinRequireMikrotikUA && !routerUserAgent(userAgent) {
		m.noteRefusedUA(token, userAgent) // a valid token turned away for its User-Agent shows on the building page
		return nil, "", ErrJoinInvalid
	}
	if !strings.HasPrefix(token, stagePrefix(stage)) || (stagePrefix(stage) == "" && (strings.HasPrefix(token, "cut.") || strings.HasPrefix(token, "cln."))) {
		return nil, "", ErrJoinInvalid // the token belongs to another stage
	}
	want := HashToken(token)
	now := m.now()
	if stage == StageCutover && !force {
		// Read the router before the token is used up: a refusal leaves it valid.
		var sid string
		m.st.View(func(d *store.Data) {
			for _, s := range d.Sites {
				if s.Join.Hash != "" && subtle.ConstantTimeCompare([]byte(want), []byte(s.Join.Hash)) == 1 && !s.Removed && s.Join.UsedAt.IsZero() && now.Before(s.Join.ExpiresAt) {
					sid = s.ID
				}
			}
		})
		confirmed := false
		m.st.View(func(d *store.Data) { confirmed = sid != "" && billing.SiteMark(d, sid, billing.MarkCutoverConfirmed) })
		if confirmed {
			return nil, "", fmt.Errorf("%w: already cut over (the cutover was confirmed); changes are applied by the server now. Run it again only if you mean to (force)", ErrCannotRender)
		}
		if sid != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			unseen, uerr := m.UnseenEntitled(ctx, sid)
			unmanaged, merr := m.UnmanagedDevices(ctx, sid)
			cancel()
			if uerr != nil {
				return nil, "", fmt.Errorf("%w: could not read the router's leases to check the flats (%v); fix the connection or force the cutover", ErrCannotRender, uerr)
			}
			if merr == nil && len(unmanaged) > 0 {
				return nil, "", fmt.Errorf("%w: the router has devices the server does not know, which would be cut off: %s. Check them, or force the cutover (they are then copied as static entries)", ErrCannotRender, strings.Join(unmanaged, "; "))
			}
			if len(unseen) > 0 {
				return nil, "", fmt.Errorf("%w: these paying flats have not been seen on the router (no lease or ARP entry with their MAC), so they would lose service: %s. Fix their MAC, or force the cutover", ErrCannotRender, strings.Join(unseen, "; "))
			}
		}
	}
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
		if stage == StageCutover || stage == StageCleanup {
			stageROS.Store(site.ID, ROSVersionFromUA(userAgent)) // read by renderStage (see joinreport.go)
			defer stageROS.Delete(site.ID)
			return m.renderLaterStage(d, site, stage, now, from, noCheck, force, &script, &siteID)
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
		site.RESTUser, site.RESTPort = nonEmpty(site.RESTUser, DefaultUser), nonZeroInt(site.RESTPort, DefaultREST)
		if site.TunnelIP == "" {
			ip, err := d.NextTunnelIP(m.pfx)
			if err != nil {
				return err
			}
			site.TunnelIP = ip
		}
		m.refused.clear(site.ID)
		site.Join.UsedAt, site.Join.UsedFrom = now, from
		site.JoinedAt = time.Time{} // the next successful ping marks it joined again
		script, err = RenderJoinScript(ScriptInput{
			Site: ApplyDefaults(m.cfg, site), PrivateKey: priv, RESTPass: pass, ServerPub: m.cfg.WGPublicKey,
			Endpoint: m.cfg.WGEndpoint, WGSubnet: m.pfx, PublicBase: m.cfg.PublicBaseURL, PublicIP: m.cfg.PublicIP, Generated: now, NoCheck: noCheck,
			Stage: stage, ROSVersion: ROSVersionFromUA(userAgent), Token: token,
		})
		if err != nil {
			return fmt.Errorf("could not render the join script: %w", err)
		}
		d.Sites[site.ID] = site
		siteID = site.ID
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if stage == StageCutover || stage == StageCleanup {
		m.log.Info("ipoe script handed out", "site", siteID, "stage", stage, "from", from)
		return script, siteID, nil
	}
	if err := m.ApplyPeers(); err != nil {
		m.log.Warn("could not write the WireGuard peers file", "site", siteID, "err", err)
	}
	m.Reload(siteID)
	if m.opt.OnJoined != nil {
		m.opt.OnJoined(siteID)
	}
	m.log.Info("join script handed out", "site", siteID, "from", from)
	return script, siteID, nil
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
	RefusedUA string    `json:"refused_ua,omitempty"`
	RefusedAt time.Time `json:"refused_at,omitempty"`
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
