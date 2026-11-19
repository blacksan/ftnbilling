# API v6: apartments

An apartment is a building ("MG One Central"). A customer account lives in one apartment and has a unit (flat) inside it.
Routers and apartments are many to many: a router can serve several apartments, and a large apartment can be served by
several routers. Additive on top of v5, except the removal of the router contacts calls (below).
Errors use the standard envelope. Roles: `viewer` < `operator` < `owner`. All paths are also served under `/api/v1/routers/...`
where they mention a router.

## The rules

1. An account has an apartment (`apartment_id`, required when creating) and a unit (free text such as `B4`).
2. A unit is used once per apartment among open accounts, compared without regard to case or repeated spaces. Another apartment may use the same unit. Refusal: 409 `unit_in_use`.
3. An account has a router. It must be one of its apartment's routers. When the apartment has exactly one router it is chosen automatically (leave `site_id` out). With several, `site_id` is required (400 `validation_failed`, field `site_id`).
4. A MAC address of an open account is never provisioned in another apartment (see "MAC in another apartment").
5. The server writes a static lease, queue and ARP row for an account only on its own router, and only while that router serves the account's apartment. If it does not, nothing is written and `problem` on the account says why.

## Apartment JSON

```json
{
  "id": "ab86a18",
  "name": "MG One Central",
  "address": "Ngong Road",
  "notes": "",
  "caretaker": {"name": "Mzee Otieno", "phone": "+254722000111", "phone2": "", "email": "", "notes": "gate", "whatsapp_url": "https://wa.me/254722000111"},
  "landlord":  {"name": "", "phone": "", "phone2": "", "email": "", "notes": "", "whatsapp_url": ""},
  "routers": [{"site_id": "s1", "site_name": "Riverside", "accounts": 12, "router_id": "s1", "router_name": "Riverside"}],
  "accounts_total": 12, "accounts_active": 9, "online_now": 7,
  "created_at": "2026-11-19T10:00:00Z"
}
```

`caretaker` and `landlord` are always present (empty strings when not recorded). Phones are stored as `+254XXXXXXXXX`
(the M-Pesa phone rules: `0722 000 111`, `722000111`, `254722000111` and `+254722000111` are all accepted and normalised).
A bad phone is 400 `validation_failed` with `details.fields["caretaker.phone"]` (or `.phone2`, `.email`; `landlord.*` likewise).
`whatsapp_url` is `https://wa.me/<digits>` for `phone`, empty when there is none. `accounts` in a router entry counts the open accounts of this apartment on that router.
`accounts_active` counts open accounts with paid time; `online_now` those with a bound lease right now.
The single apartment answer also has `accounts`: the open accounts (account JSON, below).

## Calls

| Call | Role | Notes |
|---|---|---|
| `GET /apartments` | viewer | `?q=` matches name, address, caretaker or landlord name, and either phone in any form; `?site=` (or `?router=`) only apartments that router serves. Paged: `{items, next_cursor}` |
| `POST /apartments` | operator | body `name` (required, unique ignoring case), `address`, `notes`, `caretaker{name,phone,phone2,email,notes}`, `landlord{...}`, `router_ids` (links on create). 201 with the apartment. 409 `apartment_name_in_use` |
| `GET /apartments/{id}` | viewer | the apartment with its `accounts` |
| `PATCH /apartments/{id}` | operator | any of the create fields. Fields not sent stay. `"caretaker": {}` clears the section. 200 |
| `DELETE /apartments/{id}` | owner | 204. 409 `apartment_in_use` while any account, open or closed, belongs to it |
| `POST /apartments/{id}/routers` | operator | body `{"site_id": "..."}` (`router_id` also accepted). Links a router. 200 with the apartment |
| `DELETE /apartments/{id}/routers/{site_id}` | operator | Unlinks. 409 `router_serves_accounts` while open accounts of this apartment are on that router: move them first (`details` is empty, the message names the router and how many) |
| `POST /accounts/{id}/move` | operator | body `{"apartment_id", "site_id", "unit"}`; each optional (omitted = keep). 200 with the account |

## Accounts

`POST /accounts` takes `apartment_id` (required), `unit` (required; `flat` is accepted as the same field), optional `site_id` (see rule 3),
and the existing fields. `PATCH /accounts/{id}` takes `unit`, `apartment_id` and `site_id` as well; a change of apartment or router is a move (below).
Account JSON gains:

