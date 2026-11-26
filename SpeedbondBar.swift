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
    private var busyText: String?
    private var proofLines: [String] = []
    private var benchRunning = false
    private var lastNote: String?
    private var lastNoteTime: Date = .distantPast
    private var autopilotBusy = false
    private var lastAlfaTry: Date = .distantPast
    private var lastAlfaHeal: Date = .distantPast
    private var lastPhoneHeal: Date = .distantPast

    func applicationDidFinishLaunching(_ notification: Notification) {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        statusItem.menu = menu
        updateTitle("⚪ 🚀")
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

    private func setNote(_ text: String?) {
        lastNote = text
        lastNoteTime = Date()
    }

    // MARK: - Friendly Emoji Labels

    private func friendlyLabel(_ name: String) -> String {
        switch name {
        case "en0": return "📶 Mac Wi-Fi"
        case "alfa": return "📡 Alfa Antenna"
        case "feth50": return "🤖 Android (USB)"
        default:
            if name.hasPrefix("en"), let num = Int(name.dropFirst(2)) {
                return num >= 7 ? "📱 iPhone (USB)" : "🔌 Ethernet (\(name))"
            }
            return "🌐 \(name)"
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
                let name = friendlyLabel(parts[1])
                return raw.contains("err:") ? "\(name): no internet" : "\(name): \(parts[2]) Mbps"
            }
        }
        if raw.hasPrefix("BONDED ") {
            let parts = raw.split(whereSeparator: { $0.isWhitespace }).map(String.init)
            if parts.count >= 4 { return "🚀 Combined: \(parts[2]) Mbps" }
        }
        if raw.hasPrefix("PROOF:") {
            if let open = raw.firstIndex(of: "("), let close = raw.firstIndex(of: ")") {
                return "✅ \(raw[raw.index(after: open)..<close])"
            }
            return "✅ \(raw.replacingOccurrences(of: "PROOF: ", with: ""))"
        }
        return raw
    }

    // MARK: - Polling + Automatic Link Discovery

    private func poll() {
        tickCount += 1
        let runAutopilot = (tickCount == 1 || tickCount % 4 == 0)

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
                if self.lastNote != nil && Date().timeIntervalSince(self.lastNoteTime) > 12 {
                    self.lastNote = nil
                }
                self.render()
            }
            if runAutopilot, let st = decoded, !self.benchRunning {
                self.autopilotCheck(status: st)
            }
        }.resume()
    }

    /// Fully automatic discovery while Open Speedify is ON:
    /// - Keeps macOS system SOCKS5 proxy OFF while bonded (never modifies /tmp/alfa-mode)
    /// - Auto-adds EVERY active physical IPv4 interface (Wi-Fi, iPhone, Android, Ethernet)
    /// - Auto-bridges and hot-adds Alfa Antenna whenever plugged in & VM is up
    /// - Auto-prunes unplugged USB interfaces so link list stays clean
    private func autopilotCheck(status st: ClientStatus) {
        guard !autopilotBusy else { return }
        autopilotBusy = true
        DispatchQueue.global(qos: .utility).async { [weak self] in
            defer { self?.autopilotBusy = false }
            guard let self = self else { return }

            // 1. Keep system SOCKS5 off while bonded (without modifying /tmp/alfa-mode)
            self.clearMacOSSocksIfEnabled()

            let existing = Dictionary(uniqueKeysWithValues: st.links.map { ($0.name, $0) })

            // 2. Auto-connect Android USB tether if Samsung/Android plugged in without feth50 IP
            let hasAndroidUSB = self.runShell("ioreg -p IOUSB -w0 2>/dev/null | grep -qiE 'SAMSUNG_Android|Android'").code == 0
            if hasAndroidUSB && self.runShell("ipconfig getifaddr feth50").out.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                if Date().timeIntervalSince(self.lastPhoneHeal) > 25 {
                    self.lastPhoneHeal = Date()
                    _ = self.runShell("rndis-tether connect 2>/dev/null || true")
                }
            }

            // 3. Discover ALL active physical IPv4 interfaces (skip link-local 169.254.x.x & virtuals)
            let ifacesOut = self.runShell("\(self.baseDir)/speedbond interfaces | awk '$2 !~ /^(utun|awdl|llw|bridge|ap|lo|anpi|anri)/ && $0 ~ /[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+\\// && $0 !~ /169\\.254\\./ { print $2 }'").out
            var liveIfaces = Set<String>()
            for line in ifacesOut.split(separator: "\n") {
                let dev = String(line).trimmingCharacters(in: .whitespaces)
                if !dev.isEmpty { liveIfaces.insert(dev) }
            }

            for dev in liveIfaces.sorted() where existing[dev] == nil {
                self.postControlSync("/links/add?spec=\(dev)")
                DispatchQueue.main.async {
                    self.setNote("Added \(self.friendlyLabel(dev))")
                }
            }

            // Prune unplugged physical USB interfaces that disappeared and are DOWN
            for link in st.links where link.interface != nil && link.name != "en0" {
                if !link.paused && link.state != "UP" && !liveIfaces.contains(link.name) {
                    let exists = self.runShell("ifconfig \(link.name) >/dev/null 2>&1").code == 0
                    if !exists {
                        self.postControlSync("/links/remove?name=\(link.name)")
                    }
                }
            }

            // 4. Auto-join & auto-heal Alfa USB Antenna whenever plugged in & VM is running
            let hasAlfaUSB = self.runShell("ioreg -p IOUSB -w0 2>/dev/null | grep -q '802\\.11ac'").code == 0
            let hasAlfaVM = FileManager.default.fileExists(atPath: "\(self.alfaDir)/monitor.sock")
            let alfaLink = existing["alfa"]

            if hasAlfaUSB && hasAlfaVM {
                if alfaLink == nil {
                    if Date().timeIntervalSince(self.lastAlfaTry) > 8 {
                        self.lastAlfaTry = Date()
                        let prep = self.runShell("\(self.baseDir)/start-bond.sh --setup-alfa")
                        if prep.code == 0 {
                            self.postControlSync("/links/add?spec=alfa=127.0.0.1:4546")
                            DispatchQueue.main.async {
                                self.setNote("Added 📡 Alfa Antenna")
                            }
                        }
                    }
                } else if alfaLink!.state != "UP" && !alfaLink!.paused {
                    if Date().timeIntervalSince(self.lastAlfaHeal) > 20 {
                        self.lastAlfaHeal = Date()
                        _ = self.runShell("\(self.baseDir)/start-bond.sh --setup-alfa --force")
                    }
                }
            } else if let al = alfaLink, al.state != "UP" && !al.paused {
                // Alfa unplugged or turned off -> remove stale link
                self.postControlSync("/links/remove?name=alfa")
            }
        }
    }

    // MARK: - Minimal Emoji-First UI

    private func render() {
        if busyText != nil {
            updateTitle("⏳ 🚀")
        } else if benchRunning {
            updateTitle("⚡ 🚀 …")
        } else if let st = currentStatus {
            let speed = st.total_rx_mbps + st.total_tx_mbps
            if st.active_links >= 2 {
                updateTitle(speed >= 0.5
                    ? String(format: "🟢 🚀 %dx ↓%.1fM", st.active_links, st.total_rx_mbps)
                    : "🟢 🚀 \(st.active_links)x")
            } else if st.active_links == 1 {
                updateTitle(speed >= 0.5
                    ? String(format: "🟡 🚀 1x ↓%.1fM", st.total_rx_mbps)
                    : "🟡 🚀 1x")
            } else {
                updateTitle("🔴 🚀 …")
            }
        } else {
            updateTitle("⚪ 🚀")
        }

        menu.removeAllItems()

        if let busy = busyText {
            addLabel("⏳ \(busy)", bold: true)
        } else if let st = currentStatus {
            if st.active_links >= 2 {
                addLabel("🟢 Combined · \(st.active_links) connections", bold: true)
            } else if st.active_links == 1 {
                addLabel("🟡 Active · 1 connection", bold: true)
            } else {
                addLabel("🔴 Reconnecting…", bold: true)
            }
            addLabel(String(format: "   ↓ %.1f Mbps  ↑ %.1f Mbps  ·  🇩🇪 VPN",
                            st.total_rx_mbps, st.total_tx_mbps), dim: true)

            for link in st.links {
                let label = friendlyLabel(link.name)
                if link.state == "UP" {
                    let q = qualityLabel(link)
                    let spd = (link.rx_mbps + link.tx_mbps) >= 0.2
                        ? String(format: " · ↓%.1fM", link.rx_mbps)
                        : ""
                    addLabel("   🟢 \(label) · \(q)\(spd)")
                } else if link.paused {
                    addLabel("   ⏸ \(label) · paused", dim: true)
                } else {
                    addLabel("   🟡 \(label) · connecting…", dim: true)
                }
            }
        } else {
            addLabel("⚪ Open Speedify OFF", bold: true)
            addLabel("   Combines Alfa, Wi-Fi & phones automatically", dim: true)
        }

        if let note = lastNote {
            addLabel("   ℹ️ \(note)", dim: true)
        }

        if !proofLines.isEmpty {
            menu.addItem(.separator())
            addLabel(benchRunning ? "⚡ Testing speed…" : "📊 Speed Test", bold: true)
            for line in proofLines {
                addLabel("   \(friendlyProofLine(line))")
            }
        }

        menu.addItem(.separator())

        if currentStatus != nil {
            let stopItem = NSMenuItem(title: "🔴 Turn OFF", action: #selector(stopBonding), keyEquivalent: "s")
            stopItem.target = self
            stopItem.isEnabled = (busyText == nil)
            menu.addItem(stopItem)

            let benchItem = NSMenuItem(
                title: benchRunning ? "⚡ Testing…" : "⚡ Speed Test",
                action: benchRunning ? nil : #selector(runSpeedProof),
                keyEquivalent: "b"
            )
            benchItem.target = self
            menu.addItem(benchItem)
        } else {
            let startItem = NSMenuItem(title: "🟢 Turn ON (Combine All)", action: #selector(startBonding), keyEquivalent: "s")
            startItem.target = self
            startItem.isEnabled = (busyText == nil)
            menu.addItem(startItem)
        }

        menu.addItem(.separator())
        let moreRoot = NSMenuItem(title: "🔧 More", action: nil, keyEquivalent: "")
        let more = NSMenu()

        let wifiItem = NSMenuItem(title: "📶 Join Another Wi-Fi…", action: #selector(joinAnotherWiFi), keyEquivalent: "w")
        wifiItem.target = self
        more.addItem(wifiItem)

        if let st = currentStatus {
            more.addItem(.separator())
            let info = NSMenuItem(title: "🌐 \(st.tunnel) (\(st.tunnel_ip)) · \(st.server)", action: nil, keyEquivalent: "")
            info.isEnabled = false
            more.addItem(info)
            for link in st.links {
                let act = link.paused ? "▶ Resume" : "⏸ Pause"
                let item = NSMenuItem(title: "\(act) \(friendlyLabel(link.name))",
                                      action: #selector(toggleLink(_:)), keyEquivalent: "")
                item.target = self
                item.representedObject = [link.name, link.paused ? "resume" : "pause"]
                more.addItem(item)
            }
        }
        more.addItem(.separator())
        let repairPhone = NSMenuItem(title: "🤖 Repair Android USB", action: #selector(connectPhone), keyEquivalent: "")
        repairPhone.target = self
        more.addItem(repairPhone)

        let tuiItem = NSMenuItem(title: "💻 Terminal Monitor (bond)", action: #selector(openTUI), keyEquivalent: "")
        tuiItem.target = self
        more.addItem(tuiItem)

        moreRoot.submenu = more
        menu.addItem(moreRoot)

        let quitItem = NSMenuItem(title: "🚪 Quit", action: #selector(quitApp), keyEquivalent: "q")
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
        proofLines = []
        lastNote = nil
        render()
        DispatchQueue.global(qos: .userInitiated).async {
            _ = self.runShell("rndis-tether connect 2>/dev/null || true")

            let prep = self.runShell("\(self.baseDir)/start-bond.sh --prepare")
            guard prep.code == 0 else {
                DispatchQueue.main.async {
                    self.busyText = nil
                    self.setNote("No internet connections found")
                    self.render()
                }
                return
            }

            // Clear macOS SOCKS5 proxy while bonded (never touch /tmp/alfa-mode)
            self.clearMacOSSocksIfEnabled()

            let launchCmd = """
            /usr/bin/python3 -c 'import subprocess; subprocess.Popen(["/bin/bash","-c","set -a; source /tmp/speedbond-launch.env; rm -f /tmp/speedbond-launch.env; exec \(self.baseDir)/speedbond client -server \\"$SPEEDBOND_SERVER\\" -links \\"$SPEEDBOND_LINKS\\" -pin \\"$SPEEDBOND_PIN\\""], stdin=subprocess.DEVNULL, stdout=open("/tmp/open-speedify-client.log","wb"), stderr=subprocess.STDOUT, start_new_session=True)'
            """
            let ok = self.runAdminShell(launchCmd)
            if ok.code == 0 {
                // Wait briefly for control API to come up so UI transitions smoothly
                for _ in 0..<14 {
                    if self.runShell("curl -sf --max-time 1 \(self.ctrlURL)/status >/dev/null").code == 0 {
                        break
                    }
                    Thread.sleep(forTimeInterval: 0.4)
                }
                _ = self.runShell("rndis-tether disconnect 2>/dev/null || true; sleep 0.3; rndis-tether connect 2>/dev/null || true")
            }
            DispatchQueue.main.async {
                self.busyText = nil
                if ok.code != 0 {
                    _ = self.runShell("rm -f /tmp/speedbond-launch.env")
                    self.setNote("Cancelled")
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
            DispatchQueue.global(qos: .userInitiated).async {
                // Wait briefly for speedbond client to release port 19454
                for _ in 0..<10 {
                    if self.runShell("nc -z 127.0.0.1 19454 2>/dev/null").code != 0 {
                        break
                    }
                    Thread.sleep(forTimeInterval: 0.2)
                }
                // Immediately restore Alfa as default if Alfa is ON
                let mode = self.runShell("cat /tmp/alfa-mode 2>/dev/null || echo on").out.trimmingCharacters(in: .whitespacesAndNewlines)
                if mode != "off" && FileManager.default.fileExists(atPath: "\(self.alfaDir)/monitor.sock") {
                    _ = self.runShell("\(self.alfaDir)/alfa.sh proxy on >/dev/null 2>&1 || true")
                }
                DispatchQueue.main.async {
                    self.busyText = nil
                    self.proofLines = []
                    self.lastNote = nil
                    self.poll()
                }
            }
        }
    }

    @objc private func runSpeedProof() {
        guard !benchRunning else { return }
        benchRunning = true
        proofLines = ["Measuring each connection, then combined (~25s)…"]
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
                    self.notify("⚡ Speed Test Done", self.friendlyProofLine(summary))
                }
            }
        }
    }

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
            let pref = self.runShell("networksetup -listpreferredwirelessnetworks en0 | tail -n +2")
            for line in pref.out.split(separator: "\n") {
                let s = String(line).trimmingCharacters(in: .whitespacesAndNewlines)
                if !s.isEmpty && !ssids.contains(s) { ssids.append(s) }
            }

            let hasAlfa = FileManager.default.fileExists(atPath: "\(self.alfaDir)/monitor.sock")
                && self.runShell("ioreg -p IOUSB -w0 2>/dev/null | grep -q '802\\.11ac'").code == 0

            DispatchQueue.main.async {
                self.busyText = nil
                self.render()
                NSApp.activate(ignoringOtherApps: true)

                let alert = NSAlert()
                alert.messageText = "📶 Join Another Wi-Fi"
                alert.informativeText = "Pick a network. Leave password blank if already saved."

                let container = NSView(frame: NSRect(x: 0, y: 0, width: 320, height: hasAlfa ? 96 : 64))
                let netCombo = NSComboBox(frame: NSRect(x: 0, y: hasAlfa ? 66 : 34, width: 320, height: 26))
                netCombo.isEditable = true
                netCombo.addItems(withObjectValues: ssids)
                if let first = ssids.first { netCombo.stringValue = first }
                netCombo.placeholderString = "Wi-Fi name"
                container.addSubview(netCombo)

                let passField = NSSecureTextField(frame: NSRect(x: 0, y: hasAlfa ? 34 : 2, width: 320, height: 24))
                passField.placeholderString = "Password (blank if saved)"
                container.addSubview(passField)

                let targetPopup = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 320, height: 26))
                if hasAlfa {
                    targetPopup.addItems(withTitles: [
                        "📶 Mac Wi-Fi",
                        "📡 Alfa Antenna"
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
                            _ = self.runShell("\(self.baseDir)/start-bond.sh --setup-alfa --force")
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
                        self.setNote(res.code == 0
                            ? "Joined \(ssid) on \(useAlfa ? "📡 Alfa" : "📶 Mac Wi-Fi")"
                            : "Could not join \(ssid)")
                        self.poll()
                    }
                }
            }
        }
    }

    // MARK: - More Actions

    @objc private func toggleLink(_ sender: NSMenuItem) {
        guard let pair = sender.representedObject as? [String], pair.count == 2 else { return }
        let name = pair[0].addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? pair[0]
        let action = pair[1]
        postControl("/links/\(action)?name=\(name)") { [weak self] _ in
            DispatchQueue.main.async { self?.poll() }
        }
    }

    @objc private func connectPhone() {
        busyText = "Repairing Android…"
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
                self.setNote(res.code == 0 ? "🤖 Android USB ready" : "Enable USB Tethering on Android first")
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

    private func clearMacOSSocksIfEnabled() {
        let check = runShell("scutil --proxy | grep -q 'SOCKSEnable : 1'")
        if check.code == 0 {
            _ = runShell("""
            networksetup -listnetworkserviceorder 2>/dev/null | awk '
              /^\\([0-9]+\\) / { name=$0; sub(/^\\([0-9]+\\) /, "", name); next }
              /\\(Hardware Port:.*Device: en[0-9]+\\)/ && name != "" { print name; name="" }
            ' | while IFS= read -r svc; do
              networksetup -setsocksfirewallproxystate "$svc" off 2>/dev/null || true
            done
            """)
        }
    }

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
