package storetest

// V2JSON is a format 2 (single building) database file.
const V2JSON = `{
 "format": 2,
 "packages": {"p1": {"id":"p1","name":"1 Day","slug":"1-day","duration_min":1440,"speed_mbps":10,"price_kes":50,"sort":10}},
 "accounts": {"a1": {"id":"a1","code":"ABC234","flat":"Flat 1","mac":"AA:BB:CC:DD:EE:01","hs_pass":"x","created_at":"2026-01-01T00:00:00Z"}},
 "orders": {"o1": {"id":"o1","account_id":"a1","package_id":"p1","method":"test","amount_kes":50,"status":"paid","logged_in":false,"created_at":"2026-01-02T00:00:00Z"}},
 "vouchers": {"V1": {"code":"V1","package_id":"p1","batch":"b","used":false,"created_at":"2026-01-01T00:00:00Z"}},
 "payments": {"K1": {"key":"K1","method":"mpesa-c2b","status":"applied","amount_kes":50,"account_id":"a1","paid_at":"2026-01-02T00:00:00Z","created_at":"2026-01-02T00:00:00Z"}},
 "devices": {"AA:BB:CC:DD:EE:02": {"mac":"AA:BB:CC:DD:EE:02","source":"portal","first_seen":"2026-01-03T00:00:00Z","last_seen":"2026-01-03T00:00:00Z","hits":2}},
 "tasks": {"t1": {"id":"t1","account_id":"a1","code":"ABC234","rev":1,"attempts":0,"next_try":"2026-01-03T00:00:00Z","created_at":"2026-01-03T00:00:00Z"}},
 "events": [{"time":"2026-01-03T00:00:00Z","kind":"info","msg":"hello"}]
}`

// V3JSON is a format 3 (multi-building) database file with stray references.
const V3JSON = `{
 "format": 3,
 "sites": {"s111111": {"id":"s111111","name":"Block A","mode":"env","created_at":"2026-01-01T00:00:00Z"}},
 "packages": {"p1": {"id":"p1","name":"1 Day","slug":"1-day","duration_min":1440,"speed_mbps":10,"price_kes":50,"sort":10}},
 "accounts": {
  "a1": {"id":"a1","site_id":"s111111","code":"AAA222","flat":"Flat 1","mac":"AA:BB:CC:DD:EE:01","tenant":"Alice","phone":"254711000001","whatsapp":"254711000001","hs_pass":"x","created_at":"2026-01-02T00:00:00Z"},
  "a2": {"id":"a2","site_id":"s111111","code":"BBB333","flat":"Flat 2","mac":"AA:BB:CC:DD:EE:02","tenant":"","phone":"0711000001","wa_opt_out":true,"hs_pass":"x","created_at":"2026-01-03T00:00:00Z"},
  "a3": {"id":"a3","site_id":"s111111","code":"CCC444","flat":"Flat 3","mac":"AA:BB:CC:DD:EE:03","tenant":"Bob","hs_pass":"x","created_at":"2026-01-04T00:00:00Z"},
  "a4": {"id":"a4","site_id":"s111111","code":"DDD555","flat":"Flat 4","mac":"AA:BB:CC:DD:EE:04","hs_pass":"x","created_at":"2026-01-05T00:00:00Z"}
 },
 "payments": {
  "K1": {"key":"K1","site_id":"s111111","method":"mpesa-c2b","status":"applied","amount_kes":50,"account_id":"a1","paid_at":"2026-01-06T00:00:00Z","created_at":"2026-01-06T00:00:00Z"},
  "K2": {"key":"K2","site_id":"s111111","method":"mpesa-c2b","status":"applied","amount_kes":50,"account_id":"gone","bill_ref":"ZZ9QQQ (deleted: Flat 9)","paid_at":"2026-01-07T00:00:00Z","created_at":"2026-01-07T00:00:00Z"}
 },
 "orders": {"o1": {"id":"o1","site_id":"s111111","account_id":"gone2","package_id":"p1","method":"test","amount_kes":50,"status":"failed","created_at":"2026-01-08T00:00:00Z"}},
 "events": [{"time":"2026-01-03T00:00:00Z","kind":"info","msg":"one"},{"time":"2026-01-04T00:00:00Z","kind":"info","msg":"two"}]
}`
