#!/bin/sh
set -eu
TOKEN_FILE=/data/current.jwt
mkdir -p /data

# lightweight re-auth endpoint (POST /token)
busybox httpd -f -p 3090 &
cat > /tmp/index.html <<EOF
<!DOCTYPE html><title>ttyd-helper</title>
<p>POST a JWT to /token to re‑authenticate.</p>
EOF
# shellcheck disable=SC2016
( while true; do nc -lp 3090 -e sh -c '
  read method path proto; [ "$method" = "POST" ] && [ "$path" = "/token" ] || exit 0
  len=0; while read line && [ "$line" != $"\r" ]; do
    case "$line" in Content-Length:*) len=${line#*: };; esac
  done
  dd bs=$len count=1 of='$TOKEN_FILE' 2>/dev/null
  printf "HTTP/1.1 204 No Content\r\n\r\n"
'
 done ) &

# wait for first token
echo "[helper] awaiting initial JWT…"
while [ ! -s "$TOKEN_FILE" ]; do sleep 1; done

exec ttyd -p 3080 -i 127.0.0.1 \
  -m 86400 --once --cwd "$HOME" \
  /usr/local/bin/helper-shell.sh "$TOKEN_FILE"