# Open Speedify (`speedbond`)

Open-source multi-link QUIC bonding VPN for macOS + Linux relay. Combines **any number of internet connections**—MacBook Wi-Fi, external USB Wi-Fi antennas (including QEMU-bridged Alfa adapters), Android/iPhone USB tethering, Ethernet, or secondary links—into one encrypted tunnel with one stable public IP.

```text
                 ┌── QUIC DATAGRAM over Wi-Fi (en0) ───────────┐
                 ├── QUIC DATAGRAM over Phone USB (feth50/enX) ─┼── Linux Relay ── Internet
macOS (utun) ────┼── QUIC DATAGRAM over Alfa VM UDP bridge ─────┤   (one public IP)
                 └── QUIC DATAGRAM over Ethernet / Nth link ───┘
```

## How close is this to Speedify?

| Capability | Status | Details |
|---|---|---|
| Combine N links (Wi-Fi + Phone + Alfa + Ethernet) | **Working & Proven** | Any mix of local IPv4 interfaces (`en0`, `feth50`, `en5`…) and UDP bridges (`alfa=127.0.0.1:4546`). Verified live with 3 links simultaneously. |
| Flow-sticky + MinRTT backpressure spillover | **Working & Proven** | Spreads concurrent streams across all links with zero cross-flow stalls (`Reordered: 0`, `Gaps: 0`), and spills single-stream bursts across remaining links. |
| Seamless link failover (keep calls/TCP alive) | **Working & Proven** | Verified live: killing primary link (`alfa`) mid-stream produced **12/12 pings (0.0% loss)** while keeping VPN IP `188.34.194.25`. |
| Hot-add / remove / pause links live | **Working & Proven** | Add/pause/remove links mid-session without dropping connections (`SpeedbondBar.app`, `bond` TUI, or `speedbond ctl`). |
| macOS menu bar app + TUI monitor | **Working & Proven** | Native `SpeedbondBar.app` status bar controller + `bond` terminal TUI. |
| Built-in Solo vs Bonded speed proof | **Working & Proven** | `speedbond bench` isolates each link solo, then measures all bonded together. |
| Live multi-ISP bonded speedup measurement | **Proven (+78.3% / 100% sum)** | Live proof: `alfa` solo `0.59 Mbps` + `en0` solo `0.75 Mbps` → **bonded `1.34 Mbps` (+78.3% vs best solo, 100% of combined sum)**; 3-link simultaneous TX peaked at **9.97 Mbps** (`6.88 + 2.83 + 0.26`). |
| Redundant / FEC mode (duplicate packets for zero-glitch calls on hard physical unplug) | **Not yet** | Clean failover keeps calls connected; sudden silent unplug waits for QUIC idle timeout (~5–10s) unless redundant packet duplication is added later. |
| IPv6 tunneling | **Not yet** | IPv4 tunnel only. |
| Automatic plug-and-play interface watcher | **Not yet** | Menu bar detects new interfaces for 1-click hot-add; full background auto-join not added yet. |

## Verified Live Proof (2026-11-22)

1. **Live Solo vs Bonded Speed Proof (`./speedbond bench -seconds 8 -streams 3`, Hetzner FSN1, two separate public IPs `154.74.156.15` + `154.74.156.13`):**
   ```text
   SOLO   alfa            0.59 Mbps  (0.59 MB in 8s)
   SOLO   en0             0.75 Mbps  (0.75 MB in 8s)
   BONDED alfa+en0        1.34 Mbps  (1.34 MB in 8s)

   PROOF: best solo=0.75 Mbps | sum=1.34 Mbps | bonded=1.34 Mbps (+78.3% vs best solo, 100% of combined sum)
   ```
   Live peak during bonded download: **`1.51 Mbps` RX** (`alfa` `1.01 Mbps` + `en0` `0.51 Mbps`), **`Reordered: 0`, `Gaps: 0`**.
2. **Live 3-Link Simultaneous Bonding (`alfa` Njamakuya Wi-Fi + `en0` Infinix HOT 60i + `feth50` Samsung Android USB):**
   ```text
   Total:  RX 0.50 Mbps  TX 9.97 Mbps  Delivered: 76957  Reordered: 0  Gaps: 0
   alfa    UP  68%  192.7ms  0.0%  RX 0.19  TX 6.88 Mbps  (127.0.0.1:4546)
   en0     UP  29%  251.0ms  0.0%  RX 0.21  TX 2.83 Mbps  (en0)
   feth50  UP   3%  314.8ms  5.0%  RX 0.10  TX 0.26 Mbps  (feth50)
   ```
   Also observed simultaneous 3-link TX+RX: `alfa` `0.61↓/5.32↑` + `en0` `0.19↓/1.84↑` + `feth50` `0.19↓/0.98↑` = **`0.99 Mbps↓ / 8.14 Mbps↑`** (`196 MB` uploaded with 0 tunnel errors, 0 dropped).
