# Changelog

All notable changes to this project are documented here. Dates use ISO format.

## [Unreleased] - 2026-11-22

### Added

- Minimal Go macOS client and Ubuntu relay using one encrypted QUIC DATAGRAM connection per underlay link.
- Certificate pinning, shared-token link authentication, session address allocation, sequencing, bounded reordering, and per-link reconnection.
- Native macOS `utun` and Linux TUN support, relay NAT, and IPv4 route cleanup.
- Loopback integration proof with two QUIC links and failover after closing one link.
- Android RNDIS support through the user-space `rndis-tether` driver and PoC launch/test scripts.
- Systemd relay unit and initial setup/status documentation.

### Fixed

- Skip occupied macOS `utun` units for both `EADDRINUSE` and `EBUSY`.
- Bind link sockets to interface-specific IPv4 addresses and preserve the direct relay host route.
- Bound macOS TUN reads so shutdown cannot remain blocked indefinitely.
- Replace the Linux TUN `os.File` poller path with direct `unix.Read`/`unix.Write`; the poller failed with `read /dev/net/tun: not pollable`, silently dropping all downlink traffic.
- Restore normal Wi-Fi routing after research tests.

### Verified

- `go test -race ./...` passes.
- Direct Wi-Fi establishes a relay session over UDP 443.
- Full Wi-Fi traffic exits through the relay public IP with bidirectional packet counters.
- Android joins the same session and passes packets in both directions.
- Normal Wi-Fi baseline without the tunnel: 10 MB in 17.999 seconds, approximately 4.66 Mbps down.

### Known limitations

- Bonded upload/download throughput and physical unplug failover remain unmeasured.
- Link flaps occurred during the extended session.
- Round-robin scheduling is not adaptive to capacity or latency and may hurt mismatched links.
- RNDIS may need reconnection after Speedbond creates its tunnel; competing VPN system extensions interfere with routing.
- No production security review, adaptive scheduler, GUI, IPv6, FEC, abuse controls, or managed multi-user operation.
