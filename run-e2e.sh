#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
LOG=/tmp/speedbond-e2e.log
READY=/tmp/speedbond-e2e.ready
rm -f "$LOG" "$READY"
app=""
tun=""

cleanup() {
  if [[ -n "$app" ]]; then
    kill -TERM "$app" 2>/dev/null || true
    sleep 1
    if kill -0 "$app" 2>/dev/null; then
      if [[ -n "$tun" ]]; then
        /sbin/route -n delete -net 0.0.0.0/1 -interface "$tun" 2>/dev/null || true
        /sbin/route -n delete -net 128.0.0.0/1 -interface "$tun" 2>/dev/null || true
      fi
      kill -KILL "$app" 2>/dev/null || true
    fi
  fi
  /sbin/route -n delete -host 188.34.194.25 2>/dev/null || true
  /sbin/route -n delete -host 188.34.194.25 -ifscope feth50 2>/dev/null || true
  /sbin/route -n change default 192.168.0.1 2>/dev/null || true
  scutil --allow-new-interfaces off
}
trap cleanup EXIT

pkill -9 -f '^/Users/eminmahrt/Developer/open-speedify/speedbond client -route=false' 2>/dev/null || true
pkill -TERM -f 'io.tailscale.ipn.macsys.network-extension' 2>/dev/null || true
sleep 1
/sbin/ifconfig utun5 down 2>/dev/null || true
/sbin/ifconfig utun9 down 2>/dev/null || true
/sbin/route -n change default 192.168.0.1
/usr/local/bin/rndis-tether config prefer-phone no
/usr/local/bin/rndis-tether connect
for _ in {1..30}; do
  address="$(ipconfig getifaddr feth50 2>/dev/null || true)"
  [[ -n "$address" ]] && break
  sleep 1
done
[[ -n "$address" ]]

./speedbond client \
  -server 188.34.194.25:443 \
  -links en0,feth50 \
  -pin e725e90aa80dbe97733d036c6eebdfef52b98f25488a276b93bd302a449292fa >"$LOG" 2>&1 &
app=$!
for _ in {1..30}; do
  if [[ -f "$LOG" ]]; then
    tun="$(sed -n 's/.*connected: tunnel=\([^ ]*\).*/\1/p' "$LOG" | awk 'NR == 1 {print; exit}')"
    [[ -n "$tun" ]] && break
  fi
  kill -0 "$app" 2>/dev/null
  sleep 1
done
printf 'pid=%s\ntun=%s\n' "$app" "$tun" >"$READY"
sleep 600
