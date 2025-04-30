# entrypoint.ps1
$packageDir = Split-Path --Parent $MyInvocation.MyCommand.Definition
Push-Location $packageDir

# We’ll write the token file here
$jwtFile = Join-Path $packageDir 'current.jwt'

# Start ttyd, pointing it at our helper-shell and JWT file
Write-Host "Starting AI-Expedite terminal helper (ttyd)…`n"
& .\ttyd.exe `
   -p 3080 `
   -i 127.0.0.1 `
   --once `
   powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File helper-shell.ps1 $jwtFile

Pop-Location
