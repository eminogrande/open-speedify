import Cocoa
import CoreWLAN
import Foundation

struct ReassemblerStats: Decodable {
    let delivered: UInt64
    let reordered: UInt64
    let gaps_skipped: UInt64
    let duplicates: UInt64
    let pending: Int
}

struct LinkStatus: Decodable {
    let id: Int
    let name: String
    let spec: String
    let interface: String?
    let endpoint: String?
    let state: String
    let paused: Bool
    let last_error: String?
    let weight: Int
    let weight_pct: Double
    let rtt_ms: Double
    let min_rtt_ms: Double
    let loss_pct: Double
    let tx_mbps: Double
    let rx_mbps: Double
    let sent_packets: UInt64
    let recv_packets: UInt64
}

struct ClientStatus: Decodable {
    let running: Bool
    let server: String
    let tunnel: String
    let tunnel_ip: String
    let mtu: Int
    let routing: Bool
    let uptime_sec: Int64
    let active_links: Int
    let total_links: Int
    let total_tx_mbps: Double
    let total_rx_mbps: Double
    let reassembler: ReassemblerStats
    let links: [LinkStatus]
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    private let baseDir = "/Users/eminmahrt/Developer/open-speedify"
    private let alfaDir = "/Users/eminmahrt/Developer/alfa-vm"
    private let ctrlURL = "http://127.0.0.1:19454"

    private var statusItem: NSStatusItem!
    private var menu = NSMenu()
    private var timer: Timer?
    private var tickCount = 0

    private var currentStatus: ClientStatus?
    private var socksDisabledByUs = false
    private var busyText: String?
    private var proofLines: [String] = []
    private var benchRunning = false
    private var lastNote: String?
    private var autopilotBusy = false
    private var lastAlfaHeal: Date = .distantPast
    private var lastPhoneHeal: Date = .distantPast

    func applicationDidFinishLaunching(_ notification: Notification) {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        statusItem.menu = menu
        updateTitle("⚪ Bond …")
        poll()
        timer = Timer.scheduledTimer(withTimeInterval: 1.5, repeats: true) { [weak self] _ in
            self?.poll()
        }
    }

    private func updateTitle(_ text: String) {
        DispatchQueue.main.async {
            self.statusItem.button?.title = text
        }
    }

    // MARK: - Friendly Human Labels

    private func friendlyName(_ name: String) -> String {
        switch name {
        case "en0": return "Mac Wi-Fi"
        case "alfa": return "Alfa Antenna"
        case "feth50": return "Android Phone (USB)"
        default:
            if name.hasPrefix("en"), let num = Int(name.dropFirst(2)) {
                return num >= 7 ? "iPhone (USB)" : "USB Ethernet (\(name))"
            }
            return name
        }
    }

    private func qualityLabel(_ link: LinkStatus) -> String {
        if link.rtt_ms <= 0 { return "ready" }
        if link.loss_pct >= 10 { return "weak" }
        if link.rtt_ms < 250 { return "fast" }
        if link.rtt_ms < 450 { return "good" }
        return "slow"
    }

    private func friendlyProofLine(_ raw: String) -> String {
        if raw.hasPrefix("SOLO ") {
            let parts = raw.split(whereSeparator: { $0.isWhitespace }).map(String.init)
            if parts.count >= 4 {
                let name = friendlyName(parts[1])
                if raw.contains("err:") {
                    return "\(name) alone: no internet"
                }
                return "\(name) alone: \(parts[2]) Mbps"
            }
        }
        if raw.hasPrefix("BONDED ") {
            let parts = raw.split(whereSeparator: { $0.isWhitespace }).map(String.init)
            if parts.count >= 4 {
                return "Combined: \(parts[2]) Mbps"
            }
        }
        if raw.hasPrefix("PROOF:") {
            if let open = raw.firstIndex(of: "("), let close = raw.firstIndex(of: ")") {
                let detail = String(raw[raw.index(after: open)..<close])
                return "✅ Result: \(detail)"
            }
            return "✅ \(raw.replacingOccurrences(of: "PROOF: ", with: ""))"
        }
        if raw.contains("failed:") {
            return "Test couldn't finish — check that internet is working"
        }
        return raw
    }

