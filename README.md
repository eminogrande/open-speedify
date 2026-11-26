# 🚀 Open Speedify (`speedbond`)

Open-source, self-hosted **multi-link QUIC bonding VPN** for macOS + Linux VPS.
Combine **Mac Wi-Fi, Alfa USB Wi-Fi, iPhone USB, Android USB, and Ethernet** into one encrypted VPN with one stable IP — faster speeds + drop-resistant calls.

```text
               ┌── 📶 Mac Wi-Fi (en0) ───────────┐
               ├── 📡 Alfa Antenna (VM UDP) ─────┼── 🌍 Fastest VPS ── Internet
macOS (utun) ──┼── 📱 iPhone USB (enX) ──────────┤   (1 stable VPN IP)
               ├── 🤖 Android USB (feth50) ──────┤
               └── 🔌 Ethernet / Nth link ───────┘
```

## ⚡ Quick Start (2 Commands)

Requires **Go 1.24+**, **Xcode CLT** (`xcode-select --install`) on macOS, and any **Ubuntu/Debian KVM VPS**.

```bash
git clone https://github.com/eminogrande/open-speedify.git
cd open-speedify

# 1. Deploy your VPS relay (repeat for 2nd/3rd VPS — auto-picks fastest!)
./deploy-relay.sh root@YOUR_VPS_IP "🇩🇪 Germany"

# 2. Build binaries + launch macOS menu bar app (auto-starts on login)
./build-speedbondbar.sh
```

Then click **`⚪ 🚀`** in the menu bar → **`🟢 Turn ON (Combine All)`**.

- **Menu bar:** `⚪ 🚀` OFF · `🟡 🚀 1x` 1 link · `🟢 🚀 3x` 3 links combined
- **Automatic everything:** picks your fastest healthy VPS, joins Wi-Fi / Alfa / iPhone / Android / Ethernet as you plug them in, prunes unplugged USBs, and recovers tunnel IP after Mac sleep.
- **Security:** `relays.json` is gitignored (`chmod 600`). Every relay requires a pinned TLS cert + shared `SPEEDBOND_TOKEN`.

## 🌍 Add More Servers (Human or AI Agent)

No app rebuild needed — `SpeedbondBar` reads `relays.json` live every 1.5s.

```bash
# Add any Ubuntu/Debian VPS (auto-falls back to UDP 4545 if UDP 443 is busy):
./deploy-relay.sh root@SECOND_VPS_IP "🇰🇪 Nairobi"

# Compare real QUIC ping across all servers:
./speedbond pick
```

- **Switch server in menu bar:** `🔧 More ▸ 🌍 Server` → `⚡ Auto (Fastest)` or pin any VPS.
- **Using an AI agent?** Point your agent to **[`AGENTS.md`](AGENTS.md)** with your new VPS IP — it deploys, checks ports, updates `relays.json`, and verifies `./speedbond pick` automatically.

## 📊 Proven Results

- **Combined speed proof (`speedbond bench`, two ISPs):** `alfa` solo `0.59 Mbps` + `en0` solo `0.75 Mbps` → **combined `1.34 Mbps` (`+78.3%` vs best solo, `100%` of sum)**, `0` gaps, `0` reordered.
- **4 simultaneous links (`4/4 UP`):** Mac Wi-Fi + Alfa + Android USB + iPhone USB (`9.97 Mbps` peak TX, `0%` loss).
- **Mid-stream failover:** Killing primary link mid-ping kept **`12/12` packets (`0%` loss)** with zero IP change.

<details>
<summary>CLI commands, manual VPS setup & optional hardware (Alfa / Android USB)</summary>

### CLI & TUI
```bash
./speedbond pick                                         # Test all servers in relays.json
./start-bond.sh                                          # Start bonding from terminal
SPEEDBOND_RELAY="🇩🇪 Germany" ./start-bond.sh             # Pin a specific server
./speedbond-tui.py                                       # Live terminal TUI
./speedbond status                                       # Live per-link Mbps, RTT, loss
./speedbond bench -seconds 8 -streams 3                  # Solo vs. combined speed proof
go test -count=1 -race ./...                             # Automated race-detector tests
```

### Manual VPS Setup (without `deploy-relay.sh`)
```bash
GOOS=linux GOARCH=amd64 go build -o speedbond-linux .
# Copy to VPS /usr/local/sbin/speedbond, then on VPS:
sudo mkdir -p /etc/speedbond
sudo /usr/local/sbin/speedbond cert -cert /etc/speedbond/relay.crt -key /etc/speedbond/relay.key
echo "SPEEDBOND_TOKEN=$(openssl rand -hex 24)" | sudo tee /etc/speedbond/environment >/dev/null
sudo chmod 600 /etc/speedbond/environment
sudo cp speedbond-relay.service /etc/systemd/system/
sudo systemctl enable --now speedbond-relay
```
Copy [`relays.example.json`](relays.example.json) → `relays.json` (`chmod 600`) and fill in `server`, `pin`, and `token`.

### Optional Hardware
- **Alfa AWUS1900 on Apple Silicon:** [eminogrande/alfa-awus1900-apple-silicon](https://github.com/eminogrande/alfa-awus1900-apple-silicon) — auto-detected and bridged over QEMU UDP (`127.0.0.1:4546`), with clean SOCKS5 handoff on ON/OFF.
- **Android USB Tethering on macOS:** `brew install stlk0/tap/rndis-tether && sudo rndis-tether install && rndis-tether config prefer-phone no && rndis-tether config tx-batch 1`.

</details>
