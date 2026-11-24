-- OLT management port (internal/sites/oltadopt.go): the router port with the role "OLT management" and the router's own
-- address on it. Both empty = no such port. Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE sites ADD COLUMN olt_mgmt_port text NOT NULL DEFAULT '',
                  ADD COLUMN olt_mgmt_net text NOT NULL DEFAULT '';
