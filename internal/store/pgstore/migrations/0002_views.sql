-- Reporting views for ad-hoc SQL. None of them expose secret columns
-- (rest_pass, hs_pass, join_hash, national id ciphertext or hash).

CREATE VIEW v_payment_history AS
SELECT p.key, p.paid_at, (p.paid_at AT TIME ZONE 'Africa/Nairobi') AS paid_at_eat,
       p.method, p.status, p.amount_kes,
       s.name AS site, a.code AS account_code, a.flat, a.floor,
       c.name AS customer, c.phone AS customer_phone,
       k.name AS package, p.phone AS payer_phone, p.name AS payer_name, p.bill_ref, p.note
FROM payments p
LEFT JOIN sites s ON s.id = p.site_id
LEFT JOIN accounts a ON a.id = p.account_id
LEFT JOIN customers c ON c.id = p.customer_id
LEFT JOIN packages k ON k.id = p.package_id;

CREATE VIEW v_revenue_monthly AS
SELECT p.site_id, s.name AS site,
       to_char(date_trunc('month', p.paid_at AT TIME ZONE 'Africa/Nairobi'), 'YYYY-MM') AS month_eat,
       count(*) AS payments, sum(p.amount_kes) AS revenue_kes
FROM payments p
LEFT JOIN sites s ON s.id = p.site_id
WHERE p.status = 'applied' AND p.method NOT IN ('voucher', 'admin')
GROUP BY p.site_id, s.name, date_trunc('month', p.paid_at AT TIME ZONE 'Africa/Nairobi');

CREATE VIEW v_equipment_current AS
SELECT e.id, e.kind, e.serial, e.mac, e.model, e.firmware, e.status, e.installed_at,
       s.name AS site, a.code AS account_code, a.flat, c.name AS customer
FROM equipment e
LEFT JOIN sites s ON s.id = e.site_id
LEFT JOIN accounts a ON a.id = e.account_id
LEFT JOIN customers c ON c.id = a.customer_id;

CREATE VIEW v_customers AS
SELECT c.id, c.name, c.phone, c.phone2, c.whatsapp, c.wa_opt_out, c.email, c.created_at, c.anonymized_at,
       (c.national_id_hmac <> '') AS has_national_id,
       (SELECT count(*) FROM tenancies t WHERE t.customer_id = c.id AND t.move_out IS NULL) AS open_tenancies
FROM customers c;
