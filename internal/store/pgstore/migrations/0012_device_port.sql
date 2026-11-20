-- A device seen only in the bridge host table has no address yet: the port it sits on is shown instead.
-- Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE devices ADD COLUMN port text NOT NULL DEFAULT '';
