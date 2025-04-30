param(
  [string]$TokenFile  # passed in by entrypoint.ps1 via ttyd
)

Write-Host "[helper] awaiting initial JWT…"
while (-not (Test-Path $TokenFile) -or ((Get-Item $TokenFile).Length -eq 0)) {
  Start-Sleep -Seconds 1
}

# Read & decode JWT payload
$jwt     = Get-Content $TokenFile -Raw
$payload = ($jwt.Split('.')[1] |
            ForEach-Object { [System.Text.Encoding]::Utf8.GetString([System.Convert]::FromBase64String($_)) }
           ) | ConvertFrom-Json

$userID      = $payload.userID
$workspaceID = $payload.workspaceID

# Prepare OPA input
$tmpInput = "$env:TEMP\ai_input.json"
@"
{ "userID":"$userID", "workspaceID":"$workspaceID" }
"@ | Out-File -Encoding ascii $tmpInput

# Evaluate policy
& .\opa.exe eval -i $tmpInput -d .\policy.rego data.ai.allow | Out-Null
if ($LASTEXITCODE -ne 0) {
  Write-Error "Permission denied by policy engine."
  exit 1
}

# If we get here, policy passed—launch an interactive shell
Write-Host "Policy OK. Dropping into PowerShell…`n"
# -NoLogo / -NoProfile to keep it lean
& powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass
