# macOS Code Signing & Notarization

The GitHub Actions workflows sign the macOS `.dmg` with an Apple **Developer ID
Application** certificate and notarize it with Apple's notary service. Without
this, users get "unidentified developer" / "app is damaged" Gatekeeper errors
on macOS 10.15+.

## One-time setup

### 1. Create a Developer ID Application certificate (on a Mac)

1. Open **Keychain Access** → **Certificate Assistant** → **Request a
   Certificate From a Certificate Authority…**
2. Enter your email (`daniel@aiexpedite.com`), leave CA Email blank, choose
   **Saved to disk**, click Continue. Save the `.certSigningRequest` file.
3. Go to https://developer.apple.com/account → **Certificates, IDs & Profiles**
   → **Certificates** → **+** button.
4. Choose **Developer ID Application** (under "Software"). Continue.
5. Upload the `.certSigningRequest` file. Download the resulting `.cer` file.
6. Double-click the `.cer` file — Keychain Access imports it and pairs it with
   the private key you created in step 2.
7. In Keychain Access, find the cert named
   `Developer ID Application: AI Expedite Inc. (TEAMID)`. Right-click → **Export**
   → save as `DeveloperID.p12`, set a strong password (you'll need it below).

> **No Mac handy?** You can generate the CSR with OpenSSL on any machine:
> ```bash
> openssl genrsa -out devid.key 2048
> openssl req -new -key devid.key -out devid.csr \
>   -subj "/emailAddress=daniel@aiexpedite.com/CN=Daniel Kupisz/C=US"
> ```
> Upload `devid.csr` to Apple. After downloading `developerID_application.cer`,
> bundle into a `.p12`:
> ```bash
> openssl x509 -inform DER -in developerID_application.cer -out devid.pem
> openssl pkcs12 -export -inkey devid.key -in devid.pem -out DeveloperID.p12
> ```

### 2. Find your Apple Team ID

Go to https://developer.apple.com/account → **Membership details** (top
navigation). The **Team ID** is a 10-character string (e.g. `ABCDE12345`).

### 3. Create an app-specific password for notarization

`notarytool` needs an app-specific password — you cannot use your main Apple
ID password.

1. Go to https://account.apple.com → **Sign-In and Security** → **App-Specific
   Passwords** → **Generate an app-specific password**.
2. Label it `AI Expedite Terminal Notarization`. Copy the password (format:
   `abcd-efgh-ijkl-mnop`).

### 4. Base64-encode the `.p12` for GitHub

```bash
# On Mac / Linux
base64 -i DeveloperID.p12 -o DeveloperID.p12.base64

# On Windows (PowerShell)
[Convert]::ToBase64String([IO.File]::ReadAllBytes("DeveloperID.p12")) | Out-File -Encoding ASCII DeveloperID.p12.base64
```

### 5. Add GitHub repository secrets

Go to https://github.com/AIExpedite/aiexpedite-local-terminal/settings/secrets/actions
and add these secrets:

| Secret name | Value |
|---|---|
| `MACOS_CERTIFICATE` | Contents of `DeveloperID.p12.base64` |
| `MACOS_CERTIFICATE_PASSWORD` | Password you set when exporting the `.p12` |
| `MACOS_KEYCHAIN_PASSWORD` | Any random string — used for the temporary CI keychain only |
| `MACOS_SIGNING_IDENTITY` | `Developer ID Application: AI Expedite Inc. (TEAMID)` — exactly as shown in Keychain Access |
| `MACOS_NOTARIZATION_APPLE_ID` | `daniel@aiexpedite.com` |
| `MACOS_NOTARIZATION_TEAM_ID` | The 10-character Team ID from step 2 |
| `MACOS_NOTARIZATION_PWD` | The app-specific password from step 3 |

### 6. Test it

Push a commit to `main` — the `release-nonprod` workflow's darwin jobs will
sign and notarize. Download the resulting `.dmg`, open on macOS, and the app
should launch with no Gatekeeper warning.

Verify the signature manually:
```bash
spctl --assess --type execute --verbose /Applications/AIExpediteTerminal.app
# Should say: "accepted source=Notarized Developer ID"
```

## What the workflow does

1. **Import cert** — decodes `MACOS_CERTIFICATE` into a temporary keychain.
2. **Code sign** — signs the binary and `.app` bundle with hardened runtime
   (`--options runtime`), a secure timestamp, and `build/entitlements.mac.plist`.
3. **Package DMG** — builds the `.dmg` around the signed `.app`.
4. **Sign DMG** — signs the DMG itself (required for notarization).
5. **Notarize** — uploads to Apple's notary service, waits for approval
   (usually 1–5 minutes, up to 30-minute timeout).
