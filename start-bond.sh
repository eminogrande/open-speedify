#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"

RELAY="188.34.194.25:443"
PIN="e725e90aa80dbe97733d036c6eebdfef52b98f25488a276b93bd302a449292fa"
ALFA_DIR="/Users/eminmahrt/Developer/alfa-vm"
ALFA_KEY="$ALFA_DIR/id_ed25519"
ALFA_SOCK="$ALFA_DIR/monitor.sock"
ALFA_PORT=4546
LOG=/tmp/open-speedify-client.log

setup_alfa_bridge() {
  [[ -S "$ALFA_SOCK" && -f "$ALFA_KEY" ]] || return 1
  local ssh_opts=(-q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=3 -p 2222 -i "$ALFA_KEY")
  local wlx
  wlx="$(ssh "${ssh_opts[@]}" alfa@127.0.0.1 'ip -4 -o addr show | awk "/wlx/{print \$2; exit}"' 2>/dev/null || true)"
  # Auto-heal Alfa Wi-Fi DHCP after Mac sleep if dongle is present without an IPv4 address
  if [[ -z "$wlx" ]] && ioreg -p IOUSB -w0 2>/dev/null | grep -q '802\.11ac'; then
    local saved_ssid="" saved_pass=""
    if [[ -f "$ALFA_DIR/wifi.env" ]]; then
      # shellcheck disable=SC1090
      source "$ALFA_DIR/wifi.env" 2>/dev/null || true
      saved_ssid="${SSID:-}"
      saved_pass="${PASS:-}"
    fi
    if [[ -n "$saved_ssid" ]]; then
      "$ALFA_DIR/alfa.sh" wifi "$saved_ssid" "$saved_pass" >/dev/null 2>&1 || {
        "$ALFA_DIR/alfa.sh" usb-off >/dev/null 2>&1 || true
        sleep 1
        "$ALFA_DIR/alfa.sh" usb-on >/dev/null 2>&1 || true
        sleep 2
        "$ALFA_DIR/alfa.sh" wifi "$saved_ssid" "$saved_pass" >/dev/null 2>&1 || true
      }
      wlx="$(ssh "${ssh_opts[@]}" alfa@127.0.0.1 'ip -4 -o addr show | awk "/wlx/{print \$2; exit}"' 2>/dev/null || true)"
    fi
  fi
  [[ -n "$wlx" ]] || return 1

  # Deploy arm64 binary into VM if missing or outdated (atomic mv avoids ETXTBSY)
  if ! ssh "${ssh_opts[@]}" alfa@127.0.0.1 'test -x ~/speedbond-bridge' 2>/dev/null || \
     [[ "$(stat -f%m ./speedbond-arm64)" -gt "$(ssh "${ssh_opts[@]}" alfa@127.0.0.1 'stat -c%Y ~/speedbond-bridge 2>/dev/null || echo 0')" ]]; then
    scp -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -P 2222 -i "$ALFA_KEY" \
      ./speedbond-arm64 alfa@127.0.0.1:~/speedbond-bridge.new
    ssh "${ssh_opts[@]}" alfa@127.0.0.1 'chmod +x ~/speedbond-bridge.new && mv -f ~/speedbond-bridge.new ~/speedbond-bridge'
  fi

  # Start UDP bridge inside Alfa VM bound directly to wlx interface
  ssh "${ssh_opts[@]}" alfa@127.0.0.1 "
    sudo pkill -f '[s]peedbond-bridge bridge' 2>/dev/null || true
    sudo rm -f /tmp/speedbond-bridge.log
    sleep 0.2
    sudo python3 -c 'import subprocess; subprocess.Popen([\"/home/alfa/speedbond-bridge\",\"bridge\",\"-listen\",\":$ALFA_PORT\",\"-target\",\"$RELAY\",\"-iface\",\"$wlx\"], stdin=subprocess.DEVNULL, stdout=open(\"/var/log/speedbond-bridge.log\",\"wb\"), stderr=subprocess.STDOUT, start_new_session=True)'
  "

  # Add live QEMU UDP hostfwd without rebooting VM
  python3 - "$ALFA_SOCK" "$ALFA_PORT" <<'PY'
import socket, sys, time
sock_path, port = sys.argv[1], sys.argv[2]
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(2)
s.connect(sock_path)
time.sleep(0.1)
try: s.recv(4096)
except Exception: pass
s.sendall(f"hostfwd_remove n0 udp:127.0.0.1:{port}\n".encode())
time.sleep(0.15)
try: s.recv(4096)
except Exception: pass
s.sendall(f"hostfwd_add n0 udp:127.0.0.1:{port}-:{port}\n".encode())
time.sleep(0.2)
try: s.recv(4096)
except Exception: pass
s.close()
PY
  echo "Alfa UDP bridge ready on 127.0.0.1:$ALFA_PORT -> $wlx -> $RELAY" >&2
  return 0
}

