# Windows Code Signing (YubiKey + SSL.com EV)

The `release-prod` workflow signs `aiexpedite-terminal-windows-amd64.exe` and
`AIExpediteTerminal-Setup.exe` with an **EV Code Signing** certificate from
SSL.com. The private key lives on a **YubiKey 5** in PIV slot `9a` and never
leaves the hardware. Without this, Windows SmartScreen shows a red "publisher
unknown" warning to anyone installing the app.

> EV Code Signing certs (issued under the CA/B Forum Code Signing BRs) **must**
> live on a hardware token. They're not exportable to a `.p12`, and they cannot
> be hosted on a generic CI runner — the YubiKey has to be physically present
> on the machine that signs.

## Architecture

Both `release-prod` (manual) and `release-nonprod` (every push to main) now
target the same self-hosted Windows runner. **prod** signs everything;
**nonprod** signs **beta only** (see "Why beta is signed but dev/stg aren't"
below).

```
┌────────────────────────────────────┐
│  GitHub-hosted runners             │   macOS, Linux builds run on
│  (macos-latest, ubuntu-latest)     │   GitHub-hosted (no Windows key needed)
└────────────────────────────────────┘
              │
              │  every push to main (nonprod) /
              │  workflow_dispatch (prod)
              ▼
┌────────────────────────────────────┐
│  Self-hosted Windows runner        │   runner-group: yubikey-signing
│  (your Windows box, YubiKey in     │   labels: self-hosted, Windows,
│   USB, runner running interactively│           X64, yubikey
│   so signtool can prompt for PIN)  │
│                                    │
│  1. go build -> unsigned .exe      │
│  2. (BETA + PROD)                  │
│     signtool sign .exe ─────────┐  │
│                                 ├──── YubiKey PIN prompt
│  3. ISCC.exe -> installer       │  │
│  4. (BETA + PROD)               │  │
│     signtool sign installer ────┘  │
│                                    │
│  dev/stg builds skip 2 + 4         │
│  Uploads (signed if beta/prod)     │
└────────────────────────────────────┘
              │
              ▼
┌────────────────────────────────────┐
│  release-* / update-latest         │   publishes binaries to
│  (back on ubuntu-latest)           │   GitHub Releases + SLSA attestation
└────────────────────────────────────┘
```

### Why labels alone are sufficient (no runner group needed)

The runner is registered at the **repository** level (via the repo's
**Settings → Actions → Runners → New self-hosted runner** flow), not at the
org level. Repo-scoped runners can ONLY be dispatched to from workflows in
that single repo — no other org repo can match the labels and dispatch a job
here, regardless of label collisions.

If we ever need to share this runner across multiple org repos, promote it
to an org-level runner inside a runner group restricted to selected repos
(`runs-on: { group: yubikey-signing, labels: [...] }`). Until then, the
plain label list is the simpler and equally-restrictive choice.

### Why beta is signed but dev/stg aren't

Same logic as macOS notarization: **beta** is publicly distributed (external
testers download it), so it must not trip SmartScreen. **dev** and **stg** are
internal — testers can click through the warning, and skipping the sign step
on every push to main keeps the PIN-prompt cadence manageable.

## PIN handling on the self-hosted runner

This is the failure mode you'll hit first if you don't plan for it.

The PIV PIN policy on slot `9a` is `once` (set by `Setup-YubiKey.ps1`), which
means the YubiKey caches the PIN **for the lifetime of one signtool process**.
Each separate `signtool` invocation prompts again.

For each signing workflow run, the number of PIN prompts is:

| Workflow                    | Sign steps          | PIN prompts |
|-----------------------------|---------------------|-------------|
| `release-prod`              | 2 (exe + installer) | 2           |
| `release-nonprod` (beta)    | 2 (exe + installer) | 2           |
| `release-nonprod` (dev/stg) | 0                   | 0           |

`release-nonprod` runs on **every push to main**. With beta signing, that's
2 PIN dialogs per push.

### Current setup: option 1 (interactive prompts, accepted)

The current operational choice is **interactive PIN prompts**. The YubiKey
machine is staffed during release windows; missed prompts are tolerated as
a minor annoyance because the failure mode is safe (see below). Cost is low
(no extra config, no money) and the tradeoffs of the alternatives weren't
worth it at current release cadence.

**If this changes** (e.g. release cadence climbs, or unattended automation
becomes important), the alternatives in priority order are:

1. **PIN cache via YubiKey minidriver.** Configure
   `HKLM\SOFTWARE\Yubico\YubiKey\PIVPINCacheTimeoutMinutes` (DWORD) to extend
   the cached-PIN window. Cache the PIN once at runner-session start and
   signtool stops prompting until the timeout. Trades some security for
   unattended operation. Reasonable middle ground.

