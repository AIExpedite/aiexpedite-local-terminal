#!/bin/sh
TOKEN_FILE=$1

parse() { # jq‑less JWT parse (header.payload.signature)
  echo "$1" | cut -d'.' -f2 | base64 -d 2>/dev/null || true
}

TOKEN=$(cat "$TOKEN_FILE")
USER_ID=$(parse "$TOKEN" | busybox grep -o '"userID":"[^"]*' | cut -d'"' -f4)
WS_ID=$(parse "$TOKEN" | busybox grep -o '"workspaceID":"[^"]*' | cut -d'"' -f4)

export USER_ID WS_ID

# OPA input
cat > /tmp/ai_input.json <<EOF
{ "userID": "$USER_ID", "workspaceID": "$WS_ID" }
EOF

if ! opa eval -i /tmp/ai_input.json -d /policy data.ai.allow | grep -q true; then
  echo "Permission denied by policy engine."; exit 1
fi

exec ${SHELL:-/bin/sh}