#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"

ALFA_DIR="${ALFA_DIR:-$HOME/Developer/alfa-vm}"
ALFA_KEY="$ALFA_DIR/id_ed25519"
ALFA_SOCK="$ALFA_DIR/monitor.sock"
ALFA_PORT=4546
LOG=/tmp/open-speedify-client.log

SSH_OPTS=(
  -q -o BatchMode=yes
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
  -o ConnectTimeout=3
  -o IdentitiesOnly=yes
  -o PreferredAuthentications=publickey
  -o PasswordAuthentication=no
  -p 2222 -i "$ALFA_KEY"
)
SCP_OPTS=(
  -q -o BatchMode=yes
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
  -o ConnectTimeout=3
  -o IdentitiesOnly=yes
  -o PreferredAuthentications=publickey
  -o PasswordAuthentication=no
  -P 2222 -i "$ALFA_KEY"
)

pick_relay() {
  # Explicit env override (optional)
  if [[ -n "${SPEEDBOND_SERVER:-}" && -n "${SPEEDBOND_PIN:-}" && -n "${SPEEDBOND_TOKEN:-}" ]]; then
    RELAY="$SPEEDBOND_SERVER"
    PIN="$SPEEDBOND_PIN"
    TOKEN="$SPEEDBOND_TOKEN"
    RELAY_NAME="${SPEEDBOND_RELAY_NAME:-$RELAY}"
    return 0
  fi

  local pick_args=(pick -json)
  [[ -n "${SPEEDBOND_CONFIG:-}" ]] && pick_args+=(-config "$SPEEDBOND_CONFIG")
  [[ -n "${SPEEDBOND_RELAY:-}" ]] && pick_args+=(-prefer "$SPEEDBOND_RELAY")

  local json
  json="$(./speedbond "${pick_args[@]}")" || return 1

  local env_lines
  env_lines="$(PICK_JSON="$json" python3 -c '
import json, os, shlex, sys
best = json.loads(os.environ["PICK_JSON"]).get("best")
if not best:
    sys.exit(1)
rtt = "%.0fms" % best.get("rtt_ms", 0)
for k, v in [
    ("RELAY", best["server"]),
    ("PIN", best["pin"]),
    ("TOKEN", best.get("token", "")),
    ("RELAY_NAME", best.get("name") or best["server"]),
    ("RELAY_RTT", rtt),
]:
    print("%s=%s" % (k, shlex.quote(str(v))))
')" || return 1
  eval "$env_lines"
}

active_relay_server() {
  # If speedbond client is already running, match its active relay server
  local live
  live="$(curl -sf --max-time 1 http://127.0.0.1:19454/status 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("server",""))' 2>/dev/null || true)"
  if [[ -n "$live" ]]; then
    echo "$live"
    return 0
  fi
  if [[ -n "${SPEEDBOND_SERVER:-}" ]]; then
    echo "$SPEEDBOND_SERVER"
    return 0
  fi
  if pick_relay 2>/dev/null; then
    echo "$RELAY"
    return 0
  fi
  return 1
}

