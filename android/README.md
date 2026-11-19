# FTN Admin (Android)

Native shell for the ftnbilling admin: it imports a WireGuard config from `ftnbilling-admin-peer`,
brings up an embedded tunnel (wireguard-android `GoBackend`, no separate WireGuard app) that routes
only the admin network `10.98.0.0/24`, and shows the admin at `http://10.98.0.1:8080/admin` in a
hardened WebView, with a native home screen for tunnel, server health and fleet status.

Package `ke.co.ftw.ftnadmin`, minSdk 26, targetSdk 36. Single module `app`, Kotlin, Compose,
Material 3, manual DI (`AppContainer`).

## Build

CI is the reference build (`.github/workflows/android.yml`: lint, unit tests, `assembleDebug`).
The Gradle wrapper jar is not committed; CI uses a pinned Gradle (9.3.1). Locally, open `android/` in
Android Studio (it generates the wrapper) or run:

```
gradle -p android lint testDebugUnitTest assembleDebug
```

Needs JDK 17. The debug APK is `android/app/build/outputs/apk/debug/app-debug.apk`
(application id `ke.co.ftw.ftnadmin.debug`, installs next to a release build).

If the toolchain ever fails to resolve, the fallback is AGP 8.13.x, Gradle 8.14.x,
`org.jetbrains.kotlin.android` plus the Compose plugin at Kotlin 2.2.21.

## Releases

A release is a signed APK published as a GitHub Release that is never marked "latest":
`deploy/install.sh` installs the server from `releases/latest`, so an Android release must not
become "latest" (the server workflow `release.yml` passes `--latest` for the same reason).
Each release has three assets: `ftn-admin-X.Y.Z.apk`, `ftn-admin-X.Y.Z.apk.sha256` and
`update.json` (read by the in-app update check, see below).

**Secret.** The signing key lives in this repository, encrypted: `android/signing/ftn-admin-signing.enc`
(AES-256-CBC, PBKDF2, 600000 iterations; a tar of `release.jks` and `signing.env`). The only secret
is its passphrase, stored as the repository secret `ANDROID_SIGNING_PASSPHRASE`
(Settings > Secrets and variables > Actions). Without it the release job stops at once with a message
naming it. The job also refuses to publish unless the APK is signed with the expected certificate
(SHA-256 `B9:4C:E4:0D:7A:66:F0:7E:B3:FB:CC:56:F0:FE:9B:EA:1E:D6:CF:5A:46:B1:01:13:D7:F0:6C:96:FA:42:89:C4`).

**Cut a release from the web (no git needed).**

1. GitHub > Actions > **android** > **Run workflow**.
2. Branch `main`, `version` = for example `0.2.0` (digits `X.Y.Z`). The workflow creates the tag
   `android-v0.2.0` and the release. Leave `version` empty to only run a build.
3. Wait for the `release` job. Phones with "Check for updates automatically" on offer the update
   within about six hours; More > App settings > Check for updates does it at once.

Pushing a tag `android-vX.Y.Z` does the same. `versionName` comes from the tag; `versionCode` is the
workflow run number, so every release is higher than the one before (Android needs that to update).

**Key backup.** Keep both halves: the encrypted file (it is in the repo, so it is backed up with it;
also keep a copy elsewhere) and the passphrase in a password manager. If the passphrase is lost
the key cannot be decrypted, no further updates can be built, and installed apps cannot be updated:
Android refuses an APK signed with a different key, so every phone would have to uninstall the app
and import the tunnels again. Never commit the decrypted `release.jks` or `signing.env`.
Local `assembleRelease` without the key produces an unsigned APK.

**In-app update check.** Release builds read `https://api.github.com/repos/blacksan/ftnbilling/releases`,
take the newest published release whose tag starts with `android-v` (server releases `v*` are ignored),
and read its `update.json`. An update is offered when its `versionCode` is higher than the installed
one. The APK is downloaded with OkHttp only from `github.com`, `objects.githubusercontent.com` and
`release-assets.githubusercontent.com` over https, checked against the size and SHA-256 in
`update.json`, and its signing certificate must equal the installed app's before the system installer
is opened. Debug builds (`.debug` application id) show the section disabled.

