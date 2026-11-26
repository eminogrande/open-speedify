# Open Speedify — Agent / LLM Playbook

Instructions for any AI agent adding a new VPS relay to an existing Open Speedify installation.

## Rules

- **Never commit `relays.json`** (gitignored, `chmod 600`) or hardcode any VPS IP, cert pin, or token into tracked files.
- **Never rebuild `SpeedbondBar.app`** just to add or change a server — it reads `relays.json` dynamically every 1.5s.
- **Never overwrite an existing service on UDP 443** (e.g., Caddy/Nginx HTTP/3). `deploy-relay.sh` automatically checks UDP `:443` and falls back to UDP `:4545` if `:443` is in use.

## Add a New VPS Relay (Standard Flow)

Given a VPS SSH target (e.g., `root@203.0.113.10`) and friendly label (e.g., `"🇰🇪 Nairobi"`):

1. Ensure the local `speedbond` binary exists:
   ```bash
   [[ -x ./speedbond ]] || go build -o speedbond .
   ```
2. Deploy the relay and auto-register it in local `relays.json`:
   ```bash
   ./deploy-relay.sh root@203.0.113.10 "🇰🇪 Nairobi"
   ```
   - Custom SSH key / avoid `Too many authentication failures` with `ssh-agent`:
     ```bash
     SSH_OPTS="-i ~/.ssh/my_key -o IdentitiesOnly=yes" ./deploy-relay.sh root@203.0.113.10 "🇰🇪 Nairobi"
     ```
   - Explicit UDP port (optional 3rd arg, defaults to `443`, or `4545` if `443` is busy):
     ```bash
     ./deploy-relay.sh root@203.0.113.10 "🇰🇪 Nairobi" 4545
     ```
3. Verify reachability and compare latency across all configured servers:
   ```bash
   ./speedbond pick
   ```
   - If the new server shows `FAIL` (`context deadline exceeded`) while `systemctl status speedbond-relay` is `active`, check the **cloud provider firewall / security group** (Hetzner Cloud Firewall, Vultr Firewall, AWS Security Group, Oracle Ingress Rules) and allow inbound **UDP 443** (or **UDP 4545**).
4. Confirm in `SpeedbondBar` (`🔧 More ▸ 🌍 Server`):
   - `⚡ Auto (Fastest)` automatically tests all servers in `relays.json` and connects to the lowest-RTT healthy server on Turn ON.
   - Users can also manually pin any server in `🔧 More ▸ 🌍 Server` (toggle OFF → ON to switch active sessions).

## Manual `relays.json` Format

See [`relays.example.json`](relays.example.json). Each entry supports:
- `name`: display label with flag emoji (e.g. `"🇩🇪 Nuremberg"`)
- `server`: `IPv4:port`
- `pin`: SHA-256 hex digest of `/etc/speedbond/relay.crt` (DER)
- `token`: shared secret from `/etc/speedbond/environment` (or `token_keychain` on macOS)
