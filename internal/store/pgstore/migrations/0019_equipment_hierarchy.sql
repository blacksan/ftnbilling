-- Equipment hierarchy: router > OLT > PON port > ONU. An OLT is an equipment item of kind 'olt' (router in site_id);
-- an ONU (kind 'ont') names its OLT and PON port. Everything is optional: items and accounts that exist today keep
-- working with the new columns empty. pon_apartments maps a PON port number to the apartment ids it serves (jsonb).
-- mgmt_pass is sealed with the data key by the application and never leaves it. Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE equipment ADD COLUMN name text NOT NULL DEFAULT '',
                      ADD COLUMN router_port text NOT NULL DEFAULT '',
                      ADD COLUMN pon_ports integer NOT NULL DEFAULT 0,
                      ADD COLUMN pon_apartments jsonb NOT NULL DEFAULT '{}',
                      ADD COLUMN mgmt_ip text NOT NULL DEFAULT '',
                      ADD COLUMN mgmt_user text NOT NULL DEFAULT '',
                      ADD COLUMN mgmt_pass text NOT NULL DEFAULT '',
                      ADD COLUMN olt_id text NOT NULL DEFAULT '',
                      ADD COLUMN pon_port integer NOT NULL DEFAULT 0;
ALTER TABLE equipment ADD CONSTRAINT equipment_pon_port_range CHECK (pon_port >= 0 AND pon_ports >= 0);
CREATE INDEX equipment_olt ON equipment (olt_id) WHERE olt_id <> '';
