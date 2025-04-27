#!/usr/bin/env bash
set -e
IMAGE=aiexpedite/ttyd-helper:latest

if ! command -v docker >/dev/null 2>&1; then
  echo "Docker is required – install it first."; exit 1
fi

echo "== pulling helper image =="
docker pull "$IMAGE"

echo "== starting container =="
docker run -d --name ai-helper \
  --restart unless-stopped \
  -p 3080:3080 -p 3090:3090 \
  -v ai-helper-data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  "$IMAGE"

echo "All set!  Visit the AI‑Expedite site and approve the connection."