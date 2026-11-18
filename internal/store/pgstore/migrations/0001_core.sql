-- ispbill core schema (Phase 3). Forward-only; never edit once released.
CREATE TABLE meta (key text PRIMARY KEY, value text NOT NULL);
CREATE TABLE sites (
  id text PRIMARY KEY CHECK (id <> ''), name text NOT NULL, mode text NOT NULL CHECK (mode IN ('wg','env')),
  notes text NOT NULL DEFAULT '', support_phone text NOT NULL DEFAULT '',
  disabled boolean NOT NULL DEFAULT false, removed boolean NOT NULL DEFAULT false,
  tunnel_ip text NOT NULL DEFAULT '', wg_pub text NOT NULL DEFAULT '',
  rest_user text NOT NULL DEFAULT '', rest_pass text NOT NULL DEFAULT '', rest_port integer NOT NULL DEFAULT 0,
  join_hash text NOT NULL DEFAULT '', join_expires_at timestamptz, join_created_at timestamptz,
  join_used_at timestamptz, join_used_from text NOT NULL DEFAULT '', joined_at timestamptz,
  hotspot_server text NOT NULL DEFAULT '', wan_interface text NOT NULL DEFAULT '', wan_list text NOT NULL DEFAULT '',
  olt_interface text NOT NULL DEFAULT '', vlan_id integer NOT NULL DEFAULT 0, customer_subnet text NOT NULL DEFAULT '',
  wan_cap_mbps double precision NOT NULL DEFAULT 0, olt_cap_mbps double precision NOT NULL DEFAULT 0,
  fup_tb double precision NOT NULL DEFAULT 0, provision boolean NOT NULL DEFAULT false,
  bench_port text NOT NULL DEFAULT '', legacy_container boolean NOT NULL DEFAULT false,
  pkg_overrides jsonb NOT NULL DEFAULT '{}',
  address text NOT NULL DEFAULT '', lat double precision NOT NULL DEFAULT 0, lng double precision NOT NULL DEFAULT 0,
  go_live timestamptz, created_at timestamptz NOT NULL,
  CHECK (lat BETWEEN -90 AND 90 AND lng BETWEEN -180 AND 180));
CREATE UNIQUE INDEX sites_tunnel_ip_live ON sites (tunnel_ip) WHERE NOT removed AND tunnel_ip <> '';
CREATE TABLE packages (id text PRIMARY KEY, name text NOT NULL, slug text NOT NULL, duration_min integer NOT NULL,
  speed_mbps integer NOT NULL, price_kes integer NOT NULL, disabled boolean NOT NULL DEFAULT false,
  deleted boolean NOT NULL DEFAULT false, sort integer NOT NULL DEFAULT 0);
CREATE TABLE customers (id text PRIMARY KEY, name text NOT NULL, phone text NOT NULL DEFAULT '', phone2 text NOT NULL DEFAULT '',
  whatsapp text NOT NULL DEFAULT '', wa_opt_out boolean NOT NULL DEFAULT false, email text NOT NULL DEFAULT '',
  notes text NOT NULL DEFAULT '', national_id_enc text NOT NULL DEFAULT '', national_id_hmac text NOT NULL DEFAULT '',
  national_id_last4 text NOT NULL DEFAULT '', anonymized_at timestamptz, created_at timestamptz NOT NULL, updated_at timestamptz);
CREATE INDEX customers_phone ON customers (phone) WHERE phone <> '';
CREATE INDEX customers_whatsapp ON customers (whatsapp) WHERE whatsapp <> '';
CREATE INDEX customers_name_lower ON customers (lower(name));
CREATE INDEX customers_nid ON customers (national_id_hmac) WHERE national_id_hmac <> '';
CREATE TABLE equipment (id text PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('ont','router','olt','splitter','switch','other')),
  serial text NOT NULL DEFAULT '', mac text NOT NULL DEFAULT '', model text NOT NULL DEFAULT '', firmware text NOT NULL DEFAULT '',
  status text NOT NULL CHECK (status IN ('in_stock','installed','faulty','returned','retired')),
  site_id text REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED, account_id text,
  installed_at timestamptz, notes text NOT NULL DEFAULT '', created_at timestamptz NOT NULL, updated_at timestamptz,
  CHECK (account_id IS NULL OR status = 'installed'));
