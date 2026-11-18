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

A tag `android-vX.Y.Z` builds a signed APK and publishes `ftn-admin-X.Y.Z.apk` and `.sha256`
as a GitHub Release marked `--latest=false`. This matters: `deploy/install.sh` installs the server
from `releases/latest`, so an Android release must never become "latest". The server workflow
(`release.yml`) passes `--latest` for the same reason.

Create the keystore once and add the four secrets:

```
keytool -genkeypair -v -keystore ftnadmin-release.jks -alias ftnadmin \
  -keyalg RSA -keysize 4096 -validity 10000
base64 -w0 ftnadmin-release.jks | gh secret set ANDROID_KEYSTORE_B64
gh secret set ANDROID_KEYSTORE_PASSWORD
gh secret set ANDROID_KEY_ALIAS          # ftnadmin
gh secret set ANDROID_KEY_PASSWORD
```

Back the keystore up offline. If it is lost, the only way forward is to uninstall the app on every
phone and import the tunnels again, because Android refuses updates signed with a different key.
Without the secrets the release job fails immediately with a message naming what is missing, and
local `assembleRelease` produces an unsigned APK.

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
- [ ] CSV export asks where to save and the file opens with the right rows (building cookie respected).
- [ ] Printing vouchers opens the system print dialog (top bar menu, Print).
- [ ] Change the admin password in the web UI: the app asks to sign in; it does not lock you out.
- [ ] Web logout: the app disconnects and locks (if app lock is on).
- [ ] `ftnbilling-admin-peer remove <name>`, reconnect: "No handshake" message, no credentials sent.
- [ ] Start another VPN app: the tunnel drops and the admin screen shows the reconnect panel.
- [ ] App lock: biometric and device PIN; relock after the timeout; wipe offered after repeated failures.
- [ ] Home screen: public and in-tunnel health; fleet card shows "server too old" until the summary
      endpoint exists on the server, then the buildings list.
- [ ] Wipe app data returns to onboarding with the reminder to revoke the device on the server.
