#!/usr/bin/env python3
"""Speedbond TUI — zero-dependency monitor & controller for multi-link bonding."""
import curses
import json
import os
import re
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

DIR = os.path.dirname(os.path.realpath(__file__))
BIN = os.path.join(DIR, "speedbond")
CTRL = os.environ.get("SPEEDBOND_CONTROL", "127.0.0.1:19454")
BASE = f"http://{CTRL}"
ENV = {**os.environ, "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}


def api_get(path, timeout=1.2):
    req = urllib.request.Request(BASE + path)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def api_post(path, timeout=2.5):
    req = urllib.request.Request(BASE + path, method="POST", data=b"")
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def run_cmd(cmd, timeout=5):
    try:
        p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=ENV, timeout=timeout)
        return p.returncode, (p.stdout or "").strip()
    except Exception as e:
        return 1, str(e)


def check_socks_proxy():
    code, out = run_cmd(["networksetup", "-getsocksfirewallproxy", "Wi-Fi"], timeout=2)
    if code == 0 and "Enabled: Yes" in out:
        return True
    return False


def toggle_socks_proxy(currently_on):
    state = "off" if currently_on else "on"
    code, out = run_cmd(["networksetup", "-setsocksfirewallproxystate", "Wi-Fi", state], timeout=3)
    return code == 0, out


def detect_candidates(existing_names):
    """Detect physical IPv4 interfaces + Alfa UDP bridge if active."""
    cands = []
    # Check Alfa bridge port or VM
    if "alfa" not in existing_names:
        if os.path.exists("/Users/eminmahrt/Developer/alfa-vm/monitor.sock"):
            cands.append(("alfa=127.0.0.1:4546", "Alfa antenna VM UDP bridge"))
    code, out = run_cmd([BIN, "interfaces"], timeout=2)
    if code == 0:
        for line in out.splitlines():
            m = re.match(r"^\d+\s+(\S+)\s+mtu=\d+\s+\[(.*)\]", line.strip())
            if not m:
                continue
            name, addrs = m.group(1), m.group(2)
            if name in existing_names or name.startswith(("utun", "awdl", "llw", "bridge", "ap")):
                continue
            ip4 = re.findall(r"\b(?:\d{1,3}\.){3}\d{1,3}/\d+\b", addrs)
            if ip4:
                label = "Wi-Fi" if name == "en0" else ("Android USB" if name == "feth50" else "Interface")
                cands.append((name, f"{label} ({ip4[0]})"))
    return cands


def safe_add(win, y, x, text, attr=0):
    h, w = win.getmaxyx()
    if y < 0 or y >= h or x >= w:
        return
    try:
        win.addnstr(y, x, text, max(0, w - x - 1), attr)
    except curses.error:
        pass


def bar(val, vmax, width=14):
    if vmax <= 0:
        vmax = 1.0
    filled = int(min(width, max(0, round((val / vmax) * width))))
    return "█" * filled + "░" * (width - filled)


