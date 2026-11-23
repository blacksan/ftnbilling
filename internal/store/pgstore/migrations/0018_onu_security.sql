-- ONU management round 2: the Wi-Fi choice belongs to the house (accounts.home_wifi), not to the box; the ONU gets a
-- security state (its own ACS login, bound management MAC, quarantine, admin lock) and a small session state
-- (parameter keys for reset detection, what was applied); tasks learn the accepted/confirmed/expired states.
-- Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE accounts ADD COLUMN home_wifi jsonb NOT NULL DEFAULT '{}';
-- Existing data: the Wi-Fi choice of a linked box becomes the choice of its house (a house that already has one keeps it).
UPDATE accounts a SET home_wifi = o.desired
  FROM onus o
 WHERE o.account_id = a.id AND o.account_id <> '' AND o.desired <> '{}'::jsonb AND (o.desired ->> 'set') = 'true'
   AND a.home_wifi = '{}'::jsonb;

ALTER TABLE onus ADD COLUMN sec jsonb NOT NULL DEFAULT '{}',
                 ADD COLUMN state jsonb NOT NULL DEFAULT '{}';
ALTER TABLE onus DROP COLUMN desired;

ALTER TABLE onu_tasks ADD COLUMN after_reboot boolean NOT NULL DEFAULT false,
                      ADD COLUMN pass_leaf text NOT NULL DEFAULT '',
                      ADD COLUMN confirmed_at timestamptz,
                      ADD COLUMN confirmed_by text NOT NULL DEFAULT '';
-- Tasks the old code called done (a Wi-Fi change the box answered) are "accepted" now: nobody confirmed them.
UPDATE onu_tasks SET status = 'accepted' WHERE kind = 'wifi' AND status = 'done';
