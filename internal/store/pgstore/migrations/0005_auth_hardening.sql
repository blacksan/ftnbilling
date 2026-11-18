-- Sign-in hardening. Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

-- Wrong authenticator codes since the last good one, kept in the database so a
-- restart does not give a password-holder fresh guesses.
ALTER TABLE admin_users ADD COLUMN totp_fails integer NOT NULL DEFAULT 0,
                        ADD COLUMN totp_fail_at timestamptz;

-- The few refresh-token hashes before the previous one: presenting any of
-- them means a copied token (the device is revoked).
ALTER TABLE api_devices ADD COLUMN refresh_older jsonb NOT NULL DEFAULT '[]';
