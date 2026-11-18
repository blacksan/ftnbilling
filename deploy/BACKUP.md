# Backups: database dumps, restore drill, weekly Google Drive

There are three layers, all installed by `deploy/install.sh`:

| Layer | When | What |
|---|---|---|
| Database dump | every 6 hours (`ftnbilling-dump.timer`) | `pg_dump -Fc` of the `ftnbilling` database into `/var/backups/ftnbilling/db/` (root only, mode 0600). Each dump is checked with `pg_restore -l` and written under a temporary name first. Kept: the newest 8, one per day for 14 days, one per week for 8 weeks, one per month for 12 months. The same run also makes the daily file tar (`ispbill.json*`, `outbox.json`, `alerts.json`, `fleet.json`, `spool/`, kept 30 days) and, on Sundays, the `metrics/` tar (kept 4 weeks). This replaces the old daily cron job. Run by hand: `sudo ftnbilling-backup dump` |
| Restore drill | monthly (`ftnbilling-drill.timer`) | Creates a scratch database `ftnbilling_drill`, restores the newest dump into it, runs `ftnbilling db-check` against it and compares row counts and the sum of applied payments with production (the restored copy may be missing recent rows but must never have more), then drops it. A backup you have never restored is only a hope. Run by hand: `sudo ftnbilling-backup drill` |
| Google Drive | weekly (below) | An encrypted snapshot off the server, with a fresh dump, the settings file (including `DATA_KEY`), `wg0.conf` and `wg-admin.conf` |

The last dump, drill and upload results are written to `/var/lib/ftnbilling/backup-status.json` (`dump_at`, `dump_file`, `dump_bytes`, `drill_at`, `drill_ok`, `drill_msg`, `upload_at`, `upload_ok`, `error`), which the admin Settings page shows; the app treats a dump older than 36 hours as stale. A failed dump or drill also sends a Telegram message if `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` are set. `sudo ftnbilling-doctor` checks all of this.

Dumps on the same server do not protect you from losing the server: set up the Drive upload (section 1).

## Weekly Drive upload

Every Sunday at 02:30 (Nairobi time) the server makes one snapshot of
everything it needs to be rebuilt, scrambles (encrypts) it, and uploads it to
your Google Drive. Snapshots older than 12 weeks are deleted automatically.
The newest snapshot is also kept on the server in
`/var/backups/ftnbilling/weekly/`.

Why encrypt? The snapshot contains passwords (admin, router passwords,
M-Pesa keys) and the WireGuard private key. They are encrypted on the server
before upload, so nobody with access to your Google account can read them.
The price: **if you lose the two backup passphrases, the backups cannot be
opened by anyone, including us.**

## 1. One-time setup

You need two things open: your laptop and an SSH session to the server.

### On your laptop

1. Install rclone from <https://rclone.org/downloads/> (a single program; no
   installer wizard needed).
2. Open a terminal (Command Prompt on Windows) and run:

   ```
   rclone authorize "drive" --drive-scope drive.file
   ```

3. A browser opens. Sign in with the Google account whose Drive should hold
   the backups and press **Allow**. (`drive.file` means the backup tool can
   only see files it created itself, not the rest of your Drive.)
4. Go back to the terminal. It prints a line starting with `{"access_token":`
   and ending with `}`. Copy that whole line. Treat it like a password.

### On the server

```
sudo ftnbilling-backup setup
```

(If the command is not found, run the installer again: see `deploy/README.md`.)

1. Paste the line from the laptop when asked.
2. The tool tests the connection (uploads, downloads and deletes a tiny
   file) and prints two lines:

   ```
   Backup passphrase (password):  ...
   Backup salt (password2):       ...
   ```