PREPARE_ONLY=0
if [[ "${1:-}" == "--prepare" ]]; then
  PREPARE_ONLY=1
  shift
fi

links=()
if [[ $# -gt 0 ]]; then
  IFS=',' read -r -a links <<< "$1"
  for item in "${links[@]}"; do
    if [[ "$item" == alfa=* ]]; then
      setup_alfa_bridge || { echo "Alfa VM bridge setup failed"; exit 1; }
    fi
  done
else
  if setup_alfa_bridge; then
    links+=("alfa=127.0.0.1:$ALFA_PORT")
  fi
  # Auto-detect ALL active physical IPv4 interfaces (Wi-Fi, Android/iPhone USB, Ethernet, extra Wi-Fi)
  while IFS= read -r dev; do
    [[ -n "$dev" ]] && links+=("$dev")
  done < <(./speedbond interfaces | awk '
    $2 !~ /^(utun|awdl|llw|bridge|ap|lo)/ && $0 ~ /[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\// { print $2 }
  ')
fi

if [[ ${#links[@]} -eq 0 ]]; then
  echo "No active links found."
  exit 1
fi

LINKS_CSV="$(IFS=','; echo "${links[*]}")"

if [[ "$PREPARE_ONLY" -eq 1 ]]; then
  TOKEN="$(security find-generic-password -a "$USER" -s open-speedify-relay-token -w)"
  umask 077
  {
    printf 'SPEEDBOND_TOKEN=%q\n' "$TOKEN"
    printf 'SPEEDBOND_LINKS=%q\n' "$LINKS_CSV"
    printf 'SPEEDBOND_SERVER=%q\n' "$RELAY"
    printf 'SPEEDBOND_PIN=%q\n' "$PIN"
  } >/tmp/speedbond-launch.env
  echo "$LINKS_CSV"
  exit 0
fi
export SPEEDBOND_TOKEN="$(security find-generic-password -a "$USER" -s open-speedify-relay-token -w)"

# Pause alfa-autoswitch + disable macOS SOCKS5 during bonded session so browsers use the VPN tunnel
SOCKS_WAS_ON=0
ALFA_PREV_MODE="$(cat /tmp/alfa-mode 2>/dev/null || true)"
if [[ "$ALFA_PREV_MODE" != "off" ]] || networksetup -getsocksfirewallproxy Wi-Fi 2>/dev/null | grep -q "Enabled: Yes"; then
  SOCKS_WAS_ON=1
  echo off > /tmp/alfa-mode 2>/dev/null || true
  networksetup -setsocksfirewallproxystate Wi-Fi off 2>/dev/null || true
fi

: >"$LOG"
app=""
cleanup() {
  if [[ -n "$app" ]]; then
    curl -s -X POST http://127.0.0.1:19454/stop >/dev/null 2>&1 || sudo kill -TERM "$app" 2>/dev/null || true
    for _ in {1..10}; do
      kill -0 "$app" 2>/dev/null || break
      sleep 0.2
    done
    if kill -0 "$app" 2>/dev/null; then
      sudo kill -KILL "$app" 2>/dev/null || true
    fi
  fi
  if [[ "$SOCKS_WAS_ON" -eq 1 ]]; then
    [[ -n "$ALFA_PREV_MODE" ]] && echo "$ALFA_PREV_MODE" > /tmp/alfa-mode 2>/dev/null || true
    networksetup -setsocksfirewallproxystate Wi-Fi on 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "Starting Speedbond with links: $LINKS_CSV"
echo "Run 'bond' in another terminal for the live TUI monitor & speed proof."
sudo -E ./speedbond client \
  -server "$RELAY" \
  -links "$LINKS_CSV" \
  -pin "$PIN" >"$LOG" 2>&1 &
app=$!

for _ in {1..20}; do
  if curl -s http://127.0.0.1:19454/status >/dev/null 2>&1; then
    ./speedbond status
    break
  fi
  if ! kill -0 "$app" 2>/dev/null; then
    cat "$LOG"
    exit 1
  fi
  sleep 0.5
done

wait "$app"
