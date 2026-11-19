-- Apartments (buildings customers live in), their routers, and the contacts that
-- used to hang on routers. Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

CREATE TABLE apartments (
  id text PRIMARY KEY CHECK (id <> ''), name text NOT NULL CHECK (btrim(name) <> ''),
  address text NOT NULL DEFAULT '', notes text NOT NULL DEFAULT '',
  caretaker_name text NOT NULL DEFAULT '', caretaker_phone text NOT NULL DEFAULT '', caretaker_phone2 text NOT NULL DEFAULT '',
  caretaker_email text NOT NULL DEFAULT '', caretaker_notes text NOT NULL DEFAULT '',
  landlord_name text NOT NULL DEFAULT '', landlord_phone text NOT NULL DEFAULT '', landlord_phone2 text NOT NULL DEFAULT '',
  landlord_email text NOT NULL DEFAULT '', landlord_notes text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL);
CREATE UNIQUE INDEX apartments_name_lower ON apartments (lower(name));

-- Many to many: a router can serve several apartments, an apartment can have several routers.
CREATE TABLE apartment_sites (
  apartment_id text NOT NULL REFERENCES apartments(id) DEFERRABLE INITIALLY DEFERRED,
  site_id text NOT NULL REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED,
  created_at timestamptz NOT NULL, PRIMARY KEY (apartment_id, site_id));
CREATE INDEX apartment_sites_site ON apartment_sites (site_id);

ALTER TABLE accounts ADD COLUMN apartment_id text REFERENCES apartments(id) DEFERRABLE INITIALLY DEFERRED,
                     ADD COLUMN unit text NOT NULL DEFAULT '';
CREATE INDEX accounts_apartment ON accounts (apartment_id);

-- One apartment per router, named after it (the router id is added when two share a name).
-- The id is "a" + the first 6 hex digits of md5('apt|' || router id): store.MigratedApartmentID computes the same.
CREATE TEMP TABLE ftn_apt_map ON COMMIT DROP AS
SELECT s.id AS site_id, 'a' || substr(md5('apt|' || s.id), 1, 6) AS apartment_id,
       CASE WHEN row_number() OVER (PARTITION BY lower(nm.name) ORDER BY s.created_at, s.id) > 1
            THEN nm.name || ' (' || s.id || ')' ELSE nm.name END AS name,
       s.address, s.created_at
FROM sites s CROSS JOIN LATERAL (
  SELECT CASE WHEN btrim(regexp_replace(s.name, '\s+', ' ', 'g')) = '' THEN s.id
              ELSE btrim(regexp_replace(s.name, '\s+', ' ', 'g')) END AS name) nm;
INSERT INTO apartments (id, name, address, created_at) SELECT apartment_id, name, address, created_at FROM ftn_apt_map;
INSERT INTO apartment_sites (apartment_id, site_id, created_at) SELECT apartment_id, site_id, created_at FROM ftn_apt_map;

-- Every account, open and closed, takes the apartment of its router; its flat label becomes its unit.
-- A second open account of an apartment with the same label gets " (<code>)" added, with an audit event.
UPDATE accounts a SET apartment_id = m.apartment_id, unit = a.flat FROM ftn_apt_map m WHERE m.site_id = a.site_id;
CREATE TEMP TABLE ftn_unit_dupes ON COMMIT DROP AS
  SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY apartment_id, lower(regexp_replace(btrim(unit), '\s+', ' ', 'g'))
                                                ORDER BY created_at, id) AS rn
                  FROM accounts WHERE closed_at IS NULL AND unit <> '') r WHERE rn > 1;
INSERT INTO events (seq, time, site_id, kind, account_id, msg)
SELECT (SELECT coalesce(max(seq),0) FROM events) + row_number() OVER (ORDER BY a.id), now(), a.site_id, 'audit', a.id,
       'Unit conflict found by migration 0008: flat ' || a.unit || ' (' || a.code || ') shares its label with another open account of the same apartment. Its unit is now '
       || a.unit || ' (' || coalesce(nullif(a.code, ''), a.id) || ').'
