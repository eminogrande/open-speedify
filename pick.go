package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type relayEntry struct {
	Name          string `json:"name"`
	Server        string `json:"server"`
	Pin           string `json:"pin"`
	Token         string `json:"token,omitempty"`
	TokenKeychain string `json:"token_keychain,omitempty"`
}

type relayConfig struct {
	Selected string       `json:"selected"`
	Relays   []relayEntry `json:"relays"`
}

type relayProbeResult struct {
	Name   string  `json:"name"`
	Server string  `json:"server"`
	Pin    string  `json:"pin"`
	Token  string  `json:"token,omitempty"`
	OK     bool    `json:"ok"`
	RTTMs  float64 `json:"rtt_ms"`
	Error  string  `json:"error,omitempty"`
}

type pickOutput struct {
	Selected string             `json:"selected"`
	Best     *relayProbeResult  `json:"best,omitempty"`
	Results  []relayProbeResult `json:"results"`
}

func defaultRelayConfigPath() string {
	if env := strings.TrimSpace(os.Getenv("SPEEDBOND_CONFIG")); env != "" {
		return env
	}
	if _, err := os.Stat("relays.json"); err == nil {
		return "relays.json"
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "relays.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "open-speedify", "relays.json")
	}
	return "relays.json"
}

func loadRelayConfig(path string) (relayConfig, error) {
	var cfg relayConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s (copy relays.example.json or run ./deploy-relay.sh): %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(cfg.Relays) == 0 {
		return cfg, fmt.Errorf("no relays configured in %s", path)
	}
	if strings.TrimSpace(cfg.Selected) == "" {
		cfg.Selected = "auto"
	}
	return cfg, nil
}

func resolveRelayToken(r relayEntry) (string, error) {
	if t := strings.TrimSpace(r.Token); t != "" {
		return t, nil
	}
	if r.TokenKeychain != "" && runtime.GOOS == "darwin" {
		user := os.Getenv("USER")
		args := []string{"find-generic-password", "-s", r.TokenKeychain, "-w"}
		if user != "" {
			args = append([]string{"find-generic-password", "-a", user, "-s", r.TokenKeychain, "-w"})
		}
		out, err := exec.Command("security", args...).Output()
		if err == nil {
			if t := strings.TrimSpace(string(out)); len(t) >= 16 {
				return t, nil
			}
		}
	}
	if t := strings.TrimSpace(os.Getenv("SPEEDBOND_TOKEN")); len(t) >= 16 {
		return t, nil
	}
	return "", fmt.Errorf("no token configured for relay %q (set token, token_keychain, or SPEEDBOND_TOKEN)", r.Name)
}

func firstPhysicalInterface() (string, net.IP) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", nil
	}
	// Prefer en0 (Mac Wi-Fi) if it has a routable IPv4, else first active physical interface.
	var fallbackName string
	var fallbackIP net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := iface.Name
		if strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "awdl") ||
			strings.HasPrefix(name, "llw") || strings.HasPrefix(name, "bridge") ||
			strings.HasPrefix(name, "ap") || strings.HasPrefix(name, "lo") ||
			strings.HasPrefix(name, "anpi") || strings.HasPrefix(name, "anri") ||
			strings.HasPrefix(name, "speedbond") {
			continue
		}
		ip, err := interfaceIPv4(name)
		if err != nil || ip.IsLinkLocalUnicast() {
			continue
		}
		if name == "en0" {
			return name, ip
		}
		if fallbackName == "" {
			fallbackName, fallbackIP = name, ip
		}
	}
	return fallbackName, fallbackIP
}

