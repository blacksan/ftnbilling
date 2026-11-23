-- ONU management (TR-069): the built-in ACS's devices and their task queue, and the per-router management
-- subnet number and VLAN. Nested values (MACs, Wi-Fi instances, discovery profile, desired Wi-Fi) are jsonb;
-- passphrases and connection-request credentials inside are sealed with DATA_KEY before they get here.
-- Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE sites ADD COLUMN onu_mgmt_n integer NOT NULL DEFAULT 0,
                  ADD COLUMN onu_mgmt_vlan integer NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX sites_onu_mgmt_n ON sites (onu_mgmt_n) WHERE onu_mgmt_n > 0 AND NOT removed;

CREATE TABLE onus (
  id text PRIMARY KEY CHECK (id <> ''), serial text NOT NULL, oui text NOT NULL DEFAULT '',
  manufacturer text NOT NULL DEFAULT '', product_class text NOT NULL DEFAULT '',
  sw_version text NOT NULL DEFAULT '', hw_version text NOT NULL DEFAULT '', root text NOT NULL DEFAULT '',
  site_id text NOT NULL DEFAULT '', mgmt_ip text NOT NULL DEFAULT '', conn_req_url text NOT NULL DEFAULT '',
  cr_user_enc text NOT NULL DEFAULT '', cr_pass_enc text NOT NULL DEFAULT '', cr_applied boolean NOT NULL DEFAULT false,
  first_seen timestamptz, last_inform timestamptz, last_events text NOT NULL DEFAULT '',
  macs jsonb NOT NULL DEFAULT '[]', wifi jsonb NOT NULL DEFAULT '[]', profile jsonb NOT NULL DEFAULT '{}',
  account_id text NOT NULL DEFAULT '', link text NOT NULL DEFAULT '', desired jsonb NOT NULL DEFAULT '{}');
CREATE UNIQUE INDEX onus_serial ON onus (oui, serial);
CREATE INDEX onus_site ON onus (site_id);
CREATE INDEX onus_account ON onus (account_id) WHERE account_id <> '';

CREATE TABLE onu_tasks (
  id text PRIMARY KEY CHECK (id <> ''), onu_id text NOT NULL, site_id text NOT NULL DEFAULT '',
  account_id text NOT NULL DEFAULT '', kind text NOT NULL, status text NOT NULL, fault text NOT NULL DEFAULT '',
  by text NOT NULL DEFAULT '', attempts integer NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL, started_at timestamptz, finished_at timestamptz);
CREATE INDEX onu_tasks_onu ON onu_tasks (onu_id, created_at DESC);
CREATE INDEX onu_tasks_open ON onu_tasks (onu_id) WHERE status IN ('pending', 'running');
