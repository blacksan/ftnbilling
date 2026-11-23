-- Answers of the online MAC-vendor lookup (internal/billing/ouilookup.go), keyed by the OUI, the first three bytes of
-- a MAC ("AA:BB:CC"). found = false is a "no such prefix" answer, asked again after 30 days. Public registry data:
-- no MAC address of a customer is ever stored here. Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

CREATE TABLE oui_cache (
  prefix text PRIMARY KEY CHECK (prefix ~ '^[0-9A-F]{2}:[0-9A-F]{2}:[0-9A-F]{2}$'),
  vendor text NOT NULL DEFAULT '' CHECK (char_length(vendor) <= 80),
  found boolean NOT NULL DEFAULT false,
  checked_at timestamptz NOT NULL);
