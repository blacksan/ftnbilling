# FTN Admin (Android)

Native admin app for the ftnbilling server (`https://isp.ftw.co.ke`, REST API `/api/v1`): home
dashboard and live router status, customers and payments, alerts, new devices on a router, router
setup (bootstrap line, checks, reset) and remote WinBox details. It signs in with the admin's
username and password (plus a second factor when the server asks for one); the access and refresh
tokens are kept encrypted with an Android Keystore key, and the phone is listed as a device that an
owner can revoke from the server or from Settings. There is no VPN or WebView in the app.

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
is its passphrase, stored as the secret `ANDROID_SIGNING_PASSPHRASE` of the GitHub
environment `release` (Settings > Environments > release > Environment secrets). The release job runs in that
environment and only for commits that are on `main`. Without it the release job stops at once with a message
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

1. Install the release APK (see above), open the app and sign in with the admin account.
2. Settings: app lock (biometric or screen lock), hiding content from screenshots, theme, signed-in
   devices (revoke), updates, and Sign out (revokes this phone on the server and deletes the session).

## Apartments

An apartment is a building that one or more routers serve (many-to-many); a router is not an apartment.
More > Apartments (and the Apartments chip on Customers) lists them, with flats active/total and online now.
The detail screen shows the routers, the flats and the caretaker and landlord contacts with Call (tel:) and
WhatsApp links; the form creates or edits an apartment and links or unlinks routers. Adding a customer picks
the apartment and unit first (the router is automatic when the apartment has one). A customer can be moved to
another apartment from their page, and New devices shows "Belongs to ..." for a MAC of another apartment with
a confirmed "Move customer here". The old router contacts screen is gone: contacts live on the apartment.

## Security notes

- Tokens and the install id are AES-256-GCM encrypted with an Android Keystore key (StrongBox when
  available) in app storage; backups and device transfer are off (`allowBackup=false`).
- All traffic is HTTPS to the server; cleartext is not permitted and only system certificate
  authorities are trusted (network security config).
- FLAG_SECURE (no screenshots or recents preview) is on by default and toggled in Settings.
- The update check talks only to GitHub with a separate OkHttp client that carries no admin token.

## Manual test checklist (needs a phone and a server)

- [ ] Sign in, then force-stop and reopen: still signed in. Sign out: the phone shows as revoked.
- [ ] App lock: biometric or PIN prompt after the lock timeout.
- [ ] Home shows the fleet and, per router, live numbers; pull to refresh works.
- [ ] Router page shows the tunnel address and the remote WinBox line (or the "No report" line).
- [ ] Router page (owner): "Open WebFig" opens the browser at the admin's step-up page (only when the server has WEBFIG_DOMAIN; operators and viewers do not see it).
- [ ] A release build offers an update when a higher `android-v*` release exists, downloads it,
      and the system installer opens; a debug build shows the update section as switched off.

## Theme

The app follows the owner's standard Android theme (Material 3 Expressive). Code lives in
`app/src/main/kotlin/ke/co/ftw/ftnadmin/ui/theme/` (`Color.kt`, `Shape.kt`, `Type.kt`, `Theme.kt`),
brand `FastTechNetBrand`, entry point `AppTheme(...)`. Screens never hard-code colours, shapes or text
sizes: use `MaterialTheme.colorScheme` / `shapes` / `typography`, and `AppTheme.extended` for status
(tunnel Up = success, Connecting = warning, Error = error). Dynamic colour is off by default and can be
enabled in Settings ("Use wallpaper colours (Android 12+)"). `material3` is pinned to `1.5.0-rc01` in
`gradle/libs.versions.toml` (overrides the BOM). The XML theme uses an AppCompat DayNight parent because
androidx.biometric needs it, and the launch screen uses `androidx.core:core-splashscreen`.