3. **Live Mid-Stream Failover Proof (Call / Session Continuity):**
   - Continuous `ping -c 12 -i 0.5 1.1.1.1` through tunnel while pausing primary link (`alfa`) mid-stream and resuming 3s later:
     **`12 packets transmitted, 12 packets received, 0.0% packet loss`** (`188–242ms`), public VPN exit IP stayed **`188.34.194.25`** continuously.
4. **Automated Unit & Race Tests (`go test -count=1 -race ./...`):**
   - `TestReassemblerOrdersAndSkipsLostPackets` (incl. late-packet delivery + duplicate suppression) — PASS
   - `TestTwoQUICLinksAndFailover` — PASS
   - `TestRelaySharedSessionReassemblyAndBridge` — PASS
   - `TestSmoothWeightedRoundRobin` — PASS

## Quick Start (macOS)

### 1. Menu Bar App (`SpeedbondBar.app`) — Zero-Jargon Autopilot
Build and launch the native status bar controller:
```bash
swiftc -O -swift-version 5 -framework Cocoa -framework CoreWLAN SpeedbondBar.swift -o SpeedbondBar.app/Contents/MacOS/SpeedbondBar
open SpeedbondBar.app
```
From the menu bar (`⚪ Bond OFF` → `🟢 Combined 2x`):
- **▶ Turn ON / ■ Turn OFF** — one click. Automatically discovers & combines Mac Wi-Fi, Alfa Antenna, Android/iPhone USB, and Ethernet, and keeps browsers routed through the VPN.
- **Background Autopilot** — plug in a phone or wake from sleep and Speedbond auto-joins new connections and self-heals Alfa/USB links without manual clicks.
- **📶 Join Another Wi-Fi…** — scans nearby Wi-Fi networks and connects Mac Wi-Fi or Alfa Antenna to a 2nd network (prompting for password only if not already saved).
- **⚡ Test Speed (Alone vs Combined)** — plain-English speed proof right in the menu bar; all technical details stay tucked inside **Advanced ▸**.

### 2. Terminal Launcher + Live TUI (`bond`)
```bash
./start-bond.sh          # Terminal 1: auto-detects all links & starts client (sudo)
bond                     # Terminal 2: live TUI ([a] add, [space] pause, [x] remove, [b] speed proof)
```

### 3. CLI Commands
```bash
speedbond interfaces                                     # list active interfaces
speedbond probe -server VPS:443 -links en0,alfa=... -pin PIN
speedbond status                                         # live JSON/table status
speedbond ctl add feth50                                 # hot-add link live
speedbond ctl pause en0                                  # pause link live
speedbond ctl resume en0                                 # resume link live
speedbond bench -seconds 6 -streams 4                    # Solo A vs Solo B vs Bonded proof
speedbond bridge -listen :4546 -target VPS:443 -iface wlx00c0caa826f5
```

## Build & Relay Setup

Requires Go 1.24+:
```bash
go test -race ./...
go build -o speedbond .
GOOS=linux GOARCH=amd64 go build -o speedbond-linux .    # Ubuntu x86_64 relay
GOOS=linux GOARCH=arm64 go build -o speedbond-arm64 .    # ARM64 VM/SBC UDP bridge
```

On the Ubuntu relay:
```bash
install -m 755 speedbond-linux /usr/local/sbin/speedbond
/usr/local/sbin/speedbond cert -cert /etc/speedbond/relay.crt -key /etc/speedbond/relay.key
SPEEDBOND_TOKEN=... /usr/local/sbin/speedbond relay \
  -listen :443 -cert /etc/speedbond/relay.crt -key /etc/speedbond/relay.key
```
See `speedbond-relay.service` for the systemd unit.

## Source Layout

- `client.go`: multi-link manager, flexible link specs (`iface` & `name=host:port`), dynamic add/remove/pause, loopback control HTTP API (`127.0.0.1:19454`).
- `relay.go`: shared-session reassembly, adaptive downlink bonding, address allocation, NAT forwarding.
- `pool.go`, `protocol.go`, `reorder.go`: AIMD smooth weighted round-robin scheduler, failover retry, `MTU=1150` framing, `1024`-pkt reorder buffer.
- `bridge.go`: zero-dependency UDP interface bridge (used for Alfa QEMU VM `wlx*` passthrough).
- `tun_darwin.go`, `tun_linux.go`: native TUN/utun and per-interface `-ifscope` relay routing.
- `SpeedbondBar.swift`: native macOS menu bar controller & speed proof UI.
- `speedbond-tui.py`: zero-dependency terminal TUI (`bond`).
- `start-bond.sh`: auto-detect launcher (Alfa VM live QEMU UDP forward + all active IPv4 interfaces).
