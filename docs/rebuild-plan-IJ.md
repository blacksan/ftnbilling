## I. Contract from W3 (installer) that W1 must satisfy
- `ispbill admin-reset-password -setup-code`: prints the code, exit 0. If the owner already has TOTP enrolled: print a note, exit 0, no new code (installer calls it on every run).
- Flags exactly `-reset-totp`, `-revoke-sessions`, `-setup-code` (deploy/ftnbilling-admin-reset passes `--generate --setup-code --reset-totp --revoke-sessions`).
- Remove `ADMIN_ALLOW_CIDR` from config (old env lines are left; must be ignored harmlessly).
- `GET /admin/login` returns 200 when not signed in; `/admin` and `/api` refuse requests from WG_SUBNET (404).
- First-boot owner seeding from `admin.pass_hash` setting / `ADMIN_PASS` env so the setup code has a user.
- Expose "TOTP enrolled" for the doctor (e.g. `ispbill admin-status -json` → `{"owner":"admin","totp_enrolled":true}`); W1 adds it, doctor check can follow.
- `ispbill migrate -check` already exists (W3, Backend.AppliedVersion).

## J. Assumptions the Android app (W4, branch android-native) already makes — W1 must implement these exactly (and add them to docs/api-v1.md)
- `/auth/refresh` response includes `user` (app tolerates absence).
- Payment status values: `matched`, `unmatched`, `refunded`, `ignored`; app calls `GET /payments?status=unmatched`.
- Account detail `customer` = `{id,name,phone,email}`; `events[]` = `{time,msg,actor}`.
- `lease_status` ∈ `bound|waiting|none`; account `state` ∈ `active|expired|suspended|new`.
- `/unregistered` items include `site_name` and `suggested_account` (nullable); `?online=true` accepted.
- `POST /sites/{id}/unregistered/refresh` → 202 with empty body (or `{}`).
- Login with TOTP not enrolled → 403 `totp_enrol_required`; 429 carries `Retry-After` header and `details.retry_after_s`.
- Idempotency-Key replay returns the identical stored status + body.