    // MARK: - Polling + Self-Healing Autopilot

    private func poll() {
        tickCount += 1
        let runAutopilot = (tickCount == 2 || tickCount % 10 == 0)

        let url = URL(string: ctrlURL + "/status")!
        let req = URLRequest(url: url, timeoutInterval: 1.0)
        URLSession.shared.dataTask(with: req) { [weak self] data, resp, _ in
            guard let self = self else { return }
            var decoded: ClientStatus? = nil
            if let data = data, let http = resp as? HTTPURLResponse, http.statusCode == 200 {
                decoded = try? JSONDecoder().decode(ClientStatus.self, from: data)
            }
            DispatchQueue.main.async {
                self.currentStatus = decoded
                self.render()
            }
            if runAutopilot, let st = decoded, !self.benchRunning {
                self.autopilotCheck(status: st)
            }
        }.resume()
    }

    /// Fully automatic background discovery & self-healing:
    /// - Keeps macOS SOCKS5 proxy OFF while bonded so browsers always use VPN
    /// - Auto-joins any newly plugged phone, Ethernet, or Wi-Fi interface
    /// - Auto-heals Android USB tether (rndis-tether) & Alfa USB antenna after Mac sleep
    private func autopilotCheck(status st: ClientStatus) {
        guard !autopilotBusy else { return }
        autopilotBusy = true
        DispatchQueue.global(qos: .utility).async { [weak self] in
            defer { self?.autopilotBusy = false }
            guard let self = self else { return }

            // 1. Prevent alfa-autoswitch from hijacking Chrome back to local SOCKS5
            if self.detectSocksProxy() {
                _ = self.runShell("echo off > /tmp/alfa-mode; networksetup -setsocksfirewallproxystate Wi-Fi off")
            }

            let existing = Dictionary(uniqueKeysWithValues: st.links.map { ($0.name, $0) })

            // 2. Auto-connect Android USB tether if Samsung/Android plugged in but feth50 offline
            let hasAndroidUSB = self.runShell("ioreg -p IOUSB -w0 2>/dev/null | grep -qiE 'SAMSUNG_Android|Android'").code == 0
            if hasAndroidUSB && self.runShell("ipconfig getifaddr feth50").out.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                if Date().timeIntervalSince(self.lastPhoneHeal) > 30 {
                    self.lastPhoneHeal = Date()
                    _ = self.runShell("rndis-tether connect 2>/dev/null || true")
                }
            }

            // 3. Auto-join ANY active physical IPv4 interface (Wi-Fi, Android feth50, iPhone, Ethernet)
            let ifacesOut = self.runShell("\(self.baseDir)/speedbond interfaces | awk '$2 !~ /^(utun|awdl|llw|bridge|ap|lo)/ && $0 ~ /[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+\\// { print $2 }'").out
            for line in ifacesOut.split(separator: "\n") {
                let dev = String(line).trimmingCharacters(in: .whitespaces)
                if !dev.isEmpty && existing[dev] == nil {
                    self.postControlSync("/links/add?spec=\(dev)")
                    DispatchQueue.main.async {
                        self.lastNote = "Auto-connected \(self.friendlyName(dev))"
                    }
                }
            }

            // 4. Auto-heal & auto-join Alfa USB antenna if plugged in and Alfa VM is running
            let hasAlfaUSB = self.runShell("ioreg -p IOUSB -w0 2>/dev/null | grep -q '802\\.11ac'").code == 0
            let hasAlfaVM = FileManager.default.fileExists(atPath: "\(self.alfaDir)/monitor.sock")
            if hasAlfaUSB && hasAlfaVM {
                let alfaLink = existing["alfa"]
                let alfaNeedsHeal = (alfaLink == nil) || (alfaLink!.state != "UP" && !alfaLink!.paused)
                if alfaNeedsHeal && Date().timeIntervalSince(self.lastAlfaHeal) > 45 {
                    self.lastAlfaHeal = Date()
                    // Re-establish Alfa DHCP + UDP bridge using saved credentials
                    let prep = self.runShell("\(self.baseDir)/start-bond.sh --prepare 'alfa=127.0.0.1:4546'")
                    if prep.code == 0 && alfaLink == nil {
                        self.postControlSync("/links/add?spec=alfa=127.0.0.1:4546")
                        DispatchQueue.main.async {
                            self.lastNote = "Auto-connected Alfa Antenna"
                        }
                    }
                }
            }
        }
    }

    // MARK: - Clean Non-Technical UI

    private func render() {
        if let busy = busyText {
            updateTitle("⏳ \(busy)")
        } else if benchRunning {
            updateTitle("⚡ Testing…")
        } else if let st = currentStatus {
            let speed = st.total_rx_mbps + st.total_tx_mbps
            if st.active_links >= 2 {
                updateTitle(speed >= 0.3
                    ? String(format: "🟢 %dx ↓%.1fM", st.active_links, st.total_rx_mbps)
                    : "🟢 Combined \(st.active_links)x")
            } else if st.active_links == 1 {
                updateTitle(speed >= 0.3
                    ? String(format: "🟡 1x ↓%.1fM", st.total_rx_mbps)
                    : "🟡 1 Connection")
            } else {
                updateTitle("🔴 Connecting…")
            }
        } else {
            updateTitle("⚪ Bond OFF")
        }

        menu.removeAllItems()

        if let st = currentStatus {
            if st.active_links >= 2 {
                addLabel("🟢 Combined · \(st.active_links) connections working together", bold: true)
            } else if st.active_links == 1 {
                addLabel("🟡 Connected · 1 connection active", bold: true)
                addLabel("   Plug in a phone or join a 2nd Wi-Fi to combine speeds", dim: true)
            } else {
                addLabel("🔴 Reconnecting…", bold: true)
            }
            addLabel(String(format: "   ↓ %.1f Mbps  ↑ %.1f Mbps   ·   VPN: Germany 🇩🇪",
                            st.total_rx_mbps, st.total_tx_mbps))

            menu.addItem(.separator())
            addLabel("CONNECTIONS (automatic)", bold: true)

            for link in st.links {
                let name = friendlyName(link.name)
                if link.state == "UP" {
                    let q = qualityLabel(link)
                    let spd = (link.rx_mbps + link.tx_mbps) >= 0.1
                        ? String(format: " · ↓%.1fM ↑%.1fM", link.rx_mbps, link.tx_mbps)
                        : ""
                    addLabel("   🟢 \(name)   (\(q)\(spd))")
                } else if link.paused {
                    addLabel("   ⏸ \(name)   (paused)", dim: true)
                } else {
                    let hint: String
                    if link.name == "feth50" {
                        hint = "waiting for Android data…"
                    } else if friendlyName(link.name) == "iPhone (USB)" {
                        hint = "turn on Personal Hotspot on iPhone"
                    } else {
                        hint = "connecting…"
                    }
                    addLabel("   🟡 \(name)   (\(hint))", dim: true)
                }
            }
        } else {
            addLabel("⚪ Internet Bonding is OFF", bold: true)
            addLabel("   Click Turn ON to combine Wi-Fi, Alfa & phones automatically", dim: true)
        }

        if let note = lastNote {
            menu.addItem(.separator())
            addLabel("ℹ️ \(note)", dim: true)
        }

        if !proofLines.isEmpty {
            menu.addItem(.separator())
            addLabel(benchRunning ? "⚡ Testing speed…" : "📊 Speed Test Result", bold: true)
            for line in proofLines {
                addLabel("   \(friendlyProofLine(line))")
            }
        }

        menu.addItem(.separator())

        if currentStatus != nil {
            let stopItem = NSMenuItem(title: "■ Turn OFF", action: #selector(stopBonding), keyEquivalent: "s")
            stopItem.target = self
            menu.addItem(stopItem)

            let benchItem = NSMenuItem(
                title: benchRunning ? "⚡ Testing Speed…" : "⚡ Test Speed (Alone vs Combined)",
                action: benchRunning ? nil : #selector(runSpeedProof),
                keyEquivalent: "b"
            )
            benchItem.target = self
            menu.addItem(benchItem)
        } else {
            let startItem = NSMenuItem(title: "▶ Turn ON (Auto-Combine All)", action: #selector(startBonding), keyEquivalent: "s")
            startItem.target = self
            menu.addItem(startItem)
        }

        let wifiItem = NSMenuItem(title: "📶 Join Another Wi-Fi…", action: #selector(joinAnotherWiFi), keyEquivalent: "w")
        wifiItem.target = self
        menu.addItem(wifiItem)

        // Tuck technical controls cleanly into Advanced submenu
        menu.addItem(.separator())
        let advRoot = NSMenuItem(title: "Advanced", action: nil, keyEquivalent: "")
        let adv = NSMenu()
        if let st = currentStatus {
            let info = NSMenuItem(title: "Tunnel: \(st.tunnel) (\(st.tunnel_ip)) · \(st.server)", action: nil, keyEquivalent: "")
            info.isEnabled = false
            adv.addItem(info)
            let pkts = NSMenuItem(title: "Packets: \(st.reassembler.delivered) ok · \(st.reassembler.gaps_skipped) gaps", action: nil, keyEquivalent: "")
            pkts.isEnabled = false
            adv.addItem(pkts)
            adv.addItem(.separator())
            for link in st.links {
                let act = link.paused ? "Resume" : "Pause"
                let item = NSMenuItem(title: "\(act) \(friendlyName(link.name)) (\(link.name))",
                                      action: #selector(toggleLink(_:)), keyEquivalent: "")
                item.target = self
                item.representedObject = [link.name, link.paused ? "resume" : "pause"]
                adv.addItem(item)
            }
            adv.addItem(.separator())
            let customAdd = NSMenuItem(title: "Add Custom Interface / Bridge…", action: #selector(addLinkDialog), keyEquivalent: "")
            customAdd.target = self
            adv.addItem(customAdd)
        }
        let repairPhone = NSMenuItem(title: "Repair Android USB Driver (rndis-tether)", action: #selector(connectPhone), keyEquivalent: "")
        repairPhone.target = self
        adv.addItem(repairPhone)

        let tuiItem = NSMenuItem(title: "Open Terminal Monitor (bond)", action: #selector(openTUI), keyEquivalent: "")
        tuiItem.target = self
        adv.addItem(tuiItem)

        advRoot.submenu = adv
        menu.addItem(advRoot)

        let quitItem = NSMenuItem(title: "Quit", action: #selector(quitApp), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)
    }

    private func addLabel(_ text: String, bold: Bool = false, dim: Bool = false) {
        let item = NSMenuItem(title: text, action: nil, keyEquivalent: "")
        var attrs: [NSAttributedString.Key: Any] = [
            .font: bold ? NSFont.boldSystemFont(ofSize: 13) : NSFont.systemFont(ofSize: 12)
        ]
        if dim {
            attrs[.foregroundColor] = NSColor.secondaryLabelColor
        }
        item.attributedTitle = NSAttributedString(string: text, attributes: attrs)
        menu.addItem(item)
    }

    // MARK: - Core Actions

    @objc private func startBonding() {
        busyText = "Starting…"
        lastNote = nil
        render()
        DispatchQueue.global(qos: .userInitiated).async {
            // Ensure Android USB tether is connected if plugged in
            _ = self.runShell("rndis-tether connect 2>/dev/null || true")

            let prep = self.runShell("\(self.baseDir)/start-bond.sh --prepare")
            guard prep.code == 0 else {
                DispatchQueue.main.async {
                    self.busyText = nil
                    self.lastNote = "No active internet connections found yet"
                    self.render()
                }
                return
            }

            // Pause alfa-autoswitch + disable SOCKS5 so all apps use German VPN tunnel
            let alfaMode = self.runShell("cat /tmp/alfa-mode 2>/dev/null || echo auto").out.trimmingCharacters(in: .whitespacesAndNewlines)
            if alfaMode != "off" || self.detectSocksProxy() {
                _ = self.runShell("echo off > /tmp/alfa-mode; networksetup -setsocksfirewallproxystate Wi-Fi off")
                self.socksDisabledByUs = true
            }

            let launchCmd = """
            /usr/bin/python3 -c 'import subprocess; subprocess.Popen(["/bin/bash","-c","set -a; source /tmp/speedbond-launch.env; rm -f /tmp/speedbond-launch.env; exec \(self.baseDir)/speedbond client -server \\"$SPEEDBOND_SERVER\\" -links \\"$SPEEDBOND_LINKS\\" -pin \\"$SPEEDBOND_PIN\\""], stdin=subprocess.DEVNULL, stdout=open("/tmp/open-speedify-client.log","wb"), stderr=subprocess.STDOUT, start_new_session=True)'
            """
            let ok = self.runAdminShell(launchCmd)
            if ok.code == 0 {
                // Reconnect RNDIS after utun opens so Android phone joins immediately
                Thread.sleep(forTimeInterval: 1.5)
                _ = self.runShell("rndis-tether disconnect 2>/dev/null || true; sleep 0.5; rndis-tether connect 2>/dev/null || true")
            }
            DispatchQueue.main.async {
                self.busyText = nil
                if ok.code != 0 {
                    _ = self.runShell("rm -f /tmp/speedbond-launch.env")
                    self.lastNote = "Cancelled"
                } else {
                    self.lastNote = "Bonding started — finding all connections…"
                }
                self.poll()
            }
        }
    }

    @objc private func stopBonding() {
        busyText = "Stopping…"
        render()
        postControl("/stop") { [weak self] _ in
            guard let self = self else { return }
            if self.socksDisabledByUs {
                _ = self.runShell("echo auto > /tmp/alfa-mode; networksetup -setsocksfirewallproxystate Wi-Fi on")
                self.socksDisabledByUs = false
            }
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.6) {
                self.busyText = nil
                self.lastNote = "Turned OFF"
                self.proofLines = []
                self.poll()
            }
        }
    }

    @objc private func runSpeedProof() {
        guard !benchRunning else { return }
        benchRunning = true
        proofLines = ["Measuring each connection alone, then combined (~25s)…"]
        render()
        DispatchQueue.global(qos: .userInitiated).async {
            let res = self.runShell("\(self.baseDir)/speedbond bench -seconds 8 -streams 3")
            let lines = res.out
                .split(separator: "\n")
                .map { String($0).trimmingCharacters(in: .whitespaces) }
                .filter { !$0.isEmpty }
            DispatchQueue.main.async {
                self.benchRunning = false
                self.proofLines = lines.isEmpty ? ["Speed test could not run right now"] : lines
                self.render()
                if let summary = lines.first(where: { $0.hasPrefix("PROOF:") }) ?? lines.last {
                    self.notify("Speed Test Complete", self.friendlyProofLine(summary))
                }
            }
        }
    }

    /// Scans nearby Wi-Fi networks and lets a non-technical user connect either Mac Wi-Fi
    /// or Alfa Antenna to a 2nd Wi-Fi (asking for password only if needed).
    @objc private func joinAnotherWiFi() {
        busyText = "Scanning Wi-Fi…"
        render()
        DispatchQueue.global(qos: .userInitiated).async {
            var ssids: [String] = []
            if let iface = CWWiFiClient.shared().interface(),
               let nets = try? iface.scanForNetworks(withName: nil) {
                let sorted = nets
                    .compactMap { n -> (String, Int)? in
                        guard let s = n.ssid, !s.isEmpty else { return nil }
                        return (s, n.rssiValue)
                    }
                    .sorted { $0.1 > $1.1 }
                for (name, _) in sorted where !ssids.contains(name) {
                    ssids.append(name)
                }
            }
            // Also include macOS saved preferred Wi-Fi names in case scan was privacy-redacted
            let pref = self.runShell("networksetup -listpreferredwirelessnetworks en0 | tail -n +2")
            for line in pref.out.split(separator: "\n") {
                let s = String(line).trimmingCharacters(in: .whitespacesAndNewlines)
                if !s.isEmpty && !ssids.contains(s) {
                    ssids.append(s)
                }
            }

            let hasAlfa = FileManager.default.fileExists(atPath: "\(self.alfaDir)/monitor.sock")
                && self.runShell("ioreg -p IOUSB -w0 2>/dev/null | grep -q '802\\.11ac'").code == 0

            DispatchQueue.main.async {
                self.busyText = nil
                self.render()
                NSApp.activate(ignoringOtherApps: true)

                let alert = NSAlert()
                alert.messageText = "Connect to Another Wi-Fi"
                alert.informativeText = "Pick a network to combine with your other connections. Leave password blank if already saved."

                let container = NSView(frame: NSRect(x: 0, y: 0, width: 320, height: hasAlfa ? 96 : 64))

                let netCombo = NSComboBox(frame: NSRect(x: 0, y: hasAlfa ? 66 : 34, width: 320, height: 26))
                netCombo.isEditable = true
                netCombo.addItems(withObjectValues: ssids)
                if let first = ssids.first { netCombo.stringValue = first }
                netCombo.placeholderString = "Wi-Fi name (e.g. Infinix HOT 60i)"
                container.addSubview(netCombo)

                let passField = NSSecureTextField(frame: NSRect(x: 0, y: hasAlfa ? 34 : 2, width: 320, height: 24))
                passField.placeholderString = "Wi-Fi password (blank if already saved)"
                container.addSubview(passField)

                let targetPopup = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 320, height: 26))
                if hasAlfa {
                    targetPopup.addItems(withTitles: [
                        "Use Mac Wi-Fi (keeps Alfa on its current Wi-Fi)",
                        "Use Alfa Antenna (keeps Mac Wi-Fi on its current Wi-Fi)"
                    ])
                    container.addSubview(targetPopup)
                }

                alert.accessoryView = container
                alert.addButton(withTitle: "Connect")
                alert.addButton(withTitle: "Cancel")

                guard alert.runModal() == .alertFirstButtonReturn else { return }
                let ssid = netCombo.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
                let pass = passField.stringValue
                guard !ssid.isEmpty else { return }
                let useAlfa = hasAlfa && targetPopup.indexOfSelectedItem == 1

                self.busyText = "Joining \(ssid)…"
                self.render()
                DispatchQueue.global(qos: .userInitiated).async {
                    let res: (code: Int32, out: String)
                    if useAlfa {
                        res = self.runExec("/bin/bash", ["\(self.alfaDir)/alfa.sh", "wifi", ssid, pass])
                        if res.code == 0 {
                            _ = self.runShell("\(self.baseDir)/start-bond.sh --prepare 'alfa=127.0.0.1:4546'")
                            self.postControlSync("/links/add?spec=alfa=127.0.0.1:4546")
                        }
                    } else {
                        var args = ["-setairportnetwork", "en0", ssid]
                        if !pass.isEmpty { args.append(pass) }
                        res = self.runExec("/usr/sbin/networksetup", args)
                        if res.code == 0 {
                            self.postControlSync("/links/add?spec=en0")
                        }
                    }
                    DispatchQueue.main.async {
                        self.busyText = nil
                        self.lastNote = res.code == 0
                            ? "Joined \(ssid) on \(useAlfa ? "Alfa Antenna" : "Mac Wi-Fi")!"
                            : "Could not join \(ssid) — check password"
                        self.poll()
                    }
                }
            }
        }
    }

    // MARK: - Advanced Actions

    @objc private func toggleLink(_ sender: NSMenuItem) {
        guard let pair = sender.representedObject as? [String], pair.count == 2 else { return }
        let name = pair[0].addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? pair[0]
        let action = pair[1]
        postControl("/links/\(action)?name=\(name)") { [weak self] err in
            DispatchQueue.main.async {
                self?.lastNote = err ?? "\(action.capitalized)d \(self?.friendlyName(pair[0]) ?? pair[0])"
                self?.poll()
            }
        }
    }

    @objc private func addLinkDialog() {
        NSApp.activate(ignoringOtherApps: true)
        let alert = NSAlert()
        alert.messageText = "Add Custom Link"
        alert.informativeText = "Interface name (e.g. feth50, en5, en0) or bridge (alfa=127.0.0.1:4546):"
        let input = NSTextField(frame: NSRect(x: 0, y: 0, width: 280, height: 24))
        input.stringValue = "feth50"
        alert.accessoryView = input
        alert.addButton(withTitle: "Add")
        alert.addButton(withTitle: "Cancel")
        if alert.runModal() == .alertFirstButtonReturn {
            let spec = input.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !spec.isEmpty else { return }
            let q = spec.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? spec
            postControl("/links/add?spec=\(q)") { [weak self] err in
                DispatchQueue.main.async {
                    self?.lastNote = err ?? "Added \(spec)"
                    self?.poll()
                }
            }
        }
    }

    @objc private func connectPhone() {
        busyText = "Repairing Phone…"
        render()
        DispatchQueue.global(qos: .userInitiated).async {
            let cmd = "/opt/homebrew/bin/rndis-tether install && /opt/homebrew/bin/rndis-tether config prefer-phone no && /opt/homebrew/bin/rndis-tether config tx-batch 1 && /opt/homebrew/bin/rndis-tether connect"
            let res = self.runAdminShell(cmd)
            if res.code == 0 {
                Thread.sleep(forTimeInterval: 2.0)
                self.postControlSync("/links/add?spec=feth50")
            }
            DispatchQueue.main.async {
                self.busyText = nil
                self.lastNote = res.code == 0
                    ? "Android USB ready — will combine automatically"
                    : "Turn on USB Tethering on your Android phone first"
                self.poll()
            }
        }
    }

    @objc private func openTUI() {
        _ = runShell("osascript -e 'tell application \"Terminal\" to do script \"bond\"' -e 'tell application \"Terminal\" to activate'")
    }

    @objc private func quitApp() {
        NSApp.terminate(nil)
    }

    // MARK: - Helpers

    private func postControl(_ path: String, done: @escaping (String?) -> Void) {
        guard let url = URL(string: ctrlURL + path) else { done("bad url"); return }
        var req = URLRequest(url: url, timeoutInterval: 3.0)
        req.httpMethod = "POST"
        URLSession.shared.dataTask(with: req) { data, resp, err in
            if let err = err { done(err.localizedDescription); return }
            if let http = resp as? HTTPURLResponse, http.statusCode != 200 {
                let body = data.flatMap { String(data: $0, encoding: .utf8) } ?? "error \(http.statusCode)"
                done(body.trimmingCharacters(in: .whitespacesAndNewlines))
                return
            }
            done(nil)
        }.resume()
    }

    private func postControlSync(_ path: String) {
        let sem = DispatchSemaphore(value: 0)
        postControl(path) { _ in sem.signal() }
        _ = sem.wait(timeout: .now() + 3.0)
    }

    private func detectSocksProxy() -> Bool {
        let res = runShell("networksetup -getsocksfirewallproxy Wi-Fi")
        return res.code == 0 && res.out.contains("Enabled: Yes")
    }

    private func notify(_ title: String, _ body: String) {
        let safeTitle = title.replacingOccurrences(of: "\"", with: "'")
        let safeBody = body.replacingOccurrences(of: "\"", with: "'")
        _ = runShell("osascript -e 'display notification \"\(safeBody)\" with title \"\(safeTitle)\"'")
    }

    private func runAdminShell(_ cmd: String) -> (code: Int32, out: String) {
        let escaped = cmd
            .replacingOccurrences(of: "\\", with: "\\\\")
            .replacingOccurrences(of: "\"", with: "\\\"")
        let script = "do shell script \"\(escaped)\" with administrator privileges"
        return runExec("/usr/bin/osascript", ["-e", script])
    }

    private func runShell(_ cmd: String) -> (code: Int32, out: String) {
        return runExec("/bin/bash", ["-c", "export PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin; " + cmd])
    }

    private func runExec(_ path: String, _ args: [String]) -> (code: Int32, out: String) {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: path)
        p.arguments = args
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        do {
            try p.run()
            let data = pipe.fileHandleForReading.readDataToEndOfFile()
            p.waitUntilExit()
            return (p.terminationStatus, String(data: data, encoding: .utf8) ?? "")
        } catch {
            return (1, error.localizedDescription)
        }
    }
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let delegate = AppDelegate()
app.delegate = delegate
app.run()