clear_macos_socks() {
  local svc
  while IFS= read -r svc; do
    [[ -n "$svc" ]] && networksetup -setsocksfirewallproxystate "$svc" off 2>/dev/null || true
  done < <(networksetup -listnetworkserviceorder 2>/dev/null | awk '
    /^\([0-9]+\) / { name=$0; sub(/^\([0-9]+\) /, "", name); next }
    /\(Hardware Port:.*Device: en[0-9]+\)/ && name != "" { print name; name="" }
  ')
}

restore_alfa_if_on() {
  local mode
  mode="$(cat /tmp/alfa-mode 2>/dev/null || echo on)"
  if [[ "$mode" != "off" ]] && [[ -x "$ALFA_DIR/alfa.sh" ]] && [[ -S "$ALFA_SOCK" ]]; then
    "$ALFA_DIR/alfa.sh" proxy on >/dev/null 2>&1 || true
  fi
}

qemu_has_udp_fwd() {
  python3 - "$ALFA_SOCK" "$ALFA_PORT" <<'PY'
import socket, sys, time
sock_path, port = sys.argv[1], sys.argv[2]
try:
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(2)
    s.connect(sock_path)
    time.sleep(0.08)
    try: s.recv(4096)
    except Exception: pass
    s.sendall(b"info usernet\n")
    time.sleep(0.15)
    out = s.recv(16384).decode(errors="ignore")
    s.close()
    sys.exit(0 if ("UDP[HOST_FORWARD]" in out and f" {port} " in out) else 1)
except Exception:
    sys.exit(1)
PY
}

qemu_set_udp_fwd() {
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
}

setup_alfa_bridge() {
  local force="${1:-0}"
  local target_relay="${2:-${RELAY:-}}"
  [[ -n "$target_relay" ]] || target_relay="$(active_relay_server || true)"
  [[ -n "$target_relay" && -S "$ALFA_SOCK" && -f "$ALFA_KEY" ]] || return 1
  # Verify SSH reachability first so auth/timeout failures never trigger USB resets.
  ssh "${SSH_OPTS[@]}" alfa@127.0.0.1 true 2>/dev/null || return 1

  local wlx
  wlx="$(ssh "${SSH_OPTS[@]}" alfa@127.0.0.1 'ip -4 -o addr show | awk "/wlx/{print \$2; exit}"' 2>/dev/null || true)"
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
      wlx="$(ssh "${SSH_OPTS[@]}" alfa@127.0.0.1 'ip -4 -o addr show | awk "/wlx/{print \$2; exit}"' 2>/dev/null || true)"
    fi
  fi
  [[ -n "$wlx" ]] || return 1

  local need_restart="$force"
  # Deploy arm64 binary into VM if missing or outdated (atomic mv avoids ETXTBSY)
  if ! ssh "${SSH_OPTS[@]}" alfa@127.0.0.1 'test -x ~/speedbond-bridge' 2>/dev/null || \
     [[ "$(stat -f%m ./speedbond-arm64)" -gt "$(ssh "${SSH_OPTS[@]}" alfa@127.0.0.1 'stat -c%Y ~/speedbond-bridge 2>/dev/null || echo 0')" ]]; then
    scp "${SCP_OPTS[@]}" ./speedbond-arm64 alfa@127.0.0.1:~/speedbond-bridge.new
    ssh "${SSH_OPTS[@]}" alfa@127.0.0.1 'chmod +x ~/speedbond-bridge.new && mv -f ~/speedbond-bridge.new ~/speedbond-bridge'
    need_restart=1
  fi

  # Restart bridge if missing or targeting a different relay / wlx interface
  if [[ "$need_restart" -eq 0 ]]; then
    if ! ssh "${SSH_OPTS[@]}" alfa@127.0.0.1 "pgrep -f 'speedbond-bridge bridge.*-target $target_relay -iface $wlx' >/dev/null 2>&1"; then
      need_restart=1
    fi
  fi

  if [[ "$need_restart" -eq 1 ]]; then
    ssh "${SSH_OPTS[@]}" alfa@127.0.0.1 "
      sudo pkill -f '[s]peedbond-bridge bridge' 2>/dev/null || true
      sudo rm -f /tmp/speedbond-bridge.log
      sleep 0.2
      sudo python3 -c 'import subprocess; subprocess.Popen([\"/home/alfa/speedbond-bridge\",\"bridge\",\"-listen\",\":$ALFA_PORT\",\"-target\",\"$target_relay\",\"-iface\",\"$wlx\"], stdin=subprocess.DEVNULL, stdout=open(\"/var/log/speedbond-bridge.log\",\"wb\"), stderr=subprocess.STDOUT, start_new_session=True)'
    "
  fi

  if [[ "$need_restart" -eq 1 ]] || ! qemu_has_udp_fwd; then
    qemu_set_udp_fwd
  fi

  echo "Alfa UDP bridge ready on 127.0.0.1:$ALFA_PORT -> $wlx -> $target_relay" >&2
  return 0
}

if [[ "${1:-}" == "--setup-alfa" ]]; then
  force=0
  [[ "${2:-}" == "--force" ]] && force=1
  setup_alfa_bridge "$force"
  exit $?
fi

PREPARE_ONLY=0
if [[ "${1:-}" == "--prepare" ]]; then
  PREPARE_ONLY=1
  shift
fi

# Pick fastest reachable relay from relays.json (or respect SPEEDBOND_RELAY / env)
RELAY="" PIN="" TOKEN="" RELAY_NAME="" RELAY_RTT=""
pick_relay || {
  echo "No reachable relay found. Configure relays.json or run: ./deploy-relay.sh user@YOUR_VPS_IP" >&2
  exit 1
}

links=()
if [[ $# -gt 0 ]]; then
  IFS=',' read -r -a links <<< "$1"
  for item in "${links[@]}"; do
    if [[ "$item" == alfa=* ]]; then
      setup_alfa_bridge 0 "$RELAY" || { echo "Alfa VM bridge setup failed"; exit 1; }
    fi
  done
else
  if setup_alfa_bridge 0 "$RELAY"; then
    links+=("alfa=127.0.0.1:$ALFA_PORT")
  fi
  # Auto-detect ALL active physical IPv4 interfaces (skip link-local 169.254.x.x & virtuals)
  while IFS= read -r dev; do
    [[ -n "$dev" ]] && links+=("$dev")
  done < <(./speedbond interfaces | awk '
    $2 !~ /^(utun|awdl|llw|bridge|ap|lo|anpi|anri)/ && $0 ~ /[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\// && $0 !~ /169\.254\./ { print $2 }
  ')
fi

if [[ ${#links[@]} -eq 0 ]]; then
  echo "No active links found."
  exit 1
fi

LINKS_CSV="$(IFS=','; echo "${links[*]}")"

if [[ "$PREPARE_ONLY" -eq 1 ]]; then
  umask 077
  {
    printf 'SPEEDBOND_TOKEN=%q\n' "$TOKEN"
    printf 'SPEEDBOND_LINKS=%q\n' "$LINKS_CSV"
    printf 'SPEEDBOND_SERVER=%q\n' "$RELAY"
    printf 'SPEEDBOND_PIN=%q\n' "$PIN"
    printf 'SPEEDBOND_RELAY_NAME=%q\n' "$RELAY_NAME"
  } >/tmp/speedbond-launch.env
  echo "$RELAY_NAME ($RELAY ${RELAY_RTT}) -> $LINKS_CSV"
  exit 0
fi
export SPEEDBOND_TOKEN="$TOKEN"

# Clear macOS SOCKS5 proxy while bonded so browsers use the combined VPN (never touch /tmp/alfa-mode)
clear_macos_socks

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
  restore_alfa_if_on
}
trap cleanup EXIT

echo "Starting Speedbond -> $RELAY_NAME ($RELAY ${RELAY_RTT}) with links: $LINKS_CSV"
echo "Run 'bond' in another terminal for the live TUI monitor & speed proof."
sudo -E ./speedbond client \
  -server "$RELAY" \
  -relay-name "$RELAY_NAME" \
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
