# Remote WebFig (stream notes)

Server setting `WEBFIG_DOMAIN` (empty = off). See `deploy/README.md`, "Remote WebFig", for the install line, DNS and the security model.

## API

Router JSON (`GET /api/v1/sites`, `GET /api/v1/sites/{id}`, and the `/routers` twins) has one new field:

| Field | Type | Meaning |
|---|---|---|
| `webfig_url` | string | `<PUBLIC_BASE_URL>/admin/routers/webfig?id=<router id>`: the admin's own step-up page for this router. Empty when `WEBFIG_DOMAIN` is not set, when `PUBLIC_BASE_URL` is empty, or when the router has no tunnel address yet. It is not a secret and carries no ticket. |

There is no other API endpoint. The step-up needs a browser session, which a bearer token does not give, so a client opens `webfig_url` in the system browser. The app shows the button to owners only (the server refuses everyone else on that page).

## Admin pages (browser session, owner only; 403 for operator and viewer)

| Method and path | What |
|---|---|
| `GET /admin/routers/webfig?id=` | The step-up page: password and 6-digit code, or an "Open WebFig" button if a step-up passed within the last 10 minutes. Never mints a ticket. 404 when the feature is off or the router is unknown, 409 when the router has no tunnel. A signed-out browser goes to sign-in and comes back here. |
| `POST /admin/routers/webfig` (`id`, optional `password`, `code`) | With a fresh step-up: mints a one-time ticket (60 s, bound to the user, router and client address) and answers `303` to `https://<WEBFIG_DOMAIN>/_ftn/open?t=<ticket>`. Otherwise checks `password` and `code` (same limits and back-off as sign-in: 401 wrong, 429 too many) and, if right, does the same. |
| `POST /admin/routers/webfig/end` (`id`, `sid`) | Ends one open WebFig session of the router (`sid` is the public session id on the router page). |

(`/admin/sites/webfig...` is the same, like every router page.)

## The WebFig host (`https://<WEBFIG_DOMAIN>`, loopback listener `WEBFIG_LISTEN`, default `127.0.0.1:8082`)

- `GET /_ftn/open?t=` exchanges the ticket for the session cookie `__Host-ftn_wf` and answers `303 /`. A used, expired, unknown or other-address ticket gets 403 and a plain page.
- Any other path under `/_ftn/` is 404 and never proxied.
- Every other request is proxied to `http://<router tunnel IP>:<router www port>/` when it carries a live session cookie; without one: 401 and the page "Open WebFig from the router page in the admin."
- 421 for another `Host`, 400 for a bad request target, 405 for `CONNECT` and `TRACE`, 503 over the connection limit (4 per router, 16 overall), 502 when the router does not answer.

## Router page

Owners see **Open WebFig** in the Connection panel and a "WebFig access" panel (open sessions with End buttons, recent activity). Activity lines are audit events on the router: `WebFig opened on <router> (session <id>)` and `WebFig session <id> on <router> ended: <reason>`.
