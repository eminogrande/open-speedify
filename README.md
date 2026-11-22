# Open Speedify

Experimental macOS + Android link bonding over a Linux relay. This is a research proof of concept, **not a production VPN or a Speedify competitor yet**.

## Goal

Use Wi-Fi and phone bandwidth together, including within one TCP connection, while keeping the session alive when a link changes. A relay is required because Wi-Fi and cellular normally have different public IP addresses.

```text
                 ┌── QUIC DATAGRAM over Wi-Fi ──┐
macOS applications                                ├── Linux relay ── Internet
                 └── QUIC DATAGRAM over Android ┘   one public IP
```

The client creates one encrypted QUIC connection per selected interface. IP packets receive sequence numbers, are distributed across links, and pass through a bounded reorder buffer. The relay reassembles packets, applies NAT, and sends return traffic over the same session. TLS uses a pinned relay certificate; a shared token authenticates links.

## Verified on 2026-11-22

- `go test -race ./...` passes, including two real loopback QUIC links, ordered delivery, duplicate suppression, gap recovery, and transport failover after closing one link.
- A direct Wi-Fi session connects to the deployed Ubuntu relay over UDP 443.
- Full Wi-Fi tunneling works in both directions; the Mac observed the relay public IP `188.34.194.25`.
- Android USB tethering joined the same session through `feth50`. Both Wi-Fi and Android had nonzero sent/received packet counters.
- Normal Wi-Fi, **without Speedbond**, downloaded OVH's 10 MB test object in 17.999 seconds: 582,584 bytes/s, approximately **4.66 Mbps**.

## Not yet proven

- **No bonded throughput measurement has completed.** No speedup or 80% combined-capacity target is claimed.
- Clean physical unplug failover has not completed; repeated link disconnections were observed.
- Round-robin scheduling can perform badly with mismatched latency/capacity. Adaptive scheduling and automatic backup selection are not implemented.
- This is IPv4-only, has no GUI, FEC, abuse controls, production security review, or managed multi-user service.
- Android RNDIS setup is fragile on this Mac, and other full-tunnel VPNs interfere with underlay routing.

## Build and test

Requires Go 1.24 or newer.

```bash
go test -race ./...
go build -o speedbond .
GOOS=linux GOARCH=amd64 go build -o speedbond-linux .
```

Useful commands:

```bash
./speedbond interfaces
./speedbond cert -cert relay.crt -key relay.key
```

## Relay setup

On an Ubuntu host with `/dev/net/tun`, `ip`, and `iptables`:

```bash
install -m 755 speedbond-linux /usr/local/sbin/speedbond
/usr/local/sbin/speedbond cert -cert /etc/speedbond/relay.crt -key /etc/speedbond/relay.key
```

Set `SPEEDBOND_TOKEN` to a shared secret of at least 16 characters, then run:

```bash
SPEEDBOND_TOKEN=... /usr/local/sbin/speedbond relay \
  -listen :443 -cert /etc/speedbond/relay.crt -key /etc/speedbond/relay.key
```

The relay configures its TUN interface, forwarding, and narrowly scoped iptables rules. Preserve the printed certificate fingerprint for the client. `speedbond-relay.service` is the example systemd unit used for this PoC.

## macOS client

Disconnect other full-tunnel VPNs first. The client needs root for `utun` and routes. Stop it to remove its routes.

```bash
export SPEEDBOND_TOKEN=...
sudo -E ./speedbond client \
  -server RELAY_IP:443 \
  -links en0,PHONE_INTERFACE \
  -pin RELAY_CERTIFICATE_SHA256
```

The included `start-poc.sh` contains the current PoC relay address and certificate pin and reads the token from this user's macOS Keychain. Do not reuse that pin for a newly generated relay certificate.

### Android USB tethering

macOS does not natively support this Samsung's RNDIS mode. The PoC uses the open-source, user-space [`rndis-tether`](https://github.com/stlk0/rndis-tether) 1.2.0 driver; no kernel extension or SIP change is used.

```bash
brew install stlk0/tap/rndis-tether
sudo rndis-tether install
rndis-tether config prefer-phone no
rndis-tether config tx-batch 1
```

Enable USB tethering on the phone and verify that `rndis-tether status` reports an address on `feth50`. Then run:

```bash
./start-poc.sh
```

The launcher creates the Speedbond tunnel, restarts RNDIS so its DHCP address is restored, and waits for both links. Phone-data use is explicit: running the bonded test sends traffic over the phone.

## Source layout

- `client.go`: interface binding, link reconnects, client tunnel.
- `relay.go`: authenticated sessions, address allocation, relay forwarding.
- `pool.go`, `protocol.go`, `reorder.go`: framing, scheduling, sequencing, gap recovery.
- `tun_darwin.go`, `tun_linux.go`: native TUN and route setup.
- `transport_test.go`, `reorder_test.go`: protocol and failover checks.
- `run-e2e.sh`: destructive-prone research harness for this specific Mac; not a general installer.

## Current status

The client is stopped and normal Wi-Fi routing was restored after testing. See [CHANGELOG.md](CHANGELOG.md) for implementation history and remaining work.
