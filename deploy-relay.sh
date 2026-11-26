#!/usr/bin/env bash
# Deploy an Open Speedify relay onto any Ubuntu/Debian VPS and add it to local relays.json.
# Usage: [SSH_OPTS="-i ~/.ssh/key -o IdentitiesOnly=yes"] ./deploy-relay.sh user@YOUR_VPS_IP ["🇩🇪 Germany"] [443]
set -euo pipefail
cd "$(dirname "$0")"

TARGET="${1:-}"
NAME="${2:-}"
PORT="${3:-}"

if [[ -z "$TARGET" ]]; then
  cat >&2 <<'EOF'
Usage: ./deploy-relay.sh <user@vps-ip> [display-name] [udp-port]

Examples:
  ./deploy-relay.sh root@203.0.113.10 "🇩🇪 Germany"
  ./deploy-relay.sh ubuntu@198.51.100.20 "🇰🇪 Nairobi" 4545
  SSH_OPTS="-i ~/.ssh/mykey -o IdentitiesOnly=yes" ./deploy-relay.sh root@203.0.113.10 "🇿🇦 JNB"
EOF
  exit 1
fi

SSH_EXTRA=()
SCP_EXTRA=()
if [[ -n "${SSH_OPTS:-}" ]]; then
  # shellcheck disable=SC2206
  SSH_EXTRA=( $SSH_OPTS )
  # Convert -p <port> to -P <port> for scp
  i=0
  while [[ $i -lt ${#SSH_EXTRA[@]} ]]; do
    if [[ "${SSH_EXTRA[$i]}" == "-p" && $((i+1)) -lt ${#SSH_EXTRA[@]} ]]; then
      SCP_EXTRA+=("-P" "${SSH_EXTRA[$((i+1))]}")
      i=$((i+2))
    else
      SCP_EXTRA+=("${SSH_EXTRA[$i]}")
      i=$((i+1))
    fi
  done
fi

HOST="${TARGET##*@}"
[[ -n "$NAME" ]] || NAME="🌐 $HOST"

# Resolve hostname to IPv4 so reconnects after Mac sleep never depend on DNS
RELAY_HOST="$HOST"
if ! [[ "$HOST" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  RESOLVED="$(dig +short "$HOST" A 2>/dev/null | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1 || true)"
  [[ -n "$RESOLVED" ]] && RELAY_HOST="$RESOLVED"
fi

echo "==> Checking remote architecture & UDP ports on $TARGET..."
PROBE="$(ssh -o StrictHostKeyChecking=accept-new ${SSH_EXTRA[@]+"${SSH_EXTRA[@]}"} "$TARGET" '
  echo "ARCH=$(uname -m)"
  if ss -ulnp 2>/dev/null | grep -qE ":443\b" && ! ss -ulnp 2>/dev/null | grep -qE ":443\b.*speedbond"; then
    echo "UDP443=busy"
  else
    echo "UDP443=free"
  fi
')"
ARCH_RAW="$(printf '%s\n' "$PROBE" | awk -F= '/^ARCH=/{print $2; exit}')"
UDP443="$(printf '%s\n' "$PROBE" | awk -F= '/^UDP443=/{print $2; exit}')"

if [[ -z "$PORT" ]]; then
  if [[ "$UDP443" == "busy" ]]; then
    PORT=4545
    echo "==> UDP :443 already in use (e.g. Caddy/HTTP3) — using UDP :4545 instead."
  else
    PORT=443
  fi
fi

case "$ARCH_RAW" in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "Unsupported remote arch: $ARCH_RAW" >&2; exit 1 ;;
esac

TMP_BIN="$(mktemp /tmp/speedbond-relay-XXXXXX)"
trap 'rm -f "$TMP_BIN"' EXIT

echo "==> Building Linux/$GOARCH relay binary..."
GOOS=linux GOARCH="$GOARCH" CGO_ENABLED=0 go build -o "$TMP_BIN" .

echo "==> Uploading binary and systemd service..."
scp -q ${SCP_EXTRA[@]+"${SCP_EXTRA[@]}"} "$TMP_BIN" "$TARGET:/tmp/speedbond.new"
scp -q ${SCP_EXTRA[@]+"${SCP_EXTRA[@]}"} ./speedbond-relay.service "$TARGET:/tmp/speedbond-relay.service"

echo "==> Configuring /etc/speedbond and starting systemd service on UDP :$PORT..."
OUT="$(ssh ${SSH_EXTRA[@]+"${SSH_EXTRA[@]}"} "$TARGET" "PORT='$PORT' bash -s" <<'REMOTE'
set -euo pipefail
SUDO=""
[[ "$(id -u)" -ne 0 ]] && SUDO="sudo"

$SUDO install -m 755 /tmp/speedbond.new /usr/local/sbin/speedbond
rm -f /tmp/speedbond.new
$SUDO mkdir -p /etc/speedbond

if [[ ! -f /etc/speedbond/relay.crt || ! -f /etc/speedbond/relay.key ]]; then
  $SUDO /usr/local/sbin/speedbond cert -cert /etc/speedbond/relay.crt -key /etc/speedbond/relay.key >/dev/null
  $SUDO chmod 600 /etc/speedbond/relay.key
fi

PIN="$(openssl x509 -in /etc/speedbond/relay.crt -outform DER | sha256sum | awk '{print $1}')"

if [[ ! -f /etc/speedbond/environment ]]; then
  TOKEN="$(openssl rand -hex 24)"
  printf 'SPEEDBOND_TOKEN=%s\n' "$TOKEN" | $SUDO tee /etc/speedbond/environment >/dev/null
  $SUDO chmod 600 /etc/speedbond/environment
else
  TOKEN="$($SUDO awk -F= '/^SPEEDBOND_TOKEN=/{print $2; exit}' /etc/speedbond/environment)"
fi

sed "s|-listen :[0-9]*|-listen :${PORT}|g" /tmp/speedbond-relay.service | $SUDO tee /etc/systemd/system/speedbond-relay.service >/dev/null
rm -f /tmp/speedbond-relay.service

if command -v ufw >/dev/null 2>&1 && $SUDO ufw status 2>/dev/null | grep -q "Status: active"; then
  $SUDO ufw allow "${PORT}/udp" >/dev/null || true
fi

$SUDO tee /etc/sysctl.d/99-speedbond-udp.conf >/dev/null <<'SYSCTL'
net.core.rmem_max = 26214400
net.core.wmem_max = 26214400
net.core.netdev_max_backlog = 10000
SYSCTL
$SUDO sysctl --system >/dev/null 2>&1 || true

$SUDO systemctl daemon-reload
$SUDO systemctl enable --now speedbond-relay
$SUDO systemctl restart speedbond-relay
sleep 1
$SUDO systemctl is-active --quiet speedbond-relay

printf 'PIN=%s\nTOKEN=%s\n' "$PIN" "$TOKEN"
REMOTE
)"

PIN="$(printf '%s\n' "$OUT" | awk -F= '/^PIN=/{print $2; exit}')"
TOKEN="$(printf '%s\n' "$OUT" | awk -F= '/^TOKEN=/{print $2; exit}')"
SERVER="${RELAY_HOST}:${PORT}"

[[ -n "$PIN" && -n "$TOKEN" ]] || { echo "Failed to read PIN/TOKEN from remote" >&2; exit 1; }

CFG="${SPEEDBOND_CONFIG:-$PWD/relays.json}"
python3 - "$CFG" "$NAME" "$SERVER" "$PIN" "$TOKEN" <<'PY'
import json, os, sys
path, name, server, pin, token = sys.argv[1:]
cfg = {"selected": "auto", "relays": []}
if os.path.exists(path):
    with open(path) as f:
        cfg = json.load(f)
relays = [r for r in cfg.get("relays", []) if r.get("server") != server]
relays.append({"name": name, "server": server, "pin": pin, "token": token})
cfg["relays"] = relays
cfg.setdefault("selected", "auto")
os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
os.chmod(path, 0o600)
PY

echo ""
echo "✅ Relay deployed & added to $CFG (gitignored, chmod 600):"
echo "   Name:   $NAME"
echo "   Server: $SERVER"
echo "   Pin:    $PIN"
echo ""
echo "Test all relays and pick fastest:"
echo "   ./speedbond pick"