6. **Staple** — attaches the notarization ticket to the DMG so Gatekeeper can
   verify offline.
7. **Cleanup** — deletes the temporary keychain.

## How the DMG installs

The DMG deliberately contains **only the app** — no `/Applications` shortcut to
drag it onto. A machine-wide `/Applications` copy needs an admin prompt and
cannot be replaced silently by the auto-updater, so the supported install
location is the user-owned `~/Applications`.

Double-clicking the app on the mounted image *is* the installation:
`installDarwinFromMedia` (`relocate_darwin.go`) copies the bundle into
`~/Applications`, clears the quarantine attribute, writes the login
LaunchAgent, launches the installed copy, posts a notification saying where it
went, and ejects the image. macOS App Translocation is handled too — a
quarantined app launched from a DMG reports a randomized
`.../AppTranslocation/<UUID>/d/<App>.app` path rather than `/Volumes/...`.

Guards keep that from firing when it shouldn't:

- **It must be a disk image.** A `/Volumes/...` path alone proves nothing (USB
  sticks and network shares mount there too), so the volume must appear as an
  attached image in `hdiutil info`. Without that proof the app just runs where
  it is.
- **A running agent owns its install.** The replacement takes the per-account
  singleton first, because the installed agent's own updater swaps the same
  bundle from `applyVerifiedUpdate`. If another agent holds it, the install is
  skipped with a notification asking the user to quit it first — otherwise an
  older DMG could overwrite a verified update after its rollback copy is gone.
  Without that lock the install may only *create*: it lands via
  `renameatx_np(RENAME_EXCL)` so a launcher that won the race keeps its bundle.
- **Another channel is never overwritten.** All four channels package the app
  as `AIExpediteTerminal.app` while being otherwise separate (own bundle id,
  config dir, singleton, LaunchAgent label), so the shared name is the one
  place a dev image could eat a prod install — across a lock it does not even
  contend for. The destination's `CFBundleIdentifier` must match the image's;
  otherwise the install goes to a channel-scoped name
  (`AIExpediteTerminal-Dev.app`) and both coexist. Unreadable identity counts
  as foreign, and if every candidate path is foreign nothing is installed at
  all. Giving each channel its own packaged bundle name in the release
  workflows would make the collision impossible in the first place; the guard
  holds either way.
- **Installers serialize on the destination.** Each channel holds a *different*
  singleton, so it cannot order two installers against each other. A lock file
  (`~/Applications/.aixinstall.lock`) does: every installer takes it before
  choosing a path, and placement is create-first
  (`renameatx_np(RENAME_EXCL)`) with ownership re-read immediately before any
  swap, because the decision made before the copy is stale by then.
- **A failed launch does not roll back blindly.** `open` can report an error
  after the replacement started. The installer retakes the singleton to decide:
  if it can, nothing else is running and the previous bundle is restored; if it
  cannot, the replacement owns the install and the new bundle stays.

The DMG window background (`assets/dmg-background.png`) is generated by
`scripts/generate-dmg-background.py`; its text is positioned against the
`icon_locations` / `window_rect` values in the "Package as DMG" workflow step,
so change both together.

## Entitlements

`build/entitlements.mac.plist` enables the hardened runtime exceptions needed
for the terminal (JIT, unsigned executable memory, library validation,
AppleScript automation). Trim these if you don't need them — smaller
entitlements = safer app.

## Rotating the certificate

Apple Developer ID certs expire after 5 years. When yours expires:

1. Repeat steps 1 and 4 above with a new cert.
2. Update `MACOS_CERTIFICATE`, `MACOS_CERTIFICATE_PASSWORD`, and
   `MACOS_SIGNING_IDENTITY` secrets.

Old releases stay valid because the notarization ticket is stapled — Apple's
notary infrastructure vouches for them independently of your cert's status.

## Troubleshooting

**`errSecInternalComponent` during `codesign`** — usually means the keychain
partition list wasn't configured. The workflow runs
`security set-key-partition-list` to fix this. If it still fails, check that
`MACOS_KEYCHAIN_PASSWORD` is set.

**`Invalid credentials` during notarization** — re-generate the app-specific
password at account.apple.com. Make sure you're using the **Team ID**
(not the Apple ID) in `MACOS_NOTARIZATION_TEAM_ID`.

**`The signature of the binary is invalid`** — the inner binary wasn't signed
before the outer `.app` bundle. The workflow signs both in order
(binary → bundle).

**Notarization hangs** — check https://developer.apple.com/system-status/ for
notary service outages. 30-minute timeout is our limit; Apple's SLA is usually
under 5 minutes.

**Users still see "app is damaged"** — their download may be truncated.
Confirm the DMG's SHA-256 matches `SHA256SUMS` in the GitHub release.
