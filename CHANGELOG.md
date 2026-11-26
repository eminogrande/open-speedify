# Changelog

All notable changes to this project are documented here. Dates use ISO format.

## [0.5.1] - 2026-11-26

### Added

- **LLM / Agent Playbook (`AGENTS.md`):** Drop-in guide for any AI coding agent to deploy and verify new VPS relays cleanly without touching app code or committing secrets.
- **Smart `deploy-relay.sh`:** Automatically checks if UDP `:443` is already in use (e.g. Caddy/Nginx HTTP/3) and falls back to UDP `:4545`, resolves hostnames to IPv4 for sleep-proof reconnects, and supports `SSH_OPTS` for custom SSH keys/ports.

### Fixed

- **`speedbond pick` while already bonded (`pick.go`):** Iterates through all active physical interfaces plus unbound fallback instead of stopping on an unscoped `en0` route error, so `speedbond pick` and `🔍 Check Server Speeds` accurately measure all relays even while `utun` is active.

## [0.5.0] - 2026-11-26

### Added

- **Multi-relay auto-fastest selection (`pick.go`, `relays.example.json`):** Add as many VPS relays as you want in gitignored `relays.json` (chmod `600`). `speedbond pick` probes all configured relays in parallel over QUIC (binding to your physical interface so it even works accurately while bonded) and automatically selects the lowest-latency healthy server.
- **One-command VPS relay deployer (`deploy-relay.sh`):** Cross-compiles for the VPS architecture (`amd64`/`arm64`), installs `/usr/local/sbin/speedbond` + `speedbond-relay.service`, generates a unique TLS cert pin + random `SPEEDBOND_TOKEN`, tunes kernel UDP buffers (`26 MB`), opens UFW UDP `443`, and appends the relay to your local gitignored `relays.json`.
- **Dynamic relay switcher in `SpeedbondBar.swift`:** Displays active relay name (`st.relay_name`) dynamically and adds `🔧 More ▸ 🌍 Server ▸` (`⚡ Auto (Fastest)`, per-relay selector, and `🔍 Check Server Speeds`).

### Changed

- **100% open-source portable config:** Removed all hardcoded relay IPs, cert pins, and personal `/Users/...` paths from `start-bond.sh`, `SpeedbondBar.swift`, and `speedbond-tui.py`, and deleted obsolete day-1 PoC scripts (`start-poc.sh`, `run-e2e.sh`).

### Verified

- `git grep` confirms zero personal IPs, cert pins, or home paths remain in tracked files.
- `go test -count=1 -race ./...` passes (including new `TestPickFastestRelay`).
- Live `./speedbond pick` and `./start-bond.sh --prepare` succeed even while bonded on `utun5`.

## [0.4.0] - 2026-11-26

### Fixed

- **Alfa bridge SSH authentication failure (`start-bond.sh`):** Added `-o IdentitiesOnly=yes -o PreferredAuthentications=publickey -o PasswordAuthentication=no` to `ssh` and `scp` so `ssh-agent`/smartcard keys never cause `Too many authentication failures`, and verified SSH reachability before running any Wi-Fi/USB recovery.
- **Post-sleep / relay-restart tunnel IP blackhole (`client.go`, `tun_darwin.go`, `tun_linux.go`):** When a session expires during Mac sleep (>2 min) and the relay assigns a new `10.77.x.y` tunnel IP on reconnect, the client now detects the IP change via `syncTunnelIP` and updates `utun`/routes live instead of sending packets with the stale IP (`recv=0`).
- **Stopped breaking Alfa mode (`SpeedbondBar.swift`, `start-bond.sh`):** Open Speedify no longer overwrites `/tmp/alfa-mode` with `off`. It clears macOS system SOCKS5 across physical services while bonded and immediately restores Alfa proxy on Turn OFF if Alfa is ON.

### Changed

- **Ultra-simplified emoji menu bar (`SpeedbondBar.swift`, `build-speedbondbar.sh`):** Compact `⚪ 🚀` / `🟡 🚀 1x` / `🟢 🚀 3x` status bar with single `🟢 Turn ON (Combine All)` / `🔴 Turn OFF` toggle, emoji link list (`📡 Alfa`, `📶 Mac Wi-Fi`, `📱 iPhone`, `🤖 Android`, `🔌 Ethernet`), collapsed `🔧 More ▸` submenu, and login LaunchAgent (`com.speedbondbar.plist`).
- **Faster idempotent Alfa auto-join & stale link pruning:** Added `start-bond.sh --setup-alfa [--force]` (no Keychain prompt or launch-env clobber), auto-joins Alfa within seconds when plugged in & running, and auto-prunes unplugged USB interfaces.

### Verified

- `go test -count=1 -race ./...` passes (including new `TestClientSyncTunnelIP`).
- Live 3-link QUIC probe (`speedbond probe`): `alfa=127.0.0.1:4546` (`220ms`), `en0` Mac Wi-Fi (`182ms`), and `en9` iPhone USB (`219ms`) all `OK` on the same relay session (`10.77.0.48`), auto-discovered by `./start-bond.sh --prepare`.

## [0.3.0] - 2026-11-23

### Added

