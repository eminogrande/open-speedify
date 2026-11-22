#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"

phone=""
for dev in feth50 en4 en5 en6; do
  if ipconfig getifaddr "$dev" >/dev/null 2>&1; then
    phone="$dev"
    break
  fi
done
if [[ -z "$phone" ]]; then
  echo "No Android USB-tethering interface found. Enable USB tethering on the phone, then rerun."
  exit 1
fi

export SPEEDBOND_TOKEN="$(security find-generic-password -a "$USER" -s open-speedify-relay-token -w)"
LOG=/tmp/open-speedify-client.log
: >"$LOG"
app=""
tun=""
cleanup() {
  if [[ -n "$app" ]]; then
    sudo kill -TERM "$app" 2>/dev/null || true
    sleep 1
    if sudo kill -0 "$app" 2>/dev/null; then
      if [[ -n "$tun" ]]; then
        sudo route -n delete -net 0.0.0.0/1 -interface "$tun" 2>/dev/null || true
        sudo route -n delete -net 128.0.0.0/1 -interface "$tun" 2>/dev/null || true
      fi
      sudo kill -KILL "$app" 2>/dev/null || true
    fi
  fi
}
trap cleanup EXIT

if [[ "$phone" == "feth50" ]]; then
  rndis-tether config prefer-phone no
fi
sudo pkill -9 -f '^/Users/eminmahrt/Developer/open-speedify/speedbond client -route=false' 2>/dev/null || true
echo "Starting Wi-Fi (en0) + Android ($phone). Press Ctrl-C to stop."
sudo -E ./speedbond client \
  -server 188.34.194.25:443 \
  -links "en0,$phone" \
  -pin e725e90aa80dbe97733d036c6eebdfef52b98f25488a276b93bd302a449292fa >"$LOG" 2>&1 &
app=$!

sleep 2
if [[ "$phone" == "feth50" ]]; then
  rndis-tether disconnect 2>/dev/null || true
  rndis-tether connect
fi
for _ in {1..30}; do
  tun="$(sed -n 's/.*connected: tunnel=\([^ ]*\).*/\1/p' "$LOG" | awk 'NR == 1 {print; exit}')"
  [[ -n "$tun" ]] && break
  kill -0 "$app" 2>/dev/null
  sleep 1
done
if [[ -z "$tun" ]]; then
  cat "$LOG"
  exit 1
fi
echo "Bonded tunnel is running on $tun."
wait "$app"
