-- OLT management (internal/olt): per-OLT connection settings, the last status read, the ONUs found on the PON ports
-- and the saved copies of the running configuration, keyed by the OLT's equipment id. Nested values are jsonb. The
-- enable password and the saved configurations inside are sealed with DATA_KEY before they get here.
-- Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

CREATE TABLE olt_states (
  id text PRIMARY KEY CHECK (id <> ''),
  settings jsonb NOT NULL DEFAULT '{}', status jsonb NOT NULL DEFAULT '{}',
  found jsonb NOT NULL DEFAULT '[]', backups jsonb NOT NULL DEFAULT '[]',
  updated timestamptz);