- **Non-technical autopilot macOS menu bar app (`SpeedbondBar.swift` → `SpeedbondBar.app`):**
  - Plain-English status (`⚪ Bond OFF` → `🟢 Combined 2x/3x/4x`), human device names (`Mac Wi-Fi`, `Alfa Antenna`, `Android Phone (USB)`, `iPhone (USB)`), and `Advanced ▸` submenu for technical details.
  - Background autopilot: automatically discovers and hot-joins newly plugged phones (iPhone USB, Android `feth50`), Ethernet, or Wi-Fi interfaces; auto-heals Alfa USB passthrough + DHCP after Mac sleep; skips Apple link-local `169.254.x.x` sync interfaces; and pauses `alfa-autoswitch` SOCKS5 while bonded so browsers always exit through the VPS VPN.
  - Built-in **📶 Join Another Wi-Fi…** picker using macOS `CoreWLAN` so users can connect Mac Wi-Fi or Alfa Antenna to a 2nd Wi-Fi network without touching the terminal.
- **Zero-HOL flow-sticky + MinRTT backpressure spillover scheduler (`pool.go`):**
  - Replaced blocking synchronous `SendDatagram` calls with per-link non-blocking send queues (`64` slots, spill threshold `8`) and 5-tuple TCP/UDP flow pinning + spillover so slow/high-RTT links can never block fast links.
  - Default `-reorder-wait 0` direct delivery eliminates cross-flow head-of-line stalls while preserving optional ordered reassembly with late-packet delivery (`skipped` tracking in `reorder.go`).
- **Relay TCP MSS clamping (`tun_linux.go`):** Added `iptables -t mangle` `--clamp-mss-to-pmtu` rules in both directions for `speedbond0`.
- **Reliable benchmark DNS & HTTP (`main.go`):** Added pure-Go `1.1.1.1:53` DNS fallback in `speedbond bench`, resilient solo-link error handling, and Hetzner FSN1 speedtest default.

### Fixed

- **Stale gateway route on Wi-Fi or subnet switch (`client.go`, `tun_darwin.go`):** Look up each interface's current DHCP default gateway (`route -n get -ifscope <iface> default`) and clear cached host routes on disconnect/failure so switching Wi-Fi networks or phone subnets reconnects automatically.
- **Alfa VM sleep recovery & atomic bridge upload (`start-bond.sh`):** Automatic `usb-off`/`usb-on` + saved Wi-Fi reconnect when Alfa loses IPv4 after macOS sleep, plus atomic `mv` upload to avoid `ETXTBSY`.
- **macOS `osascript` detach bug (`SpeedbondBar.swift`):** Replaced `nohup` (which fails under console-less `osascript`) with Python `start_new_session=True`.
- **Browser VPN bypass:** Automatically pauses `alfa-autoswitch` (`/tmp/alfa-mode = off`) and disables macOS system SOCKS5 proxy while bonded, restoring on stop.

### Verified

- `go test -count=1 -race ./...` passes across all unit and integration tests.
- **Live Solo vs. Combined Speed Proof (`speedbond bench`):** `alfa` solo `0.59 Mbps` + `en0` solo `0.75 Mbps` → **bonded `1.34 Mbps` (+78.3% vs best solo, 100% of combined sum; peak `1.51 Mbps`)**, `Reordered: 0`, `Gaps: 0`.
- **Live 4-Link Simultaneous Bonding (`4/4 UP`):** Mac Wi-Fi (`en0`) + Alfa Antenna (`alfa`) + Samsung Android USB (`feth50`) + iPhone SE USB (`en9`) bonded together (`4.64 Mbps` active, `0%` loss, `0` gaps).
- **Live Alfa + iPhone USB Bonding:** **`7.18 Mbps` TX** (`alfa` `5.79 Mbps` + `iPhone en9` `1.39 Mbps`, `731,298` packets delivered, `0` gaps).
- **Live 3-Link Peak Upload:** **`9.97 Mbps` TX** (`alfa` `6.88` + `en0` `2.83` + `feth50` `0.26`).
- **Live Mid-Stream Failover:** Killing primary link (`alfa`) mid-ping yielded **`12/12` packets (`0.0%` loss)** while maintaining VPS VPN exit IP.

## [0.2.0] - 2026-11-22

### Fixed

- **Critical relay upload reassembly bug:** Moved `reassembler` from per-link goroutine onto shared `relaySession` in `relay.go`.
- **QUIC datagram MTU drops:** Lowered `tunnelMTU` from `1200` to `1150` so `1150 + 8B seq` safely fits inside QUIC's initial 1200-byte UDP payload.
- **UDP socket leak:** Closing or reconnecting a link now closes its underlying `net.UDPConn`.
- **Multi-link macOS routing:** Installed per-interface `-ifscope <iface>` relay host routes lazily per physical link.

### Added

- `speedbond bridge` UDP forwarder (`bridge.go`) + `name=host:port` link specs for QEMU-hosted Alfa Wi-Fi adapters.
- Loopback HTTP control API (`127.0.0.1:19454`), `speedbond status`, `speedbond ctl`, `speedbond probe`, `speedbond bench`, and `speedbond-tui.py` (`bond`).
- Regression tests `TestRelaySharedSessionReassemblyAndBridge` and `TestSmoothWeightedRoundRobin`.

## [0.1.0] - 2026-11-22

### Added

- Initial minimal Go macOS client and Ubuntu relay PoC using QUIC DATAGRAM, cert pinning, token auth, native TUN/utun, and Android `rndis-tether` scripts.