class App:
    def __init__(self):
        self.lock = threading.Lock()
        self.status = None
        self.err = ""
        self.public_ip = "…"
        self.socks_on = False
        self.sel = 0
        self.peak_rx = 0.0
        self.peak_tx = 0.0
        self.msg = ""
        self.msg_until = 0.0
        self.bench_running = False
        self.bench_lines = []
        self.stop_evt = threading.Event()

    def flash(self, text, secs=4.0):
        with self.lock:
            self.msg = text
            self.msg_until = time.time() + secs

    def poll_loop(self):
        last_ip_check = 0.0
        last_socks_check = 0.0
        while not self.stop_evt.is_set():
            try:
                st = api_get("/status")
                with self.lock:
                    self.status = st
                    self.err = ""
                    self.peak_rx = max(self.peak_rx, st.get("total_rx_mbps", 0.0))
                    self.peak_tx = max(self.peak_tx, st.get("total_tx_mbps", 0.0))
            except Exception as e:
                with self.lock:
                    self.status = None
                    self.err = str(e)
            now = time.time()
            if now - last_socks_check > 3.0:
                socks = check_socks_proxy()
                with self.lock:
                    self.socks_on = socks
                last_socks_check = now
            if now - last_ip_check > 10.0:
                code, out = run_cmd(["curl", "-4", "-s", "--noproxy", "*", "--max-time", "3", "https://api.ipify.org"], timeout=4)
                if code == 0 and out and len(out) < 40:
                    with self.lock:
                        self.public_ip = out
                last_ip_check = now
            self.stop_evt.wait(0.5)

    def run_bench_bg(self):
        with self.lock:
            if self.bench_running:
                return
            self.bench_running = True
            self.bench_lines = ["Running A vs B vs Bonded speed proof (~25s)…"]
        try:
            proc = subprocess.Popen(
                [BIN, "bench", "-control", CTRL, "-seconds", "6", "-streams", "4"],
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=ENV
            )
            lines = []
            for line in proc.stdout:
                line = line.rstrip()
                if line:
                    lines.append(line)
                    with self.lock:
                        self.bench_lines = lines[-6:]
            proc.wait()
            if proc.returncode != 0 and not lines:
                with self.lock:
                    self.bench_lines = [f"Benchmark exited {proc.returncode}"]
        except Exception as e:
            with self.lock:
                self.bench_lines = [f"Bench error: {e}"]
        finally:
            with self.lock:
                self.bench_running = False

    def prompt_add_link(self, stdscr):
        with self.lock:
            existing = {l["name"] for l in (self.status or {}).get("links", [])}
        cands = detect_candidates(existing)
        h, w = stdscr.getmaxyx()
        curses.curs_set(1)
        stdscr.nodelay(False)
        try:
            stdscr.erase()
            safe_add(stdscr, 1, 2, "ADD LINK TO LIVE BONDED SESSION", curses.A_BOLD)
            row = 3
            for i, (spec, desc) in enumerate(cands, 1):
                safe_add(stdscr, row, 4, f"[{i}] {spec:<24} {desc}")
                row += 1
            safe_add(stdscr, row + 1, 2, "Enter number or custom spec (e.g. feth50, en5, alfa=127.0.0.1:4546), empty=cancel:")
            safe_add(stdscr, row + 2, 2, "> ")
            stdscr.refresh()
            curses.echo()
            raw = stdscr.getstr(row + 2, 4, 60).decode(errors="ignore").strip()
            curses.noecho()
            if not raw:
                return
            spec = raw
            if raw.isdigit():
                idx = int(raw) - 1
                if 0 <= idx < len(cands):
                    spec = cands[idx][0]
            try:
                api_post("/links/add?spec=" + urllib.parse.quote(spec))
                self.flash(f"Added link: {spec}")
            except urllib.error.HTTPError as e:
                self.flash(f"Add failed: {e.read().decode().strip()}")
            except Exception as e:
                self.flash(f"Add failed: {e}")
        finally:
            curses.noecho()
            curses.curs_set(0)
            stdscr.nodelay(True)

    def draw(self, stdscr):
        stdscr.erase()
        h, w = stdscr.getmaxyx()
        with self.lock:
            st = self.status
            pub_ip = self.public_ip
            socks = self.socks_on
            peak_rx = self.peak_rx
            peak_tx = self.peak_tx
            msg = self.msg if time.time() < self.msg_until else ""
            bench_running = self.bench_running
            bench_lines = list(self.bench_lines)

        c_ok = curses.color_pair(1) | curses.A_BOLD
        c_warn = curses.color_pair(2) | curses.A_BOLD
        c_err = curses.color_pair(3) | curses.A_BOLD
        c_hdr = curses.color_pair(4) | curses.A_BOLD
        c_dim = curses.A_DIM

        safe_add(stdscr, 0, 2, "SPEEDBOND · MULTI-LINK QUIC BONDING MONITOR", c_hdr)

        if not st:
            safe_add(stdscr, 2, 2, "● STATUS: OFF (speedbond client not running)", c_err)
            safe_add(stdscr, 4, 2, f"Control API: http://{CTRL} (unreachable)", c_dim)
            safe_add(stdscr, 6, 2, "Start with:  ./start-bond.sh", curses.A_BOLD)
            safe_add(stdscr, 7, 2, f"Current direct IPv4: {pub_ip}   macOS SOCKS5: {'ON' if socks else 'OFF'}")
            safe_add(stdscr, h - 2, 2, "[p] Toggle macOS SOCKS5   [q] Quit", c_dim)
            if msg:
                safe_add(stdscr, h - 3, 2, msg, c_warn)
            stdscr.refresh()
            return

        active = st.get("active_links", 0)
        total = st.get("total_links", 0)
        if active >= 2:
            badge, bcol = f"● ON · BONDED ({active}/{total} links active)", c_ok
        elif active == 1:
            badge, bcol = f"● ON · SINGLE LINK ({active}/{total} active)", c_warn
        else:
            badge, bcol = f"● RECONNECTING (0/{total} active)", c_err

        safe_add(stdscr, 2, 2, badge, bcol)
        safe_add(stdscr, 3, 2,
                 f"Tunnel: {st.get('tunnel','')} ({st.get('tunnel_ip','')})  "
                 f"Relay: {st.get('server','')}  Exit IP: {pub_ip}  MTU: {st.get('mtu','')}")

        rx = st.get("total_rx_mbps", 0.0)
        tx = st.get("total_tx_mbps", 0.0)
        scale = max(20.0, peak_rx, rx, tx)
        safe_add(stdscr, 5, 2, f"DOWN  {rx:6.2f} Mbps  [{bar(rx, scale, 22)}]  peak {peak_rx:5.1f} Mbps", c_ok if rx > 0.1 else 0)
        safe_add(stdscr, 6, 2, f"UP    {tx:6.2f} Mbps  [{bar(tx, scale, 22)}]  peak {peak_tx:5.1f} Mbps")

        reasm = st.get("reassembler", {})
        safe_add(stdscr, 7, 2,
                 f"Packets: delivered={reasm.get('delivered',0)}  reordered={reasm.get('reordered',0)}  "
                 f"gaps_skipped={reasm.get('gaps_skipped',0)}  uptime={st.get('uptime_sec',0)}s", c_dim)

        row = 9
        if socks:
            safe_add(stdscr, row, 2, "⚠ macOS System SOCKS5 Proxy is ON (browsers bypass bonded tunnel! Press [p] to disable)", c_warn)
            row += 2

        safe_add(stdscr, row, 2,
                 f"{'LINK':<10} {'STATE':<10} {'SHARE':>6} {'RTT':>8} {'LOSS':>6} {'RX Mbps':>8} {'TX Mbps':>8}  {'ACTIVITY':<14} TARGET",
                 curses.A_BOLD)
        row += 1

        links = st.get("links", [])
        if self.sel >= len(links):
            self.sel = max(0, len(links) - 1)

        for idx, l in enumerate(links):
            is_sel = idx == self.sel
            state = l.get("state", "?")
            attr = curses.A_REVERSE if is_sel else 0
            if not is_sel:
                if state == "UP":
                    attr = c_ok
                elif state == "PAUSED":
                    attr = c_warn
                else:
                    attr = c_err
            target = l.get("endpoint") or l.get("interface") or ""
            l_rx = l.get("rx_mbps", 0.0)
            l_tx = l.get("tx_mbps", 0.0)
            line = (f"{l.get('name',''):<10} {state:<10} {l.get('weight_pct',0):5.0f}% "
                    f"{l.get('rtt_ms',0):6.1f}ms {l.get('loss_pct',0):5.1f}% "
                    f"{l_rx:8.2f} {l_tx:8.2f}  {bar(l_rx+l_tx, max(10.0, scale), 12):<14} {target}")
            safe_add(stdscr, row, 2, line, attr)
            if l.get("last_error") and state != "UP":
                row += 1
                safe_add(stdscr, row, 6, f"└─ {l['last_error']}", c_dim)
            row += 1

        if bench_lines:
            row += 1
            title = "SPEED PROOF (running…)" if bench_running else "SPEED PROOF RESULTS"
            safe_add(stdscr, row, 2, title, c_hdr)
            row += 1
            for bl in bench_lines:
                battr = c_ok if bl.startswith(("BONDED", "PROOF:")) else 0
                safe_add(stdscr, row, 2, bl, battr)
                row += 1

        if msg:
            safe_add(stdscr, h - 3, 2, msg, c_warn)
        safe_add(stdscr, h - 2, 2,
                 "[↑/↓] Select  [a] Add link  [space] Pause/Resume  [x] Remove  [b] Speed Proof  [p] SOCKS5  [S] Stop VPN  [q] Quit",
                 c_dim)
        stdscr.refresh()

    def main(self, stdscr):
        curses.curs_set(0)
        curses.start_color()
        curses.use_default_colors()
        curses.init_pair(1, curses.COLOR_GREEN, -1)
        curses.init_pair(2, curses.COLOR_YELLOW, -1)
        curses.init_pair(3, curses.COLOR_RED, -1)
        curses.init_pair(4, curses.COLOR_CYAN, -1)
        stdscr.nodelay(True)

        t = threading.Thread(target=self.poll_loop, daemon=True)
        t.start()
        try:
            while True:
                self.draw(stdscr)
                ch = stdscr.getch()
                if ch == -1:
                    time.sleep(0.1)
                    continue
                if ch in (ord("q"), ord("Q")):
                    break
                elif ch == curses.KEY_UP:
                    self.sel = max(0, self.sel - 1)
                elif ch == curses.KEY_DOWN:
                    with self.lock:
                        n = len((self.status or {}).get("links", []))
                    if n > 0:
                        self.sel = min(n - 1, self.sel + 1)
                elif ch == ord("a"):
                    if self.status:
                        self.prompt_add_link(stdscr)
                elif ch == ord(" "):
                    with self.lock:
                        links = (self.status or {}).get("links", [])
                        target = links[self.sel] if 0 <= self.sel < len(links) else None
                    if target:
                        action = "resume" if target.get("paused") else "pause"
                        try:
                            api_post(f"/links/{action}?name=" + urllib.parse.quote(target["name"]))
                            self.flash(f"{action.title()}d {target['name']}")
                        except Exception as e:
                            self.flash(f"Error: {e}")
                elif ch == ord("x"):
                    with self.lock:
                        links = (self.status or {}).get("links", [])
                        target = links[self.sel] if 0 <= self.sel < len(links) else None
                    if target:
                        try:
                            api_post("/links/remove?name=" + urllib.parse.quote(target["name"]))
                            self.flash(f"Removed {target['name']}")
                        except Exception as e:
                            self.flash(f"Error: {e}")
                elif ch == ord("b"):
                    if self.status:
                        threading.Thread(target=self.run_bench_bg, daemon=True).start()
                elif ch == ord("p"):
                    with self.lock:
                        cur = self.socks_on
                    ok, out = toggle_socks_proxy(cur)
                    with self.lock:
                        self.socks_on = not cur if ok else cur
                    self.flash(f"macOS SOCKS5 {'OFF' if cur else 'ON'}" if ok else f"SOCKS5 toggle failed: {out}")
                elif ch == ord("S"):
                    if self.status:
                        try:
                            api_post("/stop")
                            self.flash("Stopping speedbond client…")
                        except Exception as e:
                            self.flash(f"Stop error: {e}")
        finally:
            self.stop_evt.set()


if __name__ == "__main__":
    curses.wrapper(App().main)
