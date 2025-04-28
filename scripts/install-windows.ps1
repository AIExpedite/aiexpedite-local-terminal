# Open Terminal Bridge - Windows installation and launcher

# 1. Install ttyd if not present
if (!(Get-Command ttyd -ErrorAction SilentlyContinue)) {
    Write-Host "ttyd not found. Installing..."
    if (Get-Command winget -ErrorAction SilentlyContinue) {
        winget install -e --id tsl0922.ttyd --source winget
    } elseif (Get-Command scoop -ErrorAction SilentlyContinue) {
        scoop install ttyd
    } else {
        Write-Host "Please download the ttyd Windows binary from GitHub releases and place it in your PATH."
        exit 1
    }
}
# 2. Stop existing ttyd on port 7681 if running
Try {
    Get-Process ttyd -ErrorAction Stop | Stop-Process -Force
    Write-Host "Stopped existing ttyd process on port 7681."
} Catch {
    # no process found or error
}
# 3. Launch ttyd on localhost:7681
Write-Host "Starting ttyd on localhost:7681..."
# Run `ttyd` to launch a new PowerShell shell accessible via web:
ttyd -p 7681 -i 127.0.0.1 -W powershell
