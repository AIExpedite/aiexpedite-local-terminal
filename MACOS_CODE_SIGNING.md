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
