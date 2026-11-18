-- IPoE and sign-in. Forward-only; never edit once released.
--
-- One MAC address can never be used by more than one open account, in any
-- building. Existing duplicates are flagged (mac_conflict), never dropped or
-- blanked: the keeper stays clean, the others are excluded from the unique
-- index and from provisioning until somebody fixes them.

ALTER TABLE accounts ADD COLUMN mac_conflict boolean NOT NULL DEFAULT false,
                     ADD COLUMN ip text NOT NULL DEFAULT '';

-- The old per-building index goes first: rewriting MAC spellings below can make
-- two rows equal. The global index is created once the conflicts are flagged.
DROP INDEX accounts_site_mac_open;

-- Write MACs as AA:BB:CC:DD:EE:FF. Only text that is exactly 12 hex digits
-- (with : - . or space separators) is rewritten; anything else is left as it is
-- and flagged below.
UPDATE accounts SET mac = regexp_replace(upper(regexp_replace(mac,'[-:. ]','','g')),
       '^(..)(..)(..)(..)(..)(..)$','\1:\2:\3:\4:\5:\6')
 WHERE mac <> '' AND regexp_replace(mac,'[-:. ]','','g') ~* '^[0-9a-f]{12}$';
UPDATE accounts SET mac_conflict = true
 WHERE mac <> '' AND mac !~ '^([0-9A-F]{2}:){5}[0-9A-F]{2}$';
-- Keeper among open accounts that share a MAC: a live paid plan first, then the
-- oldest, then the lowest id.
WITH r AS (
  SELECT id, row_number() OVER (PARTITION BY mac ORDER BY
           (expires_at IS NOT NULL AND expires_at > now() AND NOT suspended) DESC, created_at, id) AS rn
  FROM accounts WHERE closed_at IS NULL AND mac <> '' AND NOT mac_conflict)
UPDATE accounts a SET mac_conflict = true FROM r WHERE a.id = r.id AND r.rn > 1;
INSERT INTO events (seq, time, site_id, kind, account_id, msg)
SELECT (SELECT coalesce(max(seq),0) FROM events) + row_number() OVER (ORDER BY id), now(), site_id, 'audit', id,
       'MAC conflict found by migration 0004: flat ' || flat || ' (' || code || ') has MAC ' || mac ||
       ', already used or malformed. Not provisioned until fixed.'
FROM accounts WHERE mac_conflict AND closed_at IS NULL;
-- Deferred foreign-key checks queued by the updates above must run before DDL.
SET CONSTRAINTS ALL IMMEDIATE;
CREATE UNIQUE INDEX accounts_mac_open ON accounts (mac)
  WHERE closed_at IS NULL AND mac <> '' AND NOT mac_conflict;
CREATE UNIQUE INDEX accounts_site_ip_open ON accounts (site_id, ip)
  WHERE closed_at IS NULL AND ip <> '';
ALTER TABLE accounts ADD CONSTRAINT accounts_mac_shape
  CHECK (mac_conflict OR mac = '' OR mac ~ '^([0-9A-F]{2}:){5}[0-9A-F]{2}$');

UPDATE equipment SET mac = regexp_replace(upper(regexp_replace(mac,'[-:. ]','','g')),
       '^(..)(..)(..)(..)(..)(..)$','\1:\2:\3:\4:\5:\6')
 WHERE mac <> '' AND regexp_replace(mac,'[-:. ]','','g') ~* '^[0-9a-f]{12}$';
SET CONSTRAINTS ALL IMMEDIATE;
-- Equipment MACs are checked by the service layer; the database enforces them
-- only when the data is already clean.
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM equipment WHERE mac <> '' AND status <> 'retired' GROUP BY mac HAVING count(*) > 1) THEN
    CREATE UNIQUE INDEX equipment_mac_live ON equipment (mac) WHERE mac <> '' AND status <> 'retired';
  END IF;
END $$;

ALTER TABLE sites ADD COLUMN access_mode text NOT NULL DEFAULT 'hotspot'
  CHECK (access_mode IN ('hotspot','ipoe_observe','ipoe'));
ALTER TABLE events ADD COLUMN actor text NOT NULL DEFAULT '', ADD COLUMN ip text NOT NULL DEFAULT '';
CREATE INDEX events_audit ON events (seq DESC) WHERE kind = 'audit';

-- What the operator sees of a device that asked for an address.
ALTER TABLE devices ADD COLUMN vendor text NOT NULL DEFAULT '',
                    ADD COLUMN host_name text NOT NULL DEFAULT '',
                    ADD COLUMN online boolean NOT NULL DEFAULT false;

CREATE TABLE admin_users (id text PRIMARY KEY, username text NOT NULL,
  role text NOT NULL CHECK (role IN ('owner','operator','viewer')),
  pass_hash text NOT NULL DEFAULT '', totp_secret_enc text NOT NULL DEFAULT '',
  totp_confirmed_at timestamptz, totp_last_step bigint NOT NULL DEFAULT 0,
  recovery_codes jsonb NOT NULL DEFAULT '[]',
  disabled boolean NOT NULL DEFAULT false, pass_changed_at timestamptz,
  created_at timestamptz NOT NULL, updated_at timestamptz);
CREATE UNIQUE INDEX admin_users_username ON admin_users (lower(username));
CREATE TABLE admin_sessions (id text PRIMARY KEY,
  user_id text NOT NULL REFERENCES admin_users(id) DEFERRABLE INITIALLY DEFERRED,
  csrf text NOT NULL, mfa boolean NOT NULL DEFAULT false,
  ip text NOT NULL DEFAULT '', user_agent text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL, expires_at timestamptz NOT NULL);
CREATE TABLE api_devices (id text PRIMARY KEY,
  user_id text NOT NULL REFERENCES admin_users(id) DEFERRABLE INITIALLY DEFERRED,
  install_id text NOT NULL DEFAULT '', name text NOT NULL DEFAULT '', model text NOT NULL DEFAULT '',
  platform text NOT NULL DEFAULT '', app_version text NOT NULL DEFAULT '',
  access_hash text NOT NULL DEFAULT '', access_expires_at timestamptz,
  refresh_hash text NOT NULL DEFAULT '', refresh_prev_hash text NOT NULL DEFAULT '',
  refresh_rotated_at timestamptz, refresh_expires_at timestamptz, absolute_expires_at timestamptz,
  last_ip text NOT NULL DEFAULT '', created_at timestamptz NOT NULL, last_seen_at timestamptz,
  revoked_at timestamptz, revoked_reason text NOT NULL DEFAULT '');
CREATE UNIQUE INDEX api_devices_access ON api_devices (access_hash) WHERE access_hash <> '';
CREATE UNIQUE INDEX api_devices_refresh ON api_devices (refresh_hash) WHERE refresh_hash <> '';
