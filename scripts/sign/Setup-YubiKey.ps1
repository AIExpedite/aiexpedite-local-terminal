# Setup-YubiKey.ps1
#
# Interactive walkthrough for provisioning the SSL.com EV Code Signing
# certificate onto a YubiKey. Run this ONCE on the machine you'll use as the
# self-hosted GitHub Actions runner.
#
# Prerequisites:
#   1. YubiKey 5 series plugged in
#   2. YubiKey Manager installed: https://www.yubico.com/support/download/yubikey-manager/
#      (puts ykman.exe at "C:\Program Files\Yubico\YubiKey Manager\ykman.exe")
#   3. SSL.com order at "validated" status with attestation page open
#
# Reference: https://www.ssl.com/how-to/key-generation-and-attestation-with-yubikey/

param(
    [string]$OutputDir = "$PSScriptRoot\out",
    [ValidateSet("ECCP256", "ECCP384")]
    [string]$Algorithm = "ECCP384",
    # RFC 4514 distinguished name (ykman requirement). Note the comma separators.
    [string]$Subject = "CN=AI Expedite Inc,O=AI Expedite Inc,C=CA,ST=Ontario,L=Toronto"
)

$ErrorActionPreference = "Stop"

function Write-Step($n, $msg) {
    Write-Host ""
    Write-Host "==========================================================" -ForegroundColor Cyan
    Write-Host "  Step $n. $msg" -ForegroundColor Cyan
    Write-Host "==========================================================" -ForegroundColor Cyan
}

function Wait-ForUser($msg = "Press ENTER when ready to continue") {
    Write-Host ""
    Read-Host $msg | Out-Null
}

# Locate ykman.exe
$ykman = $null
$candidates = @(
    "C:\Program Files\Yubico\YubiKey Manager CLI\ykman.exe",
    "C:\Program Files (x86)\Yubico\YubiKey Manager CLI\ykman.exe",
    "C:\Program Files\Yubico\YubiKey Manager\ykman.exe",
    "C:\Program Files (x86)\Yubico\YubiKey Manager\ykman.exe",
    "$env:LOCALAPPDATA\Programs\Yubico\YubiKey Manager\ykman.exe"
)
foreach ($c in $candidates) {
    if (Test-Path $c) { $ykman = $c; break }
}
if (-not $ykman) {
    $ykman = (Get-Command ykman.exe -ErrorAction SilentlyContinue)?.Source
}
if (-not $ykman) {
    Write-Error "ykman.exe not found. Install YubiKey Manager from https://www.yubico.com/support/download/yubikey-manager/"
}
Write-Host "Using ykman: $ykman" -ForegroundColor Green

New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
$csrPath           = Join-Path $OutputDir "csr.pem"
$attestationPath   = Join-Path $OutputDir "attestation.crt"
$intermediatePath  = Join-Path $OutputDir "intermediate.crt"
$issuedCertPath    = Join-Path $OutputDir "issued.crt"

# Step 0 - Sanity check the YubiKey is detected
Write-Step 0 "Detecting YubiKey"
& $ykman info
Wait-ForUser "If the YubiKey above is the right one, press ENTER. Otherwise Ctrl+C and unplug others."

# Step 1 - PIN / PUK / Management Key
Write-Step 1 "PIN, PUK, and Management Key"
Write-Host @"
The PIV applet on the YubiKey has three secrets:
  - PIN           (6-8 digits, used at sign-time)
  - PUK           (8 digits, used to reset the PIN if you forget it)
  - Management Key (used to write certificates / change config)

Defaults are PIN=123456, PUK=12345678, Mgmt Key=010203...0708 (24 bytes hex).
You MUST change these. Choose strong values, store in a password manager.

If you have ALREADY set them, just press ENTER below to skip.
Otherwise type 'set' to run the change commands.
"@ -ForegroundColor Yellow
$choice = Read-Host "Type 'set' to change PIN/PUK/Mgmt Key, or ENTER to skip"
if ($choice -eq "set") {
    Write-Host "`nChanging PIN..." -ForegroundColor Cyan
    & $ykman piv access change-pin
    Write-Host "`nChanging PUK..." -ForegroundColor Cyan
    & $ykman piv access change-puk
    Write-Host "`nChanging Management Key (use --generate --protect to store a random key encrypted by your PIN)..." -ForegroundColor Cyan
    & $ykman piv access change-management-key --generate --protect
}

# Step 2 - PIN policy = once (one PIN per signing session)
Write-Step 2 "Generate key pair and CSR in PIV slot 9a"
Write-Host "Algorithm: $Algorithm   Subject: $Subject" -ForegroundColor Yellow
Write-Host "PIN policy: ONCE (PIN cached for the signtool session - one PIN entry signs both .exe and installer)" -ForegroundColor Yellow
Write-Host ""
Write-Host "About to overwrite slot 9a if a key already exists. Continue?" -ForegroundColor Red
Wait-ForUser

