# Sign-Binary.ps1
#
# Signs one or more Windows binaries with the EV Code Signing certificate
# stored on the YubiKey (PIV slot 9a, surfaced via the Windows certificate
# store by the YubiKey Smart Card Minidriver + Certificate Propagation).
#
# Used by:
#   - Local: pwsh Sign-Binary.ps1 -Path .\dist\foo.exe
#   - CI:    invoked by .github/workflows/release-prod.yml on the self-hosted runner
#
# Requires:
#   - signtool.exe (Windows 10/11 SDK)
#   - YubiKey plugged in
#   - YubiKey Smart Card Minidriver installed (Yubico.YubiKeySmartCardMinidriver)
#   - WINDOWS_CERT_THUMBPRINT env var OR -Thumbprint param
#
# PIN prompt: when signtool first touches the private key, Windows pops the
# YubiKey PIN dialog. PIV PIN policy 'once' (set by Setup-YubiKey.ps1) caches
# the PIN for this signtool process tree, so passing multiple files in one
# call only prompts once.

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string[]]$Path,

    [string]$Thumbprint = $env:WINDOWS_CERT_THUMBPRINT,

    [string]$TimestampUrl = "http://ts.ssl.com",

    [string]$Description = "AI Expedite Terminal",

    [string]$DescriptionUrl = "https://aiexpedite.com"
)

$ErrorActionPreference = "Stop"

if (-not $Thumbprint) {
    throw "WINDOWS_CERT_THUMBPRINT not set. Pass -Thumbprint or set the env var."
}
$Thumbprint = $Thumbprint -replace '\s', '' -replace ':', ''

# Resolve files
$resolved = @()
foreach ($p in $Path) {
    $items = Get-Item -Path $p -ErrorAction Stop
    foreach ($i in $items) { $resolved += $i.FullName }
}
if ($resolved.Count -eq 0) { throw "No files matched: $($Path -join ', ')" }

# Locate signtool.exe (latest Windows SDK x64)
$signtool = $null
$sdkRoots = @(
    "${env:ProgramFiles(x86)}\Windows Kits\10\bin",
    "${env:ProgramFiles}\Windows Kits\10\bin"
) | Where-Object { Test-Path $_ }

foreach ($root in $sdkRoots) {
    $candidate = Get-ChildItem -Path $root -Recurse -Filter signtool.exe -ErrorAction SilentlyContinue |
        Where-Object { $_.FullName -match '\\x64\\signtool\.exe$' } |
        Sort-Object FullName -Descending |
        Select-Object -First 1
    if ($candidate) { $signtool = $candidate.FullName; break }
}
if (-not $signtool) {
    $signtool = (Get-Command signtool.exe -ErrorAction SilentlyContinue)?.Source
}
if (-not $signtool) {
    throw "signtool.exe not found. Install the Windows 10/11 SDK (Signing Tools component)."
}
Write-Host "signtool: $signtool" -ForegroundColor DarkGray

# Verify the cert is actually present before prompting the user
$cert = Get-ChildItem Cert:\CurrentUser\My |
    Where-Object { $_.Thumbprint -eq $Thumbprint }
if (-not $cert) {
    throw @"
Certificate with thumbprint $Thumbprint not found in CurrentUser\My.
Is the YubiKey plugged in? Try:
  - Unplug / replug the YubiKey
  - Restart-Service CertPropSvc -Force   (in elevated shell)
  - Get-ChildItem Cert:\CurrentUser\My | ft Thumbprint, Subject
"@
}
Write-Host "Certificate: $($cert.Subject)" -ForegroundColor DarkGray
Write-Host "Expires:     $($cert.NotAfter)" -ForegroundColor DarkGray

Write-Host ""
Write-Host ">>> If a YubiKey PIN dialog pops up, enter your PIN. <<<" -ForegroundColor Yellow
Write-Host ""

# Sign all files in a single signtool invocation - one PIN prompt for the whole batch.
$signArgs = @(
    "sign"
    "/sha1", $Thumbprint
    "/fd",   "sha256"
    "/tr",   $TimestampUrl
    "/td",   "sha256"
    "/d",    $Description
    "/du",   $DescriptionUrl
    "/v"
) + $resolved

& $signtool @signArgs
if ($LASTEXITCODE -ne 0) {
    throw "signtool sign failed with exit code $LASTEXITCODE"
}

# Verify
Write-Host ""
Write-Host "Verifying signatures..." -ForegroundColor Cyan
foreach ($f in $resolved) {
    & $signtool verify /pa /v $f
    if ($LASTEXITCODE -ne 0) {
        throw "signtool verify failed for $f"
    }
}

Write-Host ""
Write-Host "Signed and verified $($resolved.Count) file(s)." -ForegroundColor Green
