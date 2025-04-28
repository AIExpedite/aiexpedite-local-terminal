#!/bin/bash
# Open Terminal Bridge - macOS installation and launcher

# 1. Check if ttyd is installed; if not, install via Homebrew
if ! command -v ttyd >/dev/null; then
  echo "ttyd not found. Installing via Homebrew..."
  if command -v brew >/dev/null; then
    brew install ttyd || { 
      echo "Homebrew install failed. Please install ttyd manually."; 
      exit 1; 
    }
  else
    echo "Homebrew not found. Please install Homebrew or ttyd manually (e.g. from GitHub releases)."
    exit 1
  fi
fi

# 2. Ensure no existing ttyd is running on port 7681
if lsof -i tcp:7681 &>/dev/null; then
  echo "Port 7681 is in use. Stopping existing ttyd instance..."
  pkill -f "ttyd .*7681" 2>/dev/null || killall ttyd 2>/dev/null
fi

# 3. Launch ttyd on localhost:7681 (writable terminal, not TLS)
echo "Starting ttyd on localhost:7681..."
ttyd -p 7681 -i 127.0.0.1 -W bash