CREATE UNIQUE INDEX equipment_serial ON equipment (kind, upper(serial)) WHERE serial <> '';
CREATE INDEX equipment_mac ON equipment (mac) WHERE mac <> '';
CREATE INDEX equipment_site ON equipment (site_id); CREATE INDEX equipment_account ON equipment (account_id);
CREATE INDEX equipment_status ON equipment (status);
CREATE TABLE accounts (id text PRIMARY KEY, site_id text NOT NULL REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED,
  code text NOT NULL DEFAULT '', flat text NOT NULL, floor text NOT NULL DEFAULT '', mac text NOT NULL DEFAULT '',
  tenant text NOT NULL DEFAULT '', phone text NOT NULL DEFAULT '', notes text NOT NULL DEFAULT '',
  whatsapp text NOT NULL DEFAULT '', wa_opt_out boolean NOT NULL DEFAULT false,
  customer_id text REFERENCES customers(id) DEFERRABLE INITIALLY DEFERRED,
  package_id text REFERENCES packages(id) DEFERRABLE INITIALLY DEFERRED,
  expires_at timestamptz, suspended boolean NOT NULL DEFAULT false, hs_pass text NOT NULL DEFAULT '',
  router_active boolean NOT NULL DEFAULT false, reminded_for timestamptz, expired_for timestamptz,
  odb text NOT NULL DEFAULT '', splitter_id text REFERENCES equipment(id) DEFERRABLE INITIALLY DEFERRED,
  splitter_port integer NOT NULL DEFAULT 0, created_at timestamptz NOT NULL, closed_at timestamptz);
CREATE UNIQUE INDEX accounts_code ON accounts (code) WHERE code <> '';
CREATE UNIQUE INDEX accounts_site_mac_open ON accounts (site_id, upper(mac)) WHERE closed_at IS NULL AND mac <> '';
CREATE INDEX accounts_site_flat ON accounts (site_id, lower(flat));
CREATE INDEX accounts_customer ON accounts (customer_id); CREATE INDEX accounts_phone ON accounts (phone) WHERE phone <> '';
ALTER TABLE equipment ADD CONSTRAINT equipment_account_fk FOREIGN KEY (account_id) REFERENCES accounts(id) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE tenancies (id text PRIMARY KEY, customer_id text NOT NULL REFERENCES customers(id) DEFERRABLE INITIALLY DEFERRED,
  account_id text NOT NULL REFERENCES accounts(id) DEFERRABLE INITIALLY DEFERRED,
  move_in timestamptz NOT NULL, move_out timestamptz, note text NOT NULL DEFAULT '',
  CHECK (move_out IS NULL OR move_out >= move_in));
CREATE UNIQUE INDEX tenancies_one_open ON tenancies (account_id) WHERE move_out IS NULL;
CREATE INDEX tenancies_customer ON tenancies (customer_id);
CREATE TABLE site_contacts (id text PRIMARY KEY, site_id text NOT NULL REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED,
  role text NOT NULL CHECK (role IN ('landlord','caretaker','agent','security','other')), name text NOT NULL,
  phone text NOT NULL DEFAULT '', email text NOT NULL DEFAULT '', notes text NOT NULL DEFAULT '', created_at timestamptz NOT NULL);
CREATE INDEX site_contacts_site ON site_contacts (site_id);
CREATE TABLE orders (id text PRIMARY KEY, site_id text REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED,
  account_id text REFERENCES accounts(id) DEFERRABLE INITIALLY DEFERRED, package_id text REFERENCES packages(id) DEFERRABLE INITIALLY DEFERRED,
  phone text NOT NULL DEFAULT '', method text NOT NULL, amount_kes integer NOT NULL,
  status text NOT NULL CHECK (status IN ('pending','paid','failed')), reason text NOT NULL DEFAULT '',
  checkout_request_id text NOT NULL DEFAULT '', receipt text NOT NULL DEFAULT '', voucher_code text NOT NULL DEFAULT '',
  logged_in boolean NOT NULL DEFAULT false, mac text NOT NULL DEFAULT '', ip text NOT NULL DEFAULT '', dst text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL, paid_at timestamptz);
CREATE INDEX orders_created ON orders (created_at DESC); CREATE INDEX orders_account ON orders (account_id, created_at DESC);
CREATE INDEX orders_checkout ON orders (checkout_request_id) WHERE checkout_request_id <> '';
CREATE INDEX orders_pending ON orders (created_at) WHERE status = 'pending';
CREATE TABLE vouchers (code text PRIMARY KEY, site_id text REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED,
  package_id text NOT NULL REFERENCES packages(id) DEFERRABLE INITIALLY DEFERRED, batch text NOT NULL DEFAULT '',
  used boolean NOT NULL DEFAULT false, used_at timestamptz, used_by text NOT NULL DEFAULT '', created_at timestamptz NOT NULL);