| Field | Type | Meaning |
|---|---|---|
| `apartment` | `{id, name}` or `null` | `null` only for an account that has no apartment (data older than apartments with no router linked) |
| `unit` | string | the unit label; `flat` carries the same text |
| `problem` | string | why the server is not provisioning the account, in words; empty when fine |

`GET /accounts?apartment=<id>` filters; `?q=` also finds by unit and apartment name.

### Moving a customer

`POST /accounts/{id}/move` (or `PATCH` with the same fields) changes the apartment, the router (chosen from the new apartment's routers; automatic with one) and the unit.
When the router changes: the address is allocated from the new router's range, the lease, queue and ARP row are removed from the old router and written on the new one by the
reconciler, pending checkouts are failed, and an event "moved from <apartment> unit <u> on <router> to ..." is added to the account's history. Refusals: 409 `unit_in_use`,
400 `validation_failed` (`site_id` not serving the new apartment, or ambiguous).

## MAC in another apartment

A MAC that belongs to an open account is not provisioned on a router that does not serve that account's apartment. If such a router sees the MAC it stays unregistered (walled) there.

`GET /unregistered` items gain `belongs_to`, `null` for an ordinary unknown device and otherwise:

```json
"belongs_to": {
  "account_id": "6f21262ca511", "code": "J6U5MZ", "customer": "Wanjiru",
  "apartment": {"id": "ab86a18", "name": "Alpha"}, "unit": "A-1",
  "site_id": "s1", "site_name": "Alpha",
  "same_apartment": false,
  "move_to_choices": [{"id": "ab9b1c2", "name": "Beta"}]
}
```

`site_id` is the account's own router (the item's `site_id` is the router that sees the device). `same_apartment` is true when the seeing router also serves the account's
apartment (a second router of a large apartment): moving then changes only the router. Otherwise `move_to_choices` are the apartments the seeing router serves.
A device that is the ONT of an account of the same router is not listed at all. No `suggested_account` is offered for a `belongs_to` device.

`POST /unregistered/move` (operator) body `{"site_id": "<router that sees it>", "mac", "apartment_id"}` moves the owner to that router (and apartment: required unless
`same_apartment` or the router serves one apartment). 200 with the account; 409 `bad_state` when the MAC does not belong to a customer of another router.

Refusals (409, code `mac_other_building`): `POST /accounts`, `PUT /accounts/{id}/mac`, `POST /unregistered/attach`, `POST /unregistered/create` and `PATCH /accounts/{id}`
with a MAC that belongs to an open account of another apartment. `details`:
`{"mac", "belongs_to": {"account_id", "code", "customer", "apartment": {"id","name"}, "unit", "site_id", "site_name"}}`.
The same MAC on an account of the same apartment keeps the older refusal `mac_in_use`.

`POST /unregistered/create` takes `apartment_id` (required), `unit` (`flat` accepted) and `site_id` (the router that sees the device).

## Search

The admin search (`/admin/search.json`, the Ctrl/Cmd+K box) has an "Apartments" group: name, address, caretaker or landlord name, caretaker or landlord phone in any form.

## Removed: router contacts

The router contacts calls are gone and answer 410 `gone`: `GET` and `POST /sites/{id}/contacts`, `PATCH` and `DELETE /sites/{id}/contacts/{cid}` (and the `/routers` twins).
The people are now the `caretaker` and `landlord` sections of the apartment. On upgrade each router's contacts moved to the apartment made for that router: roles `landlord` and `agent`
to `landlord`, every other role to `caretaker`; the first contact fills the section and further ones are appended to its `notes` as `Also <role>: <name>, <phone>`;
a role other than caretaker or landlord is kept as `Role: <role>` in the notes.

## Upgrade

Format 8 (JSON) and migration 0007 (Postgres): one apartment per router, named after it (a repeated name gets ` (<router id>)`), linked to it, with the router's address.
Every account (open and closed) takes the apartment of its router and keeps its flat label as `unit`. When two open accounts of one router shared a label, the later one becomes
`<label> (<code>)` and an audit event says so. `site_contacts` is dropped; JSON files no longer write `contacts`.

## Error codes added

`mac_other_building` (409), `unit_in_use` (409), `apartment_name_in_use` (409), `apartment_in_use` (409), `router_serves_accounts` (409). Validation of apartment fields uses `validation_failed`.
