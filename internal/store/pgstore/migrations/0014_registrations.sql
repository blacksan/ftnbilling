-- Self-registrations from the captive page: pending until the owner approves or rejects them.
-- Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

CREATE TABLE registrations (
  id text PRIMARY KEY CHECK (id <> ''), site_id text NOT NULL, apartment_id text NOT NULL DEFAULT '', unit text NOT NULL DEFAULT '',
  name text NOT NULL DEFAULT '', phone text NOT NULL DEFAULT '', email text NOT NULL DEFAULT '',
  mac text NOT NULL DEFAULT '', guess_mac text NOT NULL DEFAULT '', status text NOT NULL DEFAULT 'pending',
  consent_at timestamptz, created_at timestamptz NOT NULL, decided_at timestamptz,
  decided_by text NOT NULL DEFAULT '', account_id text NOT NULL DEFAULT '');
CREATE INDEX registrations_pending ON registrations (site_id) WHERE status = 'pending';
CREATE INDEX registrations_phone ON registrations (phone) WHERE phone <> '';
