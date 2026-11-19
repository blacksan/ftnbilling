-- v5 bootstrap: one script builds the router, every router is IPoE, the
-- provisioning job is gone. Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

-- WAN type and the WAN, PPPoE and admin LAN settings the script is rendered from.
-- The PPPoE password is sealed with DATA_KEY by the application.
ALTER TABLE sites ADD COLUMN wan_mode text NOT NULL DEFAULT '',
                  ADD COLUMN wan_address text NOT NULL DEFAULT '',
                  ADD COLUMN wan_gateway text NOT NULL DEFAULT '',
                  ADD COLUMN wan_dns text NOT NULL DEFAULT '',
                  ADD COLUMN pppoe_user text NOT NULL DEFAULT '',
                  ADD COLUMN pppoe_pass_enc text NOT NULL DEFAULT '',
                  ADD COLUMN lan_ports text NOT NULL DEFAULT '',
                  ADD COLUMN lan_subnet text NOT NULL DEFAULT '';

-- Every router is IPoE. The column and its check stay (the single value 'ipoe' is in use).
UPDATE sites SET access_mode = 'ipoe';
ALTER TABLE sites ALTER COLUMN access_mode SET DEFAULT 'ipoe';

-- Settings of the old provisioning job and the hotspot-to-IPoE cutover.
DELETE FROM settings
 WHERE key ~ '^site\.[^.]+\.(prov_job|prov_lease|serving_customers|cutover_seen_at|cutover_confirmed_at|hotspot_bypass_at)$';
