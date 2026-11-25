-- ONU inventory: the serial a box reports in its CWMP DeviceId when it is neither the serial nor the GPON serial
-- (V-SOL V2804N-Z: "12345" + its label MAC). Linked by the owner from the unknown-ONU list. Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE equipment ADD COLUMN tr069_serial text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX equipment_tr069_serial ON equipment (upper(tr069_serial)) WHERE tr069_serial <> '';
