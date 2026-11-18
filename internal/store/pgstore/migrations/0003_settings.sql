-- Admin settings (key -> value), including the admin password hash. Rows with
-- enc = true hold a value sealed with DATA_KEY (see internal/secretbox).
-- Forward-only; never edit once released.
CREATE TABLE settings (
  key text PRIMARY KEY CHECK (key <> ''),
  value text NOT NULL DEFAULT '',
  enc boolean NOT NULL DEFAULT false,
  updated_at timestamptz NOT NULL);
