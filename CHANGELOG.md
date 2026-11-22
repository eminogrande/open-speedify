# Changelog

All notable changes to this project are documented here. Dates use ISO format.

## [0.2.0] - 2026-11-22

### Fixed

- **Critical relay upload reassembly bug:** Moved `reassembler` from per-link goroutine onto shared `relaySession` in `relay.go`. Previously, splitting one sequence across multiple links caused constant gap stalls on the relay.
- **QUIC datagram MTU drops:** Lowered `tunnelMTU` from `1200` to `1150` so `1150 + 8B seq` safely fits inside QUIC's initial 1200-byte UDP payload before PMTUD.
- **Send-error drops & sequence gaps:** `linkPool.send()` now retries the same sequence number across remaining active links on send error instead of dropping the packet and stalling the receiver.
- **UDP socket leak:** Closing or reconnecting a link now closes its underlying `net.UDPConn`.
- **Multi-link macOS routing:** Installed per-interface `-ifscope <iface>` relay host routes lazily per physical link instead of failing when `links[0]` is down.
- **Reorder buffer & timer edge cases:** Raised reorder cap from `256` to `1024`, default `reorderWait` to `60ms`, fixed spurious timer start after `skipGap()`, and added clean `close()` on shutdown/expiry.

### Added

- **Adaptive AIMD smooth weighted round-robin scheduler** (`pool.go`) driven by QUIC `ConnectionStats` (packet loss + RTT inflation) with live per-link RX/TX Mbps, RTT, loss %, and weight % telemetry.
- **UDP bridge subcommand (`speedbond bridge`)** (`bridge.go`) + `name=host:port` link specs, enabling QEMU-hosted Alfa USB Wi-Fi adapters (`wlx*`) or remote hosts to bond as native UDP links.
- **Runtime link management & control API** (`127.0.0.1:19454`): hot-add, remove, pause, resume links live without dropping the tunnel (`speedbond status`, `speedbond ctl`, `speedbond probe`).
- **Built-in Solo vs Bonded speed proof (`speedbond bench`)**: automatically isolates each active link, then measures all links bonded together and reports speedup.
- **Native macOS menu bar app (`SpeedbondBar.swift` → `SpeedbondBar.app`)** and zero-dependency terminal TUI (`speedbond-tui.py` → `bond`).
- **Multi-link auto-detect launcher (`start-bond.sh`)**: discovers Alfa VM bridge + all active IPv4 interfaces (Wi-Fi, phones, Ethernet).
- **Regression tests:** `TestRelaySharedSessionReassemblyAndBridge` and `TestSmoothWeightedRoundRobin`.

### Verified

- `go test -count=1 -race ./...` passes across all unit and integration tests (including late-packet delivery without duplicate delivery).
- Fixed zero-HOL flow-sticky + MinRTT backpressure relay deployed on `188.34.194.25:443` (`mtu=1150`, TCP MSS clamping, `reorder=0s`).
- Live Solo vs Bonded speed proof (`alfa` + `en0` Infinix HOT 60i): `0.59 Mbps` solo + `0.75 Mbps` solo → **`1.34 Mbps` bonded (+78.3% vs best solo, 100% of combined sum)**, `Reordered: 0`, `Gaps: 0`.
- Live 3-link simultaneous bonding (`alfa` + `en0` Infinix HOT 60i + `feth50` Samsung USB): peaked at **`9.97 Mbps` combined TX** (`6.88 + 2.83 + 0.26`) and `0.99 Mbps↓ / 8.14 Mbps↑` simultaneous bidirectional traffic.
- Live mid-stream failover proof: killing primary link (`alfa`) mid-ping yielded **12/12 packets (0.0% loss)** while keeping VPN exit IP `188.34.194.25`.

## [0.1.0] - 2026-11-22

### Added

- Initial minimal Go macOS client and Ubuntu relay PoC using QUIC DATAGRAM, cert pinning, token auth, native TUN/utun, and Android `rndis-tether` scripts.
