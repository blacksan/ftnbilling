-- Ports tab: the single bench port becomes the first of a list of customer
-- ports, and the ports the server keeps switched off are stored.
-- Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE sites ADD COLUMN customer_ports text NOT NULL DEFAULT '',
                  ADD COLUMN disabled_ports text NOT NULL DEFAULT '';

UPDATE sites SET customer_ports = bench_port WHERE bench_port <> '' AND customer_ports = '';
UPDATE sites SET bench_port = '' WHERE bench_port <> '';