CREATE INDEX vouchers_batch ON vouchers (batch);
CREATE TABLE payments (key text PRIMARY KEY CHECK (key = upper(key) AND key <> ''),
  site_id text REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED, method text NOT NULL,
  status text NOT NULL CHECK (status IN ('applied','unmatched','refunded','ignored')), amount_kes integer NOT NULL,
  phone text NOT NULL DEFAULT '', name text NOT NULL DEFAULT '', bill_ref text NOT NULL DEFAULT '',
  account_id text REFERENCES accounts(id) DEFERRABLE INITIALLY DEFERRED,
  customer_id text REFERENCES customers(id) DEFERRABLE INITIALLY DEFERRED,
  package_id text REFERENCES packages(id) DEFERRABLE INITIALLY DEFERRED,
  order_id text NOT NULL DEFAULT '', note text NOT NULL DEFAULT '',
  paid_at timestamptz NOT NULL, created_at timestamptz NOT NULL, resolved_at timestamptz);
CREATE INDEX payments_paid ON payments (paid_at DESC); CREATE INDEX payments_account ON payments (account_id, paid_at DESC);
CREATE INDEX payments_customer ON payments (customer_id, paid_at DESC); CREATE INDEX payments_site ON payments (site_id, paid_at DESC);
CREATE INDEX payments_unmatched ON payments (created_at) WHERE status = 'unmatched';
CREATE INDEX payments_phone ON payments (phone) WHERE phone <> '';
CREATE TABLE devices (site_id text NOT NULL REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED, mac text NOT NULL,
  ip text NOT NULL DEFAULT '', source text NOT NULL DEFAULT '', first_seen timestamptz, last_seen timestamptz,
  hits integer NOT NULL DEFAULT 0, PRIMARY KEY (site_id, mac));
CREATE TABLE tasks (id text PRIMARY KEY, site_id text NOT NULL, account_id text NOT NULL, code text NOT NULL DEFAULT '',
  kick boolean NOT NULL DEFAULT false, rev integer NOT NULL DEFAULT 0, reason text NOT NULL DEFAULT '',
  attempts integer NOT NULL DEFAULT 0, next_try timestamptz, last_err text NOT NULL DEFAULT '', created_at timestamptz NOT NULL);
CREATE TABLE events (seq bigint PRIMARY KEY, time timestamptz NOT NULL, site_id text NOT NULL DEFAULT '', kind text NOT NULL,
  account_id text NOT NULL DEFAULT '', customer_id text NOT NULL DEFAULT '', equipment_id text NOT NULL DEFAULT '', msg text NOT NULL);
CREATE INDEX events_account ON events (account_id, seq DESC) WHERE account_id <> '';
CREATE INDEX events_site ON events (site_id, seq DESC);
CREATE TABLE equipment_events (id text PRIMARY KEY, equipment_id text NOT NULL REFERENCES equipment(id) DEFERRABLE INITIALLY DEFERRED,
  time timestamptz NOT NULL,
  kind text NOT NULL CHECK (kind IN ('received','assigned','unassigned','returned','faulty','repaired','moved','edited','note')),
  from_status text NOT NULL DEFAULT '', to_status text NOT NULL DEFAULT '', site_id text NOT NULL DEFAULT '',
  account_id text NOT NULL DEFAULT '', customer_id text NOT NULL DEFAULT '', note text NOT NULL DEFAULT '');
CREATE INDEX equipment_events_eq ON equipment_events (equipment_id, time DESC);
CREATE INDEX equipment_events_account ON equipment_events (account_id) WHERE account_id <> '';
CREATE TABLE notes (id text PRIMARY KEY, time timestamptz NOT NULL, customer_id text NOT NULL DEFAULT '',
  account_id text NOT NULL DEFAULT '', site_id text NOT NULL DEFAULT '', equipment_id text NOT NULL DEFAULT '', text text NOT NULL);
CREATE INDEX notes_customer ON notes (customer_id, time DESC) WHERE customer_id <> '';
CREATE INDEX notes_account ON notes (account_id, time DESC) WHERE account_id <> '';
