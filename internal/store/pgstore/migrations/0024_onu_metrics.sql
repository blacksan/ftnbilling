-- ONU metrics (internal/acs): the time series the ACS keeps per ONU from its TR-069 reads: the last optical, WAN and
-- host readings, and three bounded tiers of samples (fine 48 h, 5-minute 14 days, hourly 90 days; the application
-- compacts them on every write, so a row cannot grow without limit). One row per ONU, keyed by the ONU id.
-- Nothing in it is a secret. Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

CREATE TABLE onu_metrics (
  onu_id text PRIMARY KEY CHECK (onu_id <> ''),
  updated timestamptz NOT NULL,
  uptime bigint NOT NULL DEFAULT 0, reboots integer NOT NULL DEFAULT 0, last_boot timestamptz,
  optical jsonb, wan jsonb,
  hosts jsonb NOT NULL DEFAULT '[]', gaps jsonb NOT NULL DEFAULT '[]', diag jsonb NOT NULL DEFAULT '[]',
  paths jsonb NOT NULL DEFAULT '{}',
  fine jsonb NOT NULL DEFAULT '[]', mid jsonb NOT NULL DEFAULT '[]', hourly jsonb NOT NULL DEFAULT '[]');