2. **Cloud HSM** (DigiCert KeyLocker, SignPath.io, Azure Key Vault Premium
   with code-signing cert). Expensive ($50–300/mo) but eliminates the
   physical-token SPOF, the PIN prompts, and the "must run interactively"
   constraint. Multi-runner concurrent signing becomes possible. Recommended
   if signing becomes daily-ops pain or if the YubiKey machine ever needs
   to live somewhere other than a single physical desk.

3. **Switch the slot to PIN policy `never`.** Don't do this. Means anyone
   with physical access to the machine can sign anything.

### What happens if no one's there to enter the PIN?

The signtool step waits up to 60 seconds for the dialog response, then
fails. The job fails with a clear `signtool sign failed with exit code 1`.
The whole release run fails — no artifacts are published, nothing
half-signed ships. `release-nonprod`'s concurrency group cancels in-progress,
so a subsequent push will re-trigger and try again. Safe but annoying.

## One-time setup

### 1. Install prerequisites on the Windows machine

Open an **elevated** PowerShell:

```powershell
# YubiKey Manager (CLI: ykman.exe)
winget install --id Yubico.YubiKeyManager -e

# Windows 10/11 SDK (provides signtool.exe). The "Signing Tools" component
# alone is enough; the full SDK works too.
winget install --id Microsoft.WindowsSDK.10.0.22621 -e

# Inno Setup 6 (probably already installed if you've been building locally)
winget install --id JRSoftware.InnoSetup -e

# Go 1.25+ (likewise)
winget install --id GoLang.Go -e
```

Verify each:
```powershell
ykman --version
& "${env:ProgramFiles(x86)}\Windows Kits\10\bin\10.0.22621.0\x64\signtool.exe" /?
& "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" /?
go version
```

### 2. Provision the YubiKey

Plug in the YubiKey, then run the guided script:

```powershell
pwsh aiexpedite-local-terminal\scripts\sign\Setup-YubiKey.ps1
```

It walks you through:

1. Confirming the right YubiKey is plugged in (`ykman info`)
2. Setting **PIN**, **PUK**, and **Management Key** (skip if already set;
   store the values in a password manager)
3. Generating an **ECCP384** key pair in **PIV slot 9a** with PIN policy `once`
4. Generating the **CSR**, **attestation cert**, and **intermediate cert**
5. Pausing while you paste attestation + intermediate into the SSL.com order
   page (under "attestation" → click "manage")
6. Waiting for you to download the issued cert and save it to
   `scripts\sign\out\issued.crt`
7. Importing the issued cert back into slot 9a (`ykman piv certificates import`)
8. Restarting the Certificate Propagation service so Windows picks up the
   cert in `Cert:\CurrentUser\My`
9. Printing the **SHA1 thumbprint** you'll need below

> If you already did the SSL.com attestation flow with the GUI YubiKey
> Manager and just need the thumbprint, run:
> ```powershell
> Get-ChildItem Cert:\CurrentUser\My |
>     Where-Object Subject -match 'AI Expedite' |
>     Format-List Subject, Thumbprint, NotAfter
> ```

### 3. Add the thumbprint as a GitHub repo variable

Go to https://github.com/AIExpedite/aiexpedite-local-terminal/settings/variables/actions
and add a **repository variable** (not a secret — thumbprints are not
sensitive):

| Variable name | Value |
|---|---|
| `WINDOWS_CERT_THUMBPRINT` | The 40-char hex thumbprint from step 2 |

### 4. Register this machine as a self-hosted runner

1. Go to https://github.com/AIExpedite/aiexpedite-local-terminal/settings/actions/runners/new
2. Choose **Windows / x64**. Follow the on-screen `mkdir` / download / config
   commands.
3. When prompted for **labels**, enter exactly:
   ```
   yubikey
   ```
   (GitHub auto-adds `self-hosted`, `Windows`, `X64`. The workflow targets
   the union: `[self-hosted, Windows, X64, yubikey]`.)
4. **Do not install as a service** — run interactively:
   ```powershell
   .\run.cmd
   ```
   The runner needs a real desktop session so the YubiKey PIN dialog can
   appear. If you install it as a service it runs in session 0 and the PIN
   prompt is invisible / unreachable, and signing hangs forever.

### 5. Test signing locally

With the YubiKey plugged in:

```powershell
$env:WINDOWS_CERT_THUMBPRINT = "<your-thumbprint>"
pwsh aiexpedite-local-terminal\scripts\sign\Sign-Binary.ps1 `
    -Path .\aiexpedite-local-terminal\aiexpedite-terminal.exe
