package store

import "time"

// Admin roles (AdminUser.Role).
const (
	RoleOwner    = "owner"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// AdminUser is a person who can sign in to the admin pages and the app.
// Secrets are never stored in clear: PassHash is the PBKDF2 hash string,
// TOTPSecretEnc is sealed with the settings secretbox, and RecoveryCodes
// holds hashes of the unused recovery codes (a used code is removed).
type AdminUser struct {
	ID              string    `json:"id"`
	Username        string    `json:"username"`
	Role            string    `json:"role"` // owner | operator | viewer
	PassHash        string    `json:"pass_hash,omitempty"`
	TOTPSecretEnc   string    `json:"totp_secret_enc,omitempty"`
	TOTPConfirmedAt time.Time `json:"totp_confirmed_at,omitempty"` // zero = not enrolled
	TOTPLastStep    int64     `json:"totp_last_step,omitempty"`    // replay guard
	RecoveryCodes   []string  `json:"recovery_codes,omitempty"`
	Disabled        bool      `json:"disabled,omitempty"`
	PassChangedAt   time.Time `json:"pass_changed_at,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

// AdminSession is one browser sign-in. ID is the SHA-256 of the cookie value.
type AdminSession struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	CSRF       string    `json:"csrf"`
	MFA        bool      `json:"mfa,omitempty"` // second factor done
	IP         string    `json:"ip,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// APIDevice is one phone signed in to the API. Only SHA-256 hashes of the
// tokens are stored; RefreshPrevHash is the previous refresh token, kept for
// reuse detection and the short grace period.
type APIDevice struct {
	ID                string    `json:"id"`
	UserID            string    `json:"user_id"`
	InstallID         string    `json:"install_id,omitempty"`
	Name              string    `json:"name,omitempty"`
	Model             string    `json:"model,omitempty"`
	Platform          string    `json:"platform,omitempty"`
	AppVersion        string    `json:"app_version,omitempty"`
	AccessHash        string    `json:"access_hash,omitempty"`
	AccessExpiresAt   time.Time `json:"access_expires_at,omitempty"`
	RefreshHash       string    `json:"refresh_hash,omitempty"`
	RefreshPrevHash   string    `json:"refresh_prev_hash,omitempty"`
	RefreshRotatedAt  time.Time `json:"refresh_rotated_at,omitempty"`
	RefreshExpiresAt  time.Time `json:"refresh_expires_at,omitempty"`
	AbsoluteExpiresAt time.Time `json:"absolute_expires_at,omitempty"`
	LastIP            string    `json:"last_ip,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	LastSeenAt        time.Time `json:"last_seen_at,omitempty"`
	RevokedAt         time.Time `json:"revoked_at,omitempty"`
	RevokedReason     string    `json:"revoked_reason,omitempty"`
}
