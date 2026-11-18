# Fast Tech Net API v1

Contract between the server (`internal/web/api*.go`, W1) and the Android app (W4). Source: `docs/rebuild-plan.md` section B, copied verbatim below, plus the section H addendum. Any later change needs a version-bump note in the Changes section at the end.

## API contract: `https://isp.ftw.co.ke/api/v1`

Conventions: JSON UTF-8; RFC 3339 UTC times; money integer KES; MACs `AA:BB:CC:DD:EE:FF` (input accepts any common form). Auth `Authorization: Bearer <access_token>`; cookies ignored on `/api/`. Mutating calls accept `Idempotency-Key` (UUID); server replays stored response for 24 h per device.

Errors: `{"error":{"code":"...","message":"plain words","details":{}}}`

| HTTP | code | Notes |
|---|---|---|
| 400 | `bad_request`, `validation_failed` | `details.fields` = `{"mac":"..."}` |
| 401 | `invalid_credentials`, `totp_invalid`, `token_expired`, `token_revoked` | expired → refresh; revoked → log in again |
| 403 | `totp_enrol_required`, `forbidden_role` | |
| 404 | `not_found` | |
| 409 | `mac_in_use`, `flat_in_use`, `duplicate_payment`, `lower_plan_active`, `bad_state` | |
| 429 | `rate_limited` | `Retry-After` + `details.retry_after_s` |
| 503 | `db_unavailable` | maps `store.ErrUnavailable` |

`mac_in_use` details: `{"mac":"AA:..","account":{"id","code","flat","site_id","site_name"}}`.

Pagination: `?limit=` (default 50, max 200), `?cursor=` opaque. Response `{"items":[...],"next_cursor":"..."|null}`.

Auth:
| Method/path | Request | Response |
|---|---|---|
| POST `/auth/login` | `{username,password,totp?,recovery_code?,setup_code?,device:{install_id,name,model,platform:"android",app_version}}` | `{access_token,access_expires_at,refresh_token,refresh_expires_at,device_id,user:{id,username,role}}` |
| POST `/auth/refresh` | `{refresh_token}` | same token fields |
| POST `/auth/logout` | – | 204, revokes this device |
| GET `/me` | | `{user,device:{id,name,created_at}}` |
| GET `/auth/devices` | | `{items:[{id,name,model,app_version,created_at,last_seen_at,last_ip,current,revoked_at}]}` |
| DELETE `/auth/devices/{id}` | | 204 (owner, or own device) |

Read:
| Path | Response |
|---|---|
| GET `/dashboard?site=` | `{sites_total,sites_online,accounts:{active,expired,suspended,new,mac_conflict},revenue:{today_kes,month_kes,last_month_kes},unmatched_payments,unregistered_devices,open_alerts,pending_router_tasks,generated_at}` |
| GET `/sites` | `{items:[Site]}` |
| GET `/sites/{id}` | `Site` + `provision` + `uptime_30d_pct` |
| GET `/sites/{id}/qoe?period=24h\|7d\|30d` | `{latency_ms,loss_pct,wan:{down_bps,up_bps,cap_mbps,peak_util_pct},series:{t:[],down_bps:[],up_bps:[],rtt_ms:[]},flags:[{kind,text}]}` from `metrics.BuildReport` |
| GET `/sites/{id}/onts` | `{items:[{account_id,code,flat,mac,ip,lease_status:"bound"\|"waiting"\|"none",last_seen,host_name,down_bps,up_bps,suspended_on_router,optical:null}]}` |
| GET `/accounts?site=&state=&q=&cursor=&limit=` | page of `Account` |
| GET `/accounts/{id}` | `Account` + `customer`, `payments` (last 20), `events` (last 20), `usage:{month_bytes}` |
| GET `/packages?site=` | `{items:[{id,name,speed_mbps,duration_min,price_kes,disabled}]}` |
| GET `/payments?site=&status=&method=&from=&to=&q=&cursor=&limit=` | page of `Payment` + `totals:{count,revenue_kes}` |
| GET `/alerts` | `{active:[{key,rule,label,site_id,site_name,state,since,detail}],history:[...],mutes:[{site_id,until}]}` |
| GET `/unregistered?site=` | `{items:[{site_id,mac,ip,source,first_seen,last_seen,hits}]}` |
| GET `/search?q=` | `{accounts:[...],customers:[...],payments:[...]}` from `billing.Search` |
| GET `/audit?cursor=` | page of `{seq,time,actor,ip,msg,account_id,site_id}` (owner only) |