3. **Save both lines in your password manager now**, and also somewhere that
   is not this server (for example, a note in the manager titled "Fast Tech
   Net backup passphrase"). They are shown only once. A copy lives on the
   server in `/root/.config/rclone/rclone.conf`, but that is lost together
   with the server, which is exactly when you need it.
4. Press Enter. The weekly timer is switched on.

## 2. First test run

```
sudo ftnbilling-backup run
sudo ftnbilling-backup list
sudo ftnbilling-backup status
```

`run` prints "Backup OK" when the snapshot is uploaded and checked. `list`
shows the snapshot by its real name, for example
`ftnbilling-vps1-2026-11-22.tar.gz`.

Now open Google Drive in a browser. You will see a folder
`FastTechNet-backups` with folders and files whose names look like
`hocbfun6rvpff89vesk...`. **That is correct**: the file names are scrambled
too. Do not rename, move or delete them by hand; use the commands here.

If a backup fails, the reason is in the log: `journalctl -u ftnbilling-backup`.
If the server's settings file has `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID`,
a failure also sends you a Telegram message.

## 3. What is in the backup, and what is not

In it:

- Billing data: everything in `/var/lib/ftnbilling` (accounts, payments,
  `fleet.json`, `alerts.json`, `outbox.json`, the `metrics/` history and the
  WireGuard peer list in `wg/`).
- Settings, including passwords: `/etc/ftnbilling/` (the `.env` file).
- The WireGuard server keys: `/etc/wireguard/wg0.conf` (routers) and
  `/etc/wireguard/wg-admin.conf` (your admin devices). With them, a rebuilt
  server keeps the same identity and routers and admin devices reconnect on
  their own.
- The web server settings: `/etc/caddy/Caddyfile`.
- When PostgreSQL is in use (`DATABASE_URL` in the settings file): a fresh,
  verified full database dump, `database.dump` (the same kind the 6-hourly
  timer makes). If the dump fails, the Drive backup fails loudly instead of
  silently leaving the database out. The settings file holds `DATA_KEY`:
  without it the stored national IDs cannot be read, so keep the backup
  passphrases safe.
- `MANIFEST.txt`: server name, date, app version and a checksum of every file.

Not in it:

- The program itself (the installer rebuilds it from GitHub).
- The routers' own configuration (each router holds its own; a joined router
  reconnects when the server key and peer list are restored).
- Your domain's DNS and the GitHub token.
- Anything changed after the Sunday run: at worst you lose a week of changes.
  The 6-hourly local dumps cover the days in between while the server is alive.

## 4. Size

A snapshot is the data files plus `metrics/`, compressed. Expect a few MB for
the billing data and more for the metrics history, so roughly tens of MB to a
few hundred MB for a deployment of this size (this is an estimate, not a
measurement from your server). Up to 12 snapshots are kept, so multiply by 12.
Google gives 15 GB free. See the real number any time with
`sudo ftnbilling-backup status` (Total size under "On Drive"), and check
after the first month. To keep fewer weeks, set `BACKUP_KEEP_WEEKS=8` in
`/etc/systemd/system/ftnbilling-backup.service.d/override.conf` (section
`[Service]`, line `Environment=BACKUP_KEEP_WEEKS=8`) and run
`sudo systemctl daemon-reload`.

## 5. Restore drill (do this once, now, not on the day of the disaster)

A backup you have never restored is only a hope. Practise on a throwaway
server (any small VPS) so the live server is not touched.

1. Create a fresh Debian/Ubuntu VPS and run the installer as usual
   (`deploy/README.md`, section 2).
2. Repeat the laptop steps from section 1 (`rclone authorize ...`) to get a
   fresh token line. (The old one is on the old server, which may be gone.)
3. Reconnect to the existing backups with your **saved** passphrases:

   ```
   sudo ftnbilling-backup setup
   ```

   Paste the token line. When it asks "Saved passphrase (or Enter)", paste the
   passphrase, then the salt, that you saved. Do **not** just press Enter:
   that would create new passphrases that cannot open the old backups. The
   test at the end of setup passes with any passphrase, so the real proof is
   the next step.
4. List and download the newest snapshot:

   ```
   sudo ftnbilling-backup list
   sudo ftnbilling-backup restore latest
   ```

   If the passphrase is wrong, `list` shows nothing or `restore` fails. If it
   works, it prints "Checksums in MANIFEST.txt all match" and a folder under
   `/root/ftnbilling-restore-<date>/`. Nothing live is overwritten; the tool
   refuses to restore into `/var/lib/ftnbilling` or `/etc`.
5. Follow the numbered steps it prints (stop the app, copy the data and
   settings back, restore the WireGuard file, start the app). On a real
   rebuild this is also when you point the domain at the new server.
6. Open the admin and check that accounts and payments are there.

For a real disaster, do the same on the replacement server, and run the
installer first so the `ftnbilling` user and folders exist.

## 6. Everyday commands

| Command | What it does |
|---|---|
| `sudo ftnbilling-backup dump` | Database dump + file tars now |
| `sudo ftnbilling-backup drill` | Restore drill now |
| `sudo ftnbilling-backup run` | Drive snapshot right now |
| `sudo ftnbilling-backup list` | Snapshots on Drive |
| `sudo ftnbilling-backup status` | Last result, next run, space used |
| `sudo ftnbilling-backup restore <name or latest> [folder]` | Download into a new folder |
| `systemctl list-timers ftnbilling-backup.timer` | When the next run is |

If Google ever asks you to sign in again or the backups stop working with an
"invalid_grant" or "token" error (for example after a password change or after
removing the app in your Google security settings), repeat the laptop steps
and run `sudo GDRIVE_TOKEN='<new line>' ftnbilling-backup setup`. The
passphrases are kept and old backups stay readable.