& $ykman piv keys generate `
    --algorithm $Algorithm `
    --pin-policy once `
    --touch-policy never `
    9a "$OutputDir\public-key.pem"

& $ykman piv certificates request `
    --subject $Subject `
    9a "$OutputDir\public-key.pem" $csrPath

Write-Host "`nCSR written to: $csrPath" -ForegroundColor Green

# Step 3 - Attestation
Write-Step 3 "Generate attestation + intermediate certificate"
& $ykman piv keys attest 9a $attestationPath
& $ykman piv certificates export f9 $intermediatePath
Write-Host "Attestation:  $attestationPath" -ForegroundColor Green
Write-Host "Intermediate: $intermediatePath" -ForegroundColor Green

# Step 4 - Submit to SSL.com
Write-Step 4 "Submit attestation to SSL.com"
Write-Host @"
1. Open your SSL.com order page (the cert order with status 'validated').
2. Under 'attestation' click 'manage'.
3. Open both files in Notepad and paste the FULL contents (BEGIN/END lines included):
     - $attestationPath   -> 'attestation certificate' field
     - $intermediatePath  -> 'intermediate certificate' field
4. Click Submit. You should see a green 'verification successful' alert.
5. SSL.com will then issue your end-entity certificate. Download it
   (usually as 'certificate.crt' or '<order-id>.crt').
6. Save the downloaded cert to: $issuedCertPath

When the file exists at that path, press ENTER to continue.
"@ -ForegroundColor Yellow

while (-not (Test-Path $issuedCertPath)) {
    Read-Host "Waiting for $issuedCertPath - press ENTER to re-check" | Out-Null
}

# Step 5 - Import issued cert into slot 9a
Write-Step 5 "Import issued certificate into slot 9a"
& $ykman piv certificates import 9a $issuedCertPath
Write-Host "Imported." -ForegroundColor Green

# Step 6 - Verify Windows can see the cert
Write-Step 6 "Verify Windows certificate store sees the YubiKey cert"
Write-Host "Restarting Certificate Propagation service so the cert appears in CurrentUser\My..." -ForegroundColor Yellow
try { Restart-Service -Name CertPropSvc -Force -ErrorAction Stop } catch { Write-Warning "CertPropSvc restart failed (may need elevated shell): $_" }
Start-Sleep -Seconds 3

$cert = Get-ChildItem Cert:\CurrentUser\My |
    Where-Object { $_.Subject -match "AI Expedite" -and $_.HasPrivateKey } |
    Sort-Object NotAfter -Descending |
    Select-Object -First 1

if (-not $cert) {
    Write-Warning "Cert not found in CurrentUser\My yet. Try unplugging and re-plugging the YubiKey, then run:"
    Write-Host "  Get-ChildItem Cert:\CurrentUser\My | Where-Object Subject -match 'AI Expedite'" -ForegroundColor Cyan
    exit 1
}

Write-Host ""
Write-Host "==========================================================" -ForegroundColor Green
Write-Host "  YubiKey provisioning complete." -ForegroundColor Green
Write-Host "==========================================================" -ForegroundColor Green
Write-Host ""
Write-Host "Subject:    $($cert.Subject)"
Write-Host "Issuer:     $($cert.Issuer)"
Write-Host "Not after:  $($cert.NotAfter)"
Write-Host "Thumbprint: $($cert.Thumbprint)" -ForegroundColor Yellow
Write-Host ""
Write-Host "NEXT STEPS:" -ForegroundColor Cyan
Write-Host "  1. Add this thumbprint as a GitHub repository VARIABLE (not secret) named:"
Write-Host "       WINDOWS_CERT_THUMBPRINT = $($cert.Thumbprint)" -ForegroundColor Yellow
Write-Host "     (https://github.com/AIExpedite/aiexpedite-local-terminal/settings/variables/actions)"
Write-Host ""
Write-Host "  2. Register this Windows machine as a self-hosted GitHub Actions runner with labels:"
Write-Host "       self-hosted, Windows, X64, yubikey" -ForegroundColor Yellow
Write-Host "     (https://github.com/AIExpedite/aiexpedite-local-terminal/settings/actions/runners/new)"
Write-Host "     IMPORTANT: run the runner via run.cmd in an interactive console - NOT as a Windows"
Write-Host "     service - otherwise the YubiKey PIN dialog cannot appear."
Write-Host ""
Write-Host "  3. Test signing locally now:"
Write-Host "       pwsh $PSScriptRoot\Sign-Binary.ps1 -Path C:\path\to\some.exe" -ForegroundColor Yellow