func probeRelay(ctx context.Context, r relayEntry, timeout time.Duration) relayProbeResult {
	res := relayProbeResult{
		Name:   r.Name,
		Server: r.Server,
		Pin:    r.Pin,
	}
	if res.Name == "" {
		res.Name = r.Server
	}
	token, err := resolveRelayToken(r)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Token = token
	tlsCfg, err := clientTLS(r.Pin)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	remote, err := net.ResolveUDPAddr("udp4", r.Server)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	var udp *net.UDPConn
	if !remote.IP.IsLoopback() {
		if ifName, localIP := firstPhysicalInterface(); ifName != "" {
			if c, lerr := net.ListenUDP("udp4", &net.UDPAddr{IP: localIP, Port: 0}); lerr == nil {
				if raw, serr := c.SyscallConn(); serr == nil {
					var berr error
					if cerr := raw.Control(func(fd uintptr) { berr = bindToInterface(fd, ifName) }); cerr == nil && berr == nil {
						udp = c
					}
				}
				if udp == nil {
					c.Close()
				}
			}
		}
	}
	if udp == nil {
		udp, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			res.Error = err.Error()
			return res
		}
	}
	defer udp.Close()

	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	conn, err := quic.Dial(pctx, udp, remote, tlsCfg, quicConfig())
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer conn.CloseWithError(0, "pick probe done")

	stream, err := conn.OpenStreamSync(pctx)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	stream.SetDeadline(time.Now().Add(timeout))
	session, err := newSession()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	h := hello{Session: session, Link: 0, Token: tokenHash(token)}
	if err := h.writeTo(stream); err != nil {
		res.Error = err.Error()
		return res
	}
	if _, err := readHelloReply(stream); err != nil {
		res.Error = err.Error()
		return res
	}
	stream.Close()

	rtt := conn.ConnectionStats().SmoothedRTT
	if rtt <= 0 {
		rtt = time.Since(start)
	}
	res.OK = true
	res.RTTMs = float64(rtt) / float64(time.Millisecond)
	return res
}

func pickBestRelay(ctx context.Context, relays []relayEntry, preferred string, timeout time.Duration) pickOutput {
	results := make([]relayProbeResult, len(relays))
	var wg sync.WaitGroup
	for i, r := range relays {
		wg.Add(1)
		go func(idx int, entry relayEntry) {
			defer wg.Done()
			results[idx] = probeRelay(ctx, entry, timeout)
		}(i, r)
	}
	wg.Wait()

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].OK != results[j].OK {
			return results[i].OK
		}
		if results[i].OK && results[j].OK {
			return results[i].RTTMs < results[j].RTTMs
		}
		return i < j
	})

	out := pickOutput{
		Selected: preferred,
		Results:  results,
	}
	if preferred != "" && preferred != "auto" {
		for i := range results {
			if results[i].OK && (results[i].Name == preferred || results[i].Server == preferred) {
				best := results[i]
				out.Best = &best
				return out
			}
		}
	}
	for i := range results {
		if results[i].OK {
			best := results[i]
			out.Best = &best
			return out
		}
	}
	return out
}

func runPick(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("pick", flag.ExitOnError)
	cfgPath := flags.String("config", defaultRelayConfigPath(), "path to relays.json")
	prefer := flags.String("prefer", "", "override preferred relay name or server (empty = use config)")
	timeout := flags.Duration("timeout", 4*time.Second, "per-relay probe timeout")
	asJSON := flags.Bool("json", false, "output JSON (includes token for launcher)")
	flags.Parse(args)

	cfg, err := loadRelayConfig(*cfgPath)
	if err != nil {
		return err
	}
	preferred := cfg.Selected
	if strings.TrimSpace(*prefer) != "" {
		preferred = strings.TrimSpace(*prefer)
	}
	res := pickBestRelay(ctx, cfg.Relays, preferred, *timeout)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	for _, r := range res.Results {
		mark := "  "
		if res.Best != nil && r.Server == res.Best.Server && r.Name == res.Best.Name {
			mark = "★ "
		}
		if r.OK {
			fmt.Printf("%sOK    %-20s  %6.1fms  %s\n", mark, r.Name, r.RTTMs, r.Server)
		} else {
			fmt.Printf("  FAIL  %-20s  %-8s  %s (%s)\n", r.Name, "-", r.Server, r.Error)
		}
	}
	if res.Best == nil {
		return fmt.Errorf("no reachable relays in %s", *cfgPath)
	}
	return nil
}