Write (role operator/owner):
| Path | Body | Result |
|---|---|---|
| POST `/accounts` | `{site_id,flat,mac,tenant?,phone?,notes?}` | 201 `Account`; 409 `mac_in_use`/`flat_in_use` |
| PATCH `/accounts/{id}` | `{flat?,tenant?,phone?,notes?}` | `Account` |
| PUT `/accounts/{id}/mac` | `{mac}` | `Account`; 409 `mac_in_use`; 400 `validation_failed` |
| POST `/accounts/{id}/suspend`, `/unsuspend` | – | `Account` |
| POST `/accounts/{id}/package` | `{package_id,keep_time}` | `Account` |
| POST `/accounts/{id}/extend` | `{package_id,note?,force?}` | `Account`; 409 `lower_plan_active` (this is also "activate") |
| POST `/accounts/{id}/reconcile` | – | 202 `{queued:true}` |
| POST `/payments` | `{account_id,amount_kes,trans_code?,package_id?,paid_at?,note?,force?}` | 201 `Payment`; 409 `duplicate_payment` |
| POST `/payments/{key}/assign` | `{account_id,package_id,note?}` | `Payment` |
| POST `/payments/{key}/resolve` | `{status:"refunded"\|"ignored",note?}` | `Payment` |
| POST `/alerts/mute` | `{site_id,minutes}` (0 = unmute) | 204 |

Shapes:
```json
Account {"id","code","site_id","site_name","flat","floor","mac","mac_conflict":false,"ip",
 "tenant","phone","whatsapp","state":"active|expired|suspended|new","suspended":false,
 "package":{"id","name","speed_mbps","price_kes"}|null,"expires_at":null,
 "router":{"in_sync":true,"suspended_on_router":false,"lease_status":"bound","last_seen":null,"pending_task":false,"last_error":""},
 "customer_id","created_at"}
Site {"id","name","access_mode","online":true,"cause":"","tunnel":{"handshake_age_s":12},
 "accounts":{"total","active","expired","suspended"},"open_alerts":0,
 "provision":{"last_run_at","in_sync":true,"drift":0,"pending_ops":0,"last_error":"","foreign_conflicts":[]}}
Payment {"key","site_id","method","status","amount_kes","phone","name","bill_ref","account_id","account_code","flat","package_id","note","paid_at","created_at"}
```

## Addendum H: unregistered devices (pick the MAC from what asked for an address)

Supersedes the `GET /unregistered` row above. `GET /unregistered?site=&online=` returns `{items:[{site_id,site_name,mac,vendor,host_name,ip,source,online,first_seen,last_seen,hits,suggested_account:{id,code,flat}|null}]}`. `source` is `dhcp`, `arp`, `portal` or `hotspot-host`; `online` is true when the lease is bound or ARP was complete within the last 2 minutes; `vendor` and `host_name` are blank when unknown; `online=true` filters to online devices. Sorted online first, then `last_seen` newest first.

| Method/path | Body | Result |
|---|---|---|
| POST `/sites/{id}/unregistered/refresh` | – | 202 `{queued:true}` |
| POST `/unregistered/attach` | `{site_id,mac,account_id}` | `Account`; 409 `mac_in_use` (details name the owning flat) |
| POST `/unregistered/create` | `{site_id,mac,flat,tenant?,phone?,package_id?}` | 201 `Account`; 409 `mac_in_use` / `flat_in_use` |
| POST `/unregistered/dismiss` | `{site_id,mac}` | 204 |

`attach` goes through the same rule as `PUT /accounts/{id}/mac`: one MAC can never be on more than one open account in any building.

## Changes
- v1 (2026-11-18): first version (W0).