```

Expected: a YubiKey PIN dialog appears, you type the PIN, signtool finishes,
and verification reports `Successfully verified`.

Confirm in Explorer: right-click the .exe → **Properties → Digital
Signatures**. You should see `AI Expedite Inc` with a valid timestamp.

## Releasing a signed prod build

1. Plug in the YubiKey on the runner machine.
2. In the runner terminal, start the runner:
   ```powershell
   cd C:\actions-runner   # or wherever you installed it
   .\run.cmd
   ```
3. Trigger the workflow:
   - GitHub UI: **Actions → release-prod → Run workflow** → enter `vX.Y.Z`
   - Or `gh workflow run release-prod -f version=v0.3.0`
4. Watch the runner terminal. When the **Sign Windows binary** step starts, a
   YubiKey PIN dialog pops up. Type your PIN.
   - Because PIN policy is `once`, the dialog appears **only once** even
     though we sign two files (the .exe and the installer).
5. The job uploads signed artifacts; the rest of the workflow (publish
   release on Linux runner) needs no key access.

## What the workflow does

[`.github/workflows/release-prod.yml`](.github/workflows/release-prod.yml),
job `build-prod`:

1. **Checkout + Go setup** — same as before.
2. **Build** Go binary with `-H=windowsgui` and prod ldflags →
   `dist/aiexpedite-terminal-windows-amd64.exe`.
3. **Copy** the binary to the working name Inno Setup expects
   (`aiexpedite-terminal.exe`).
4. **Sign both .exe files** in one signtool call (`scripts/sign/Sign-Binary.ps1`):
   - `/sha1 ${{ vars.WINDOWS_CERT_THUMBPRINT }}` — find cert by thumbprint
   - `/fd sha256 /td sha256` — SHA-256 file digest + timestamp digest
   - `/tr http://ts.ssl.com` — RFC 3161 timestamp from SSL.com
   - `/d "AI Expedite Terminal"` — description shown in UAC prompts
5. **Build installer** with Inno Setup — embeds the now-signed .exe.
6. **Sign the installer** the same way (one more signtool call, no PIN needed
   because policy `once` cached it).
7. **Upload artifacts** — both files are signed.

## Cost / rate notes

- SSL.com EV Code Signing certs include unlimited signatures.
- Their free timestamp server (`http://ts.ssl.com`) has no rate limit
  documented for normal use; if you ever hit issues, fall back to
  `http://timestamp.digicert.com` or `http://timestamp.sectigo.com`.
- Each release does 2 signtool calls = 2 timestamps. Negligible.

## Troubleshooting

**`Certificate with thumbprint XYZ not found in CurrentUser\My`**
The Certificate Propagation service didn't load the YubiKey cert. Try:
```powershell
Restart-Service -Name CertPropSvc -Force   # elevated shell
# Or: unplug + replug the YubiKey
Get-ChildItem Cert:\CurrentUser\My | Format-List Subject, Thumbprint
```

**signtool hangs forever, no PIN dialog appears**
The runner is installed as a Windows service (session 0) instead of running
interactively. Uninstall the service and start it with `.\run.cmd` in a
desktop session:
```powershell
.\svc.cmd uninstall
.\run.cmd
```

**`SignerSign() failed: 0x8009001D` ("provider DLL failed to initialize")**
YubiKey smart card minidriver isn't loaded. Reinstall YubiKey Manager (it
ships the minidriver), reboot, and reconnect the YubiKey.

**`The timestamp server failed`**
Transient. Re-run the failed step. SSL.com timestamp server occasionally
hiccups; the workflow's signtool call retries internally a few times.

**`SignTool Error: No certificates were found that met all the given criteria`**
Either:
  - `WINDOWS_CERT_THUMBPRINT` repo variable is wrong (whitespace? leading 0
    stripped? wrong cert exported?), or
  - YubiKey isn't plugged in.

Check both, then re-run the workflow. The script trims whitespace and colons
automatically but won't fix a fundamentally wrong thumbprint.

**SmartScreen still shows a warning on user machines**
EV certs get **immediate** SmartScreen reputation (vs. OV/IV which need to
build reputation). If it still warns:
  - Confirm `signtool verify /pa /v file.exe` reports
    `Successfully verified` and shows the certificate chain ending at
    SSL.com EV.
  - The downloaded file may be corrupted — check SHA-256 against
    `SHA256SUMS` in the GitHub release.
  - If the .exe and installer are signed but SmartScreen still warns on a
    fresh download, your cert may not have replicated to Microsoft's
    reputation database yet (rare; takes <24 hours).

## Rotating the certificate

EV Code Signing certs from SSL.com are issued for 1–3 years. When yours is
within ~30 days of expiry:

1. Buy a renewal at SSL.com (you can usually get a discount on the same
   order page).
2. Run `Setup-YubiKey.ps1` again — it generates a fresh key pair in slot
   `9a` (overwriting the expiring one), produces a new CSR + attestation,
   you submit, and import the new cert.
3. Update the `WINDOWS_CERT_THUMBPRINT` repo variable to the new
   thumbprint.

Already-released signed binaries keep working — the timestamp from
`http://ts.ssl.com` proves they were signed when the cert was valid, so
SmartScreen / Authenticode trust them indefinitely.
