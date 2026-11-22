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
| Combine N links (Wi-Fi + Phone + Alfa + Ethernet) | **Working** | Any mix of local IPv4 interfaces (`en0`, `feth50`, `en5`…) and UDP bridges (`alfa=127.0.0.1:4546`). |
| Adaptive bandwidth & loss-aware scheduling | **Working** | AIMD smooth weighted round-robin using QUIC `ConnectionStats` (loss + RTT inflation) with instant send-error failover. |
| Seamless link failover (keep TCP alive) | **Working** | Shared per-session reassembly on both client and relay (`MTU=1150`, `60ms` reorder window, `1024`-packet buffer). |
| Hot-add / remove / pause links live | **Working** | Add a phone or second adapter mid-session without dropping connections (`bond` TUI, `SpeedbondBar.app`, or `speedbond ctl`). |
| macOS menu bar app + TUI monitor | **Working** | Native `SpeedbondBar.app` status bar controller + `bond` terminal TUI. |
| Built-in Solo vs Bonded speed proof | **Working** | `speedbond bench` isolates each link solo, then measures all bonded together. |
| Live 2-ISP bonded speedup measurement | **Pending phone test** | Relay + multi-link tunnel verified live; plug in cellular phone and run `speedbond bench` / **Run Speed Proof** to record 2-ISP throughput. |
| Redundant / FEC mode (voice/video packet duplication) | **Not yet** | Currently bonding + failover only (no duplicate-packet redundant mode). |
| IPv6 tunneling | **Not yet** | IPv4 tunnel only. |
| Automatic plug-and-play interface watcher | **Not yet** | Menu bar detects new interfaces for 1-click hot-add; full background auto-join not added yet. |

## Verified Proof (2026-11-22)

1. **Automated transport, shared-session reassembly, bridge & failover tests (`go test -race -v ./...`):**
   - `TestReassemblerOrdersAndSkipsLostPackets` — PASS (ordering, duplicate suppression, gap expiry).
   - `TestTwoQUICLinksAndFailover` — PASS (2 real QUIC links, 0 packet drops across simulated mid-stream link kill).
   - `TestRelaySharedSessionReassemblyAndBridge` — PASS (2 links—direct + UDP bridge—sharing one relay session, 40/40 interleaved packets delivered in exact order with **0 skipped gaps**).
   - `TestSmoothWeightedRoundRobin` — PASS (3:1 weighted scheduling verified 30:10 over 40 packets).
2. **Live multi-link relay probe against deployed Hetzner Ubuntu relay (`188.34.194.25:443`, `mtu=1150`, `reorder=60ms`):**
   ```text
   OK    en0           tunnel_ip=10.77.0.2  rtt=199ms   target=188.34.194.25:443
   OK    alfa          tunnel_ip=10.77.0.2  rtt=1.036s  target=127.0.0.1:4546
   ```
   Both MacBook Wi-Fi (`en0`) and the Alfa AWUS1900 USB antenna inside the ARM64 QEMU VM (via live `speedbond bridge` bound to `wlx00c0caa826f5`) authenticated with pinned TLS + shared token and joined the same relay session (`10.77.0.2`).
3. **Single-link baselines measured on this Mac:**
   - MacBook Wi-Fi (`en0`, unbonded): **4.66 Mbps** down.
   - Alfa AWUS1900 5 GHz (`wlx00c0caa826f5`, unbonded): **9.0 Mbps** down.
   - *Note:* `en0` and `alfa` share the same upstream router (`154.74.156.15`). Real additive speedup requires **2+ independent ISPs** (e.g. Alfa/Wi-Fi + cellular phone tether). Run **⚡ Run Speed Proof** in `SpeedbondBar.app` (or `speedbond bench`) once tethered to measure `Solo A` vs `Solo B` vs `Bonded A+B`.

## Quick Start (macOS)

### 1. Menu Bar App (`SpeedbondBar.app`)
Build and launch the native status bar controller:
```bash
swiftc -O -swift-version 5 -framework Cocoa SpeedbondBar.swift -o SpeedbondBar.app/Contents/MacOS/SpeedbondBar
open SpeedbondBar.app
```
From the menu bar (`⚡ Bond OFF` → `🟢 BOND 2x`):
- **▶ Start Bonding** — auto-detects Alfa VM bridge + all active IPv4 interfaces (Wi-Fi, phone, Ethernet) and disables macOS SOCKS5 while bonded so browsers use the tunnel.
- **📱 Connect Android USB Tether** — starts `rndis-tether` and hot-adds `feth50` live.
- **➕ Hot-Add `<iface>`** — appears automatically when you plug in a phone, USB Wi-Fi, or Ethernet adapter.
- **⚡ Run Speed Proof (Solo vs Bonded)** — isolates each link solo, then benchmarks all combined and prints `% faster`.

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
