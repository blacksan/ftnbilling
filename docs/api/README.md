# API notes per stream

`docs/api-v1.md` is the contract. While several streams build v4 in parallel, each one writes what it adds or decides to its own notes file here, so no two streams edit the same document:

| File | Stream |
|---|---|
| `v4-w5c.md` | historical: the provisioning job (superseded by v5) and the `POST /accounts` extension |
| `v4-w9.md` | live router data, interfaces, clients, overview, reboot |
| `v5-bootstrap.md` | the one-line router set-up, router record, automatic fixes, checks, ports; lists the calls that answer 410 |
| `v8-onu.md` | ONU management (TR-069): Wi-Fi change, reboot, ONU list and links, the ONU register (serial + WAN MAC pairs), swap ONU |
| `v7-webfig.md` | remote WebFig: `webfig_url` on router JSON, the admin step-up pages, the WebFig host |
| `v6-apartments.md` | apartments (buildings), units, caretaker and landlord, the MAC-in-another-apartment rule |

What belongs in a notes file: the exact request and response shapes as built (field names, nullability, error codes and statuses), role needed, limits and caching, and anything that differs from addendum L of `api-v1.md`.

Rules:
- Only write your own file. Do not edit `api-v1.md` or another stream's file.
- Routes are registered in your own `internal/web/api_<stream>.go` hook (`setupRoutes`, `liveRoutes`, `overviewRoutes`), under `/api/v1/routers/...` only. The `/sites` twin exists only for routes that existed before v4.
- The notes are folded into `api-v1.md` where it says so; until a file is deleted it stays here, and a banner at its top says when it is superseded. `api-v1.md` points at the file that wins.
