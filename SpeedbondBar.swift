import Cocoa
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
    private let ctrlURL = "http://127.0.0.1:19454"
    private var statusItem: NSStatusItem!
    private var menu = NSMenu()
    private var timer: Timer?
    private var tickCount = 0

    private var currentStatus: ClientStatus?
    private var socksOn = false
    private var socksDisabledByUs = false
    private var availableLinks: [String] = []
    private var busyText: String?
    private var proofLines: [String] = []
    private var benchRunning = false
    private var lastNote: String?

    func applicationDidFinishLaunching(_ notification: Notification) {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        statusItem.menu = menu
        updateTitle("⚡ Bond …")
        poll()
        timer = Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { [weak self] _ in
            self?.poll()
        }
    }

    private func updateTitle(_ text: String) {
        DispatchQueue.main.async {
            self.statusItem.button?.title = text
        }
    }

    private func poll() {
        tickCount += 1
        let doExtras = (tickCount == 1 || tickCount % 4 == 0)

        let url = URL(string: ctrlURL + "/status")!
        var req = URLRequest(url: url, timeoutInterval: 0.9)
        req.httpMethod = "GET"

        URLSession.shared.dataTask(with: req) { [weak self] data, resp, _ in
            guard let self = self else { return }
            var decoded: ClientStatus? = nil
            if let data = data, let http = resp as? HTTPURLResponse, http.statusCode == 200 {
                decoded = try? JSONDecoder().decode(ClientStatus.self, from: data)
            }
            if doExtras {
                let socks = self.detectSocksProxy()
                let avail = self.detectAvailableLinks()
                DispatchQueue.main.async {
                    self.socksOn = socks
                    self.availableLinks = avail
                    self.currentStatus = decoded
                    self.render()
                }
            } else {
                DispatchQueue.main.async {
                    self.currentStatus = decoded
                    self.render()
                }
            }
        }.resume()
    }

    private func render() {
        if let busy = busyText {
            updateTitle("⏳ \(busy)")
        } else if benchRunning {
            updateTitle("⏱ Proving…")
        } else if let st = currentStatus {
            let speed = st.total_rx_mbps + st.total_tx_mbps
            if st.active_links >= 2 {
                if speed >= 0.2 {
                    updateTitle(String(format: "🟢 %dx ↓%.1fM", st.active_links, st.total_rx_mbps))
                } else {
                    updateTitle("🟢 BOND \(st.active_links)x")
                }
            } else if st.active_links == 1 {
                if speed >= 0.2 {
                    updateTitle(String(format: "🟡 1x ↓%.1fM", st.total_rx_mbps))
                } else {
                    updateTitle("🟡 1x Link")
                }
            } else {
                updateTitle("🔴 Reconnecting")
            }
        } else {
            updateTitle("⚡ Bond OFF")
        }

        menu.removeAllItems()

        if let st = currentStatus {
            let badge = st.active_links >= 2
                ? "🟢 BONDED · \(st.active_links)/\(st.total_links) links active"
                : (st.active_links == 1 ? "🟡 SINGLE LINK · 1/\(st.total_links) active" : "🔴 RECONNECTING")
            addLabel(badge, bold: true)
            addLabel("Tunnel \(st.tunnel) (\(st.tunnel_ip)) · Relay \(st.server)")
            addLabel(String(format: "Total: ↓ %.2f Mbps   ↑ %.2f Mbps   (reordered: %llu, gaps: %llu)",
                            st.total_rx_mbps, st.total_tx_mbps,
                            st.reassembler.reordered, st.reassembler.gaps_skipped))

            if socksOn {
                let warn = NSMenuItem(title: "⚠️ macOS SOCKS5 ON (bypassing bond!) — Click to turn OFF",
                                      action: #selector(toggleSocks), keyEquivalent: "")
                warn.target = self
                menu.addItem(warn)
            }

            menu.addItem(.separator())
            addLabel("ACTIVE LINKS (click to pause/resume)", bold: true)
            for link in st.links {
                let icon = link.state == "UP" ? "🟢" : (link.paused ? "⏸" : "🔴")
                let label: String
                if link.state == "UP" {
                    label = String(format: "%@ %-7s  %2.0f%% · %3.0fms · ↓%4.1fM ↑%3.1fM",
                                   icon, (link.name as NSString).utf8String!,
                                   link.weight_pct, link.rtt_ms, link.rx_mbps, link.tx_mbps)
                } else if link.paused {
                    label = "\(icon) \(link.name)   PAUSED (click to resume)"
                } else {
                    let err = link.last_error.map { " (\($0.prefix(34)))" } ?? ""
                    label = "\(icon) \(link.name)   \(link.state)\(err)"
                }
                let item = NSMenuItem(title: label, action: #selector(toggleLink(_:)), keyEquivalent: "")
                item.target = self
                item.representedObject = [link.name, link.paused ? "resume" : "pause"]
                menu.addItem(item)
            }

            // One-click hot-add for ANY detected interface/bridge not yet in the session
            let activeNames = Set(st.links.map { $0.name })
            let missing = availableLinks.filter { spec in
                let name = spec.split(separator: "=").first.map(String.init) ?? spec
                return !activeNames.contains(name)
            }
            if !missing.isEmpty {
                menu.addItem(.separator())
                for spec in missing {
                    let item = NSMenuItem(title: "➕ Hot-Add \(spec) Now",
                                          action: #selector(hotAddSpec(_:)), keyEquivalent: "")
                    item.target = self
                    item.representedObject = spec
                    menu.addItem(item)
                }
            }
        } else {
            addLabel("⚡ Speedbond is OFF", bold: true)
            let detected = availableLinks.isEmpty ? "none" : availableLinks.joined(separator: ", ")
            addLabel("Available links: \(detected)")
        }

        if let note = lastNote {
            menu.addItem(.separator())
            addLabel("ℹ️ \(note)")
        }

        if !proofLines.isEmpty {
            menu.addItem(.separator())
            addLabel(benchRunning ? "⏱ SPEED PROOF (running…)" : "✅ SPEED PROOF RESULTS", bold: true)
            for line in proofLines {
                addLabel("   \(line)")
            }
        }

        menu.addItem(.separator())

        if currentStatus != nil {
            let benchItem = NSMenuItem(
                title: benchRunning ? "⏱ Speed Proof Running…" : "⚡ Run Speed Proof (Solo vs Bonded)",
                action: benchRunning ? nil : #selector(runSpeedProof),
                keyEquivalent: "b"
            )
            benchItem.target = self
            menu.addItem(benchItem)

            let addItem = NSMenuItem(title: "➕ Add Link (Phone / Wi-Fi / Bridge)…",
                                     action: #selector(addLinkDialog), keyEquivalent: "a")
            addItem.target = self
            menu.addItem(addItem)

            let stopItem = NSMenuItem(title: "■ Stop Bonding", action: #selector(stopBonding), keyEquivalent: "s")
            stopItem.target = self
            menu.addItem(stopItem)
        } else {
            let startItem = NSMenuItem(title: "▶ Start Bonding (Alfa + Phone / Wi-Fi)",
                                       action: #selector(startBonding), keyEquivalent: "s")
            startItem.target = self
            menu.addItem(startItem)
        }

        let phoneItem = NSMenuItem(title: "📱 Connect Android USB Tether (rndis-tether)",
                                   action: #selector(connectPhone), keyEquivalent: "p")
        phoneItem.target = self
        menu.addItem(phoneItem)

        let socksItem = NSMenuItem(title: "🌐 macOS SOCKS5 Proxy: \(socksOn ? "ON → Turn OFF" : "OFF → Turn ON")",
                                   action: #selector(toggleSocks), keyEquivalent: "")
        socksItem.target = self
        menu.addItem(socksItem)

        let tuiItem = NSMenuItem(title: "📊 Open Full TUI Monitor (bond)",
                                 action: #selector(openTUI), keyEquivalent: "t")
        tuiItem.target = self
        menu.addItem(tuiItem)

        menu.addItem(.separator())
        let quitItem = NSMenuItem(title: "Quit SpeedbondBar", action: #selector(quitApp), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)
    }

    private func addLabel(_ text: String, bold: Bool = false) {
        let item = NSMenuItem(title: text, action: nil, keyEquivalent: "")
        if bold {
            item.attributedTitle = NSAttributedString(
                string: text,
                attributes: [.font: NSFont.boldSystemFont(ofSize: 12.5)]
            )
        } else {
            item.attributedTitle = NSAttributedString(
                string: text,
                attributes: [.font: NSFont.monospacedSystemFont(ofSize: 11.5, weight: .regular)]
            )
        }
        menu.addItem(item)
    }

    // MARK: - Actions

    @objc private func startBonding() {
        busyText = "Starting…"
        lastNote = nil
        render()
        DispatchQueue.global(qos: .userInitiated).async {
            // 1. Prepare Alfa bridge + token env as normal user
            let prep = self.runShell("\(self.baseDir)/start-bond.sh --prepare")
            guard prep.code == 0 else {
                DispatchQueue.main.async {
                    self.busyText = nil
                    self.lastNote = "Prepare failed: \(prep.out.prefix(70))"
                    self.render()
                }
                return
            }

            // 2. Disable macOS SOCKS5 so browsers route through bonded tunnel
            if self.detectSocksProxy() {
                _ = self.runShell("networksetup -setsocksfirewallproxystate Wi-Fi off")
                self.socksDisabledByUs = true
            }

            // 3. Launch root speedbond client via native macOS Touch ID / admin prompt
            let launchCmd = """
            nohup /bin/bash -c 'set -a; source /tmp/speedbond-launch.env; rm -f /tmp/speedbond-launch.env; exec \(self.baseDir)/speedbond client -server "$SPEEDBOND_SERVER" -links "$SPEEDBOND_LINKS" -pin "$SPEEDBOND_PIN"' >/tmp/open-speedify-client.log 2>&1 </dev/null &
            """
            let ok = self.runAdminShell(launchCmd)
            DispatchQueue.main.async {
                self.busyText = nil
                if ok.code != 0 {
                    _ = self.runShell("rm -f /tmp/speedbond-launch.env")
                    self.lastNote = "Start cancelled/failed: \(ok.out.prefix(60))"
                } else {
                    self.lastNote = "Started with: \(prep.out.trimmingCharacters(in: .whitespacesAndNewlines))"
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
                _ = self.runShell("networksetup -setsocksfirewallproxystate Wi-Fi on")
                self.socksDisabledByUs = false
            }
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.6) {
                self.busyText = nil
                self.lastNote = "Stopped cleanly (routes restored)"
                self.poll()
            }
        }
    }

    @objc private func toggleLink(_ sender: NSMenuItem) {
        guard let pair = sender.representedObject as? [String], pair.count == 2 else { return }
        let name = pair[0].addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? pair[0]
        let action = pair[1]
        postControl("/links/\(action)?name=\(name)") { [weak self] err in
            DispatchQueue.main.async {
                self?.lastNote = err ?? "\(action.capitalized)d \(pair[0])"
                self?.poll()
            }
        }
    }

    @objc private func hotAddSpec(_ sender: NSMenuItem) {
        guard let spec = sender.representedObject as? String else { return }
        hotAddLink(spec)
    }

    private func hotAddLink(_ spec: String) {
        let q = spec.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? spec
        postControl("/links/add?spec=\(q)") { [weak self] err in
            DispatchQueue.main.async {
                self?.lastNote = err ?? "Added \(spec) to live bond!"
                self?.poll()
            }
        }
    }

    @objc private func addLinkDialog() {
        NSApp.activate(ignoringOtherApps: true)
        let alert = NSAlert()
        alert.messageText = "Add Link to Bonded Session"
        alert.informativeText = "Enter interface name (e.g. feth50, en5, en0) or UDP bridge (e.g. alfa=127.0.0.1:4546):"
        let input = NSTextField(frame: NSRect(x: 0, y: 0, width: 280, height: 24))
        input.stringValue = availableLinks.first(where: { $0 != "en0" }) ?? "feth50"
        alert.accessoryView = input
        alert.addButton(withTitle: "Add")
        alert.addButton(withTitle: "Cancel")
        if alert.runModal() == .alertFirstButtonReturn {
            let spec = input.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !spec.isEmpty else { return }
            hotAddLink(spec)
        }
    }

    @objc private func connectPhone() {
        busyText = "Phone USB…"
        render()
        DispatchQueue.global(qos: .userInitiated).async {
            let cmd = "/opt/homebrew/bin/rndis-tether install && /opt/homebrew/bin/rndis-tether config prefer-phone no && /opt/homebrew/bin/rndis-tether config tx-batch 1 && /opt/homebrew/bin/rndis-tether connect"
            let res = self.runAdminShell(cmd)
            var found: String? = nil
            if res.code == 0 {
                for _ in 0..<15 {
                    if let dev = self.detectPhoneInterface() {
                        found = dev
                        break
                    }
                    Thread.sleep(forTimeInterval: 0.5)
                }
            }
            DispatchQueue.main.async {
                self.busyText = nil
                if let dev = found {
                    if !self.availableLinks.contains(dev) {
                        self.availableLinks.append(dev)
                    }
                    if self.currentStatus != nil {
                        self.hotAddLink(dev)
                    } else {
                        self.lastNote = "Phone ready on \(dev)! Click Start Bonding."
                        self.render()
                    }
                } else {
                    self.lastNote = res.code == 0
                        ? "Phone not detected — enable USB Tethering on Android first"
                        : "rndis-tether error: \(res.out.prefix(55))"
                    self.render()
                }
            }
        }
    }

    @objc private func runSpeedProof() {
        guard !benchRunning else { return }
        benchRunning = true
        proofLines = ["Testing each link solo, then all bonded (~25s)…"]
        render()
        DispatchQueue.global(qos: .userInitiated).async {
            let res = self.runShell("\(self.baseDir)/speedbond bench -seconds 6 -streams 4")
            let lines = res.out
                .split(separator: "\n")
                .map { String($0).trimmingCharacters(in: .whitespaces) }
                .filter { !$0.isEmpty }
            DispatchQueue.main.async {
                self.benchRunning = false
                self.proofLines = lines.isEmpty ? ["Benchmark failed (exit \(res.code))"] : lines
                self.render()
                if let summary = lines.first(where: { $0.hasPrefix("PROOF:") }) ?? lines.last {
                    self.notify("Speedbond Speed Proof", summary)
                }
            }
        }
    }

    @objc private func toggleSocks() {
        let target = socksOn ? "off" : "on"
        DispatchQueue.global(qos: .userInitiated).async {
            _ = self.runShell("networksetup -setsocksfirewallproxystate Wi-Fi \(target)")
            let nowOn = self.detectSocksProxy()
            DispatchQueue.main.async {
                self.socksOn = nowOn
                self.lastNote = "macOS SOCKS5 proxy turned \(nowOn ? "ON" : "OFF")"
                self.render()
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

    private func detectSocksProxy() -> Bool {
        let res = runShell("networksetup -getsocksfirewallproxy Wi-Fi")
        return res.code == 0 && res.out.contains("Enabled: Yes")
    }

    private func detectAvailableLinks() -> [String] {
        var out: [String] = []
        if FileManager.default.fileExists(atPath: "/Users/eminmahrt/Developer/alfa-vm/monitor.sock") {
            out.append("alfa=127.0.0.1:4546")
        }
        let res = runShell("\(baseDir)/speedbond interfaces | awk '$2 !~ /^(utun|awdl|llw|bridge|ap|lo)/ && $0 ~ /[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+\\// { print $2 }'")
        if res.code == 0 {
            for line in res.out.split(separator: "\n") {
                let name = String(line).trimmingCharacters(in: .whitespaces)
                if !name.isEmpty && !out.contains(name) {
                    out.append(name)
                }
            }
        }
        return out
    }

    private func detectPhoneInterface() -> String? {
        for dev in ["feth50", "en4", "en5", "en6"] {
            let res = runShell("ipconfig getifaddr \(dev)")
            if res.code == 0 && !res.out.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                return dev
            }
        }
        return nil
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
