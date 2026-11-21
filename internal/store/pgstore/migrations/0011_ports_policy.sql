-- Ports are off by default: the owner enables each port. ports_policy 'off' marks a router on the
-- policy; '' is a router from before it (the first sweep records what is in service). enabled_ports
-- is the comma list of ports the owner enabled.
-- Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE sites ADD COLUMN ports_policy text NOT NULL DEFAULT '',
                  ADD COLUMN enabled_ports text NOT NULL DEFAULT '';