## Install (sideload) and verify

```
sha256sum -c ftn-admin-X.Y.Z.apk.sha256
adb install ftn-admin-X.Y.Z.apk      # or open the APK on the phone
```

## Using it

1. On the server: `ftnbilling-admin-peer add <name>` prints a QR code.
2. In the app: Get started, scan the QR (or pick/paste the `.conf`), allow the VPN request,
   optionally save the admin login, optionally turn on the app lock.
3. Revoke a phone with `ftnbilling-admin-peer remove <name>`. "Wipe app data" in Settings removes
   everything from the phone but does not revoke it on the server.

Limitations: Android allows one VPN at a time, so another VPN app disconnects this tunnel (and an
always-on VPN elsewhere blocks it). Aggressive OEM battery savers may kill the process, which drops
the tunnel (it fails closed). Connecting only works while the app is in the foreground.

## Security notes

- The tunnel config and saved login are AES-256-GCM encrypted with an Android Keystore key
  (StrongBox when available) in `noBackupFilesDir`; nothing is backed up or transferred.
  The key is not bound to biometrics, so auto-connect works and enrolling a finger loses nothing.
- Import accepts exactly one peer, only routes inside `10.98.0.0/24` including `10.98.0.1`, and
  drops DNS lines: the app never takes over the device's traffic or resolver.
- Credentials are released to the WebView and to `AdminApi` only while the tunnel is Up (handshake
  seen and `/healthz` answered). With the tunnel down, `10.98.0.1` could route to the local network.
- Stored credentials are offered once per navigation, so a changed password or the server's logout
  (always 401) cannot loop into the login rate limiter (5 failures per IP per 15 minutes).
- The WebView has no JS bridge, no file or content access, no mixed content, answers 403 to any
  host except the admin host, and blocks `javascript:`, `file:`, `intent:` and other-origin links.
- FLAG_SECURE is on by default (toggle in Settings) and always on for import, login and scanner screens.
- Cleartext HTTP is permitted only for `10.98.0.1` (network security config).

## Manual test checklist (needs a phone and a server)

- [ ] Import by QR; import by `.conf` file; import by clipboard. A config routing `0.0.0.0/0` is rejected.
- [ ] VPN consent appears once; Connect reaches "Connected"; Admin loads and shows the dashboard.
- [ ] Submit a POST form (CSRF token accepted).
- [ ] A `confirm()` dialog (for example a delete) shows a native dialog and obeys Cancel/OK.
- [ ] CSV export asks where to save and the file opens with the right rows (router selection respected).
- [ ] Printing vouchers opens the system print dialog (top bar menu, Print).
- [ ] Change the admin password in the web UI: the app asks to sign in; it does not lock you out.
- [ ] Web logout: the app disconnects and locks (if app lock is on).
- [ ] `ftnbilling-admin-peer remove <name>`, reconnect: "No handshake" message, no credentials sent.
- [ ] Start another VPN app: the tunnel drops and the admin screen shows the reconnect panel.
- [ ] App lock: biometric and device PIN; relock after the timeout; wipe offered after repeated failures.
- [ ] Home screen: public and in-tunnel health; fleet card shows "server too old" until the summary
      endpoint exists on the server, then the routers list.
- [ ] Wipe app data returns to onboarding with the reminder to revoke the device on the server.

## Theme

The app follows the owner's standard Android theme (Material 3 Expressive). Code lives in
`app/src/main/kotlin/ke/co/ftw/ftnadmin/ui/theme/` (`Color.kt`, `Shape.kt`, `Type.kt`, `Theme.kt`),
brand `FastTechNetBrand`, entry point `AppTheme(...)`. Screens never hard-code colours, shapes or text
sizes: use `MaterialTheme.colorScheme` / `shapes` / `typography`, and `AppTheme.extended` for status
(tunnel Up = success, Connecting = warning, Error = error). Dynamic colour is off by default and can be
enabled in Settings ("Use wallpaper colours (Android 12+)"). `material3` is pinned to `1.5.0-rc01` in
`gradle/libs.versions.toml` (overrides the BOM). The XML theme uses an AppCompat DayNight parent because
androidx.biometric needs it, and the launch screen uses `androidx.core:core-splashscreen`.
