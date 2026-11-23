-- ONU inventory: the OLT-side (GPON) serial of an ONT, when it differs from the serial the ONU reports over TR-069.
ALTER TABLE equipment ADD COLUMN gpon_serial text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX equipment_gpon_serial ON equipment (upper(gpon_serial)) WHERE gpon_serial <> '';
