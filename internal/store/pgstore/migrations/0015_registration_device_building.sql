-- A registration carries the device name the visitor gave and, when the server could not tell the router, the
-- building name they typed (the owner assigns it to a router). Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE registrations ADD COLUMN device_name text NOT NULL DEFAULT '';
ALTER TABLE registrations ADD COLUMN building text NOT NULL DEFAULT '';
