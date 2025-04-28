#!/bin/bash
# Open Terminal Bridge - Linux installation and launcher

# 1. Install ttyd if not present, trying common package managers
if ! command -v ttyd >/dev/null; then
  echo "ttyd not found. Attempting to install via package manager..."
  if [ -f /etc/debian_version ]; then
    sudo apt-get update && sudo apt-get install -y ttyd || true
  elif [ -f /etc/redhat-release ] && command -v dnf >/dev/null; then
    sudo dnf install -y ttyd || true
  elif command -v pacman >/dev/null; then
    sudo pacman -Sy --noconfirm ttyd || true
  elif command -v brew >/dev/null; then
    brew install ttyd || true
  elif command -v snap >/dev/null; then
    sudo snap install ttyd --classic || true
  fi
fi

# If still not installed, prompt user
if ! command -v ttyd >/dev/null; then
  echo "Error: Could not install ttyd via package manager."
  echo "Please install ttyd manually (refer to https://github.com/tsl0922/ttyd/releases)."
  exit 1
fi

# 2. Stop existing ttyd on port 7681 if running
if lsof -i tcp:7681 &>/dev/null; then
  echo "Port 7681 in use. Stopping existing ttyd..."
  pkill -f "ttyd .*7681" 2>/dev/null || killall ttyd 2>/dev/null
fi

# 3. Launch ttyd on localhost:7681
echo "Starting ttyd on localhost:7681..."
ttyd -p 7681 -i 127.0.0.1 -W bash
