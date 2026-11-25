-- ONU parameter trees (internal/acs): the TR-069 parameter names (and the values of the ones that are not secret) the
-- ACS found by walking an ONU's data model, and the real paths resolved from them (optical levels, WAN counters,
-- hosts, Wi-Fi). One current tree per ONU, keyed by the ONU id, replaced on every walk. The application keeps at most
-- 3000 parameters per ONU (tree is one line per parameter: path, flags, value). The value of a parameter whose name
-- looks secret (passphrase, password, key) is never read or stored. Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

CREATE TABLE onu_trees (
  onu_id text PRIMARY KEY CHECK (onu_id <> ''),
  walked_at timestamptz NOT NULL,
  root text NOT NULL DEFAULT '',
  sw_version text NOT NULL DEFAULT '', hw_version text NOT NULL DEFAULT '',
  reason text NOT NULL DEFAULT '',
  params integer NOT NULL DEFAULT 0,
  truncated boolean NOT NULL DEFAULT false,
  tree text NOT NULL DEFAULT '',
  resolved jsonb NOT NULL DEFAULT '{}');