FROM accounts a JOIN ftn_unit_dupes x ON x.id = a.id;
UPDATE accounts a SET unit = a.unit || ' (' || coalesce(nullif(a.code, ''), a.id) || ')' FROM ftn_unit_dupes x WHERE x.id = a.id;
UPDATE accounts SET flat = unit WHERE flat <> unit;

-- Router contacts move onto the router's apartment. Landlord and agent go to the landlord
-- section; caretaker, security and other to the caretaker section. The first contact of a
-- section fills it, the rest are listed in its notes; a role that is not caretaker or landlord
-- is written in the notes; Kenyan phones are written +254....
CREATE TEMP TABLE ftn_contacts ON COMMIT DROP AS
SELECT c.*, m.apartment_id,
       CASE WHEN c.role IN ('landlord','agent') THEN 'landlord' ELSE 'caretaker' END AS slot,
       row_number() OVER (PARTITION BY m.apartment_id, CASE WHEN c.role IN ('landlord','agent') THEN 'landlord' ELSE 'caretaker' END
                          ORDER BY c.created_at, c.id) AS rn,
       regexp_replace(c.phone, '[ ().-]', '', 'g') AS p0
FROM site_contacts c JOIN ftn_apt_map m ON m.site_id = c.site_id;
ALTER TABLE ftn_contacts ADD COLUMN phone_norm text;
UPDATE ftn_contacts SET p0 = substr(p0, 2) WHERE p0 LIKE '+%';
UPDATE ftn_contacts SET p0 = '254' || substr(p0, 2) WHERE p0 ~ '^0[0-9]{9}$';
UPDATE ftn_contacts SET p0 = '254' || p0 WHERE p0 ~ '^[17][0-9]{8}$';
UPDATE ftn_contacts SET phone_norm = CASE WHEN p0 ~ '^254[17][0-9]{8}$' THEN '+' || p0 ELSE btrim(phone) END;
CREATE TEMP TABLE ftn_slot ON COMMIT DROP AS
SELECT f.apartment_id, f.slot, f.name, f.phone_norm AS phone, f.email,
       (SELECT coalesce(string_agg(t.x, ' | ' ORDER BY t.ord), '') FROM (
          SELECT 0 AS ord, 'Role: ' || f.role AS x WHERE f.role NOT IN ('caretaker','landlord')
          UNION ALL SELECT 1, f.notes WHERE f.notes <> ''
          UNION ALL SELECT 2 + o.rn, 'Also ' || o.role || ': ' || o.name
                 || CASE WHEN o.phone <> '' THEN ', ' || o.phone ELSE '' END
                 || CASE WHEN o.email <> '' THEN ', ' || o.email ELSE '' END
                 || CASE WHEN o.notes <> '' THEN ' - ' || o.notes ELSE '' END
            FROM ftn_contacts o WHERE o.apartment_id = f.apartment_id AND o.slot = f.slot AND o.rn > 1
       ) t) AS notes
FROM ftn_contacts f WHERE f.rn = 1;
UPDATE apartments a SET caretaker_name = s.name, caretaker_phone = s.phone, caretaker_email = s.email, caretaker_notes = s.notes
  FROM ftn_slot s WHERE s.apartment_id = a.id AND s.slot = 'caretaker';
UPDATE apartments a SET landlord_name = s.name, landlord_phone = s.phone, landlord_email = s.email, landlord_notes = s.notes
  FROM ftn_slot s WHERE s.apartment_id = a.id AND s.slot = 'landlord';

-- Deferred foreign-key checks queued by the statements above must run before DDL.
SET CONSTRAINTS ALL IMMEDIATE;
-- A unit is used once per apartment among open accounts (the same rule Update checks for the JSON store).
CREATE UNIQUE INDEX accounts_apartment_unit_open ON accounts (apartment_id, lower(regexp_replace(btrim(unit), '\s+', ' ', 'g')))
  WHERE closed_at IS NULL AND unit <> '' AND apartment_id IS NOT NULL;

-- The per-router contacts are gone.
DROP TABLE site_contacts;
