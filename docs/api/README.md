# API notes per stream

`docs/api-v1.md` is the contract. While several streams build v4 in parallel, each one writes what it adds or decides to its own notes file here, so no two streams edit the same document:

| File | Stream |
|---|---|
| `v4-w5c.md` | provisioning job, setup endpoints, `POST /accounts` extension |
| `v4-w9.md` | live router data, interfaces, clients, overview, reboot |
| `v4-w8.md` | Android client notes |
| `v4-w7.md` | web JSON endpoints under `/admin` |

What belongs in a notes file: the exact request and response shapes as built (field names, nullability, error codes and statuses), role needed, limits and caching, and anything that differs from addendum L of `api-v1.md`.

Rules:
- Only write your own file. Do not edit `api-v1.md` or another stream's file.
- Routes are registered in your own `internal/web/api_<stream>.go` hook (`setupRoutes`, `liveRoutes`, `overviewRoutes`), under `/api/v1/routers/...` only. The `/sites` twin exists only for routes that existed before v4.
- The integrator folds the notes into `api-v1.md` and deletes the files.
