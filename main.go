package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "cert":
		flags := flag.NewFlagSet("cert", flag.ExitOnError)
		cert := flags.String("cert", "relay.crt", "certificate output")
		key := flags.String("key", "relay.key", "private key output")
		flags.Parse(os.Args[2:])
		err = generateCert(*cert, *key)
	case "interfaces":
		err = listInterfaces()
	case "client":
		err = runClient(ctx, os.Args[2:])
	case "relay":
		err = runRelay(ctx, os.Args[2:])
	case "bridge":
		err = runBridge(ctx, os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "ctl":
		err = runControl(os.Args[2:])
	case "bench":
		err = runBench(ctx, os.Args[2:])
	case "probe":
		err = runProbe(ctx, os.Args[2:])
	case "pick":
		err = runPick(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `open-speedify: multi-link QUIC bonding VPN

  speedbond interfaces
  speedbond cert [-cert relay.crt] [-key relay.key]
  SPEEDBOND_TOKEN=... speedbond relay -listen :4545 -cert relay.crt -key relay.key
  speedbond bridge -listen :4546 -target VPS:443 -iface wlx...
  sudo -E SPEEDBOND_TOKEN=... speedbond client -server VPS:443 -links en0,feth50,alfa=127.0.0.1:4546 -pin FINGERPRINT
  speedbond status [-json]
  speedbond ctl <add|remove|pause|resume|stop> [arg]
  speedbond pick [-config relays.json] [-prefer NAME] [-json]
  speedbond bench [-url URL] [-streams 4] [-seconds 8]
`)
}

func listInterfaces() error {
	interfaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		fmt.Printf("%d %-12s mtu=%d %s\n", iface.Index, iface.Name, iface.MTU, addresses)
	}
	return nil
}

func quicConfig() *quic.Config {
	return &quic.Config{
		EnableDatagrams:      true,
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       10 * time.Second,
		KeepAlivePeriod:      time.Second,
	}
}

func requiredToken() (string, error) {
	token := os.Getenv("SPEEDBOND_TOKEN")
	if len(token) < 16 {
		return "", fmt.Errorf("set SPEEDBOND_TOKEN to a shared secret of at least 16 characters")
	}
	return token, nil
}

func splitLinks(value string) ([]string, error) {
	specs, err := parseLinkSpecs(value)
	if err != nil {
		return nil, err
	}
	links := make([]string, len(specs))
	for i, s := range specs {
		links[i] = s.Raw
	}
	return links, nil
}

func fetchStatus(controlAddr string) (clientStatusJSON, error) {
	var st clientStatusJSON
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + controlAddr + "/status")
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return st, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

func postControl(controlAddr, path string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post("http://"+controlAddr+path, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}
	return nil
}

func runProbe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("probe", flag.ExitOnError)
	server := flags.String("server", "", "relay address host:port")
	linkNames := flags.String("links", "", "comma-separated links to probe")
	pin := flags.String("pin", "", "relay certificate SHA-256 fingerprint")
	flags.Parse(args)
	token, err := requiredToken()
	if err != nil {
		return err
	}
	specs, err := parseLinkSpecs(*linkNames)
	if err != nil {
		return err
	}
	remote, err := net.ResolveUDPAddr("udp4", *server)
	if err != nil {
		return err
	}
	tlsConfig, err := clientTLS(*pin)
	if err != nil {
		return err
	}
	session, err := newSession()
	if err != nil {
		return err
	}
	for i, spec := range specs {
		dctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		link, ip, err := dialClientLinkSpec(dctx, remote, tlsConfig, token, session, byte(i), spec)
		cancel()
		if err != nil {
			fmt.Printf("FAIL  %-12s  %v\n", spec.Name, err)
			continue
		}
		fmt.Printf("OK    %-12s  tunnel_ip=%s  rtt=%s  target=%s\n",
			spec.Name, ip, link.conn.ConnectionStats().SmoothedRTT.Round(time.Millisecond), link.target)
		link.close(0, "probe done")
	}
	return nil
}

func runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	controlAddr := flags.String("control", "127.0.0.1:19454", "control API address")
	asJSON := flags.Bool("json", false, "output raw JSON")
	flags.Parse(args)

	st, err := fetchStatus(*controlAddr)
	if err != nil {
		return fmt.Errorf("speedbond client not reachable on %s: %w", *controlAddr, err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	fmt.Printf("Status: ON  Tunnel: %s (%s)  Server: %s  Links: %d/%d up\n",
		st.Tunnel, st.TunnelIP, st.Server, st.ActiveLinks, st.TotalLinks)
	fmt.Printf("Total:  RX %.2f Mbps  TX %.2f Mbps  Delivered: %d  Reordered: %d  Gaps: %d\n\n",
		st.TotalRxMbps, st.TotalTxMbps, st.Reassembler.Delivered, st.Reassembler.Reordered, st.Reassembler.GapsSkipped)
	fmt.Printf("%-12s %-10s %6s %8s %6s %9s %9s  %s\n",
		"LINK", "STATE", "WEIGHT", "RTT", "LOSS", "RX Mbps", "TX Mbps", "TARGET/IFACE")
	for _, l := range st.Links {
		target := l.Interface
		if l.Endpoint != "" {
			target = l.Endpoint
		}
		fmt.Printf("%-12s %-10s %5.0f%% %6.1fms %5.1f%% %9.2f %9.2f  %s\n",
			l.Name, l.State, l.WeightPct, l.RTTMs, l.LossPct, l.RxMbps, l.TxMbps, target)
	}
	return nil
}

func runControl(args []string) error {
	flags := flag.NewFlagSet("ctl", flag.ExitOnError)
	controlAddr := flags.String("control", "127.0.0.1:19454", "control API address")
	flags.Parse(args)
	rest := flags.Args()
	if len(rest) == 0 {
		return fmt.Errorf("usage: speedbond ctl <add|remove|pause|resume|stop> [arg]")
	}
	action := rest[0]
	switch action {
	case "add":
		if len(rest) < 2 {
			return fmt.Errorf("usage: speedbond ctl add <iface|name=host:port>")
		}
		if err := postControl(*controlAddr, "/links/add?spec="+url.QueryEscape(rest[1])); err != nil {
			return err
		}
		fmt.Printf("Added link %s\n", rest[1])
	case "remove":
		if len(rest) < 2 {
			return fmt.Errorf("usage: speedbond ctl remove <name>")
		}
		if err := postControl(*controlAddr, "/links/remove?name="+url.QueryEscape(rest[1])); err != nil {
			return err
		}
		fmt.Printf("Removed link %s\n", rest[1])
	case "pause":
		if len(rest) < 2 {
			return fmt.Errorf("usage: speedbond ctl pause <name>")
		}
		if err := postControl(*controlAddr, "/links/pause?name="+url.QueryEscape(rest[1])); err != nil {
			return err
		}
		fmt.Printf("Paused link %s\n", rest[1])
	case "resume":
		if len(rest) < 2 {
			return fmt.Errorf("usage: speedbond ctl resume <name>")
		}
		if err := postControl(*controlAddr, "/links/resume?name="+url.QueryEscape(rest[1])); err != nil {
			return err
		}
		fmt.Printf("Resumed link %s\n", rest[1])
	case "stop":
		if err := postControl(*controlAddr, "/stop"); err != nil {
			return err
		}
		fmt.Println("Stopping speedbond client")
	default:
		return fmt.Errorf("unknown ctl action %q", action)
	}
	return nil
}

func runBench(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("bench", flag.ExitOnError)
	controlAddr := flags.String("control", "127.0.0.1:19454", "control API address")
	testURL := flags.String("url", "https://fsn1-speed.hetzner.com/100MB.bin", "download URL for throughput test")
	streams := flags.Int("streams", 3, "parallel HTTP streams")
	seconds := flags.Int("seconds", 8, "duration per phase in seconds")
	flags.Parse(args)

	st, err := fetchStatus(*controlAddr)
	if err != nil {
		return fmt.Errorf("speedbond client not reachable: %w", err)
	}
	var upLinks []string
	for _, l := range st.Links {
		if l.State == "UP" {
			upLinks = append(upLinks, l.Name)
		}
	}
	if len(upLinks) == 0 {
		return fmt.Errorf("no UP links to benchmark")
	}

	// Always restore all originally UP links on exit.
	defer func() {
		for _, name := range upLinks {
			_ = postControl(*controlAddr, "/links/resume?name="+url.QueryEscape(name))
		}
	}()

	waitForLinks := func(want []string) error {
		wantSet := make(map[string]bool, len(want))
		for _, w := range want {
			wantSet[w] = true
		}
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			cur, err := fetchStatus(*controlAddr)
			if err == nil {
				ok := true
				for _, l := range cur.Links {
					if wantSet[l.Name] && l.State != "UP" {
						ok = false
						break
					}
					if !wantSet[l.Name] && l.State == "UP" {
						ok = false
						break
					}
				}
				if ok {
					time.Sleep(400 * time.Millisecond)
					return nil
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Errorf("timed out waiting for links %v", want)
	}

	results := make(map[string]float64)
	bestSolo := 0.0
	sumSolo := 0.0

	if len(upLinks) >= 2 {
		for _, solo := range upLinks {
			for _, name := range upLinks {
				if name == solo {
					_ = postControl(*controlAddr, "/links/resume?name="+url.QueryEscape(name))
				} else {
					_ = postControl(*controlAddr, "/links/pause?name="+url.QueryEscape(name))
				}
			}
			if err := waitForLinks([]string{solo}); err != nil {
				return err
			}
			mbps, bytes, err := measureDownload(ctx, *testURL, *streams, time.Duration(*seconds)*time.Second)
			if err != nil {
				fmt.Printf("SOLO   %-12s    0.00 Mbps  (err: %v)\n", solo, err)
				continue
			}
			results[solo] = mbps
			sumSolo += mbps
			if mbps > bestSolo {
				bestSolo = mbps
			}
			fmt.Printf("SOLO   %-12s  %6.2f Mbps  (%.2f MB in %ds)\n", solo, mbps, float64(bytes)/1e6, *seconds)
		}
	}

	// Resume all links for bonded measurement.
	for _, name := range upLinks {
		_ = postControl(*controlAddr, "/links/resume?name="+url.QueryEscape(name))
	}
	if err := waitForLinks(upLinks); err != nil {
		return err
	}
	// Warmup briefly so adaptive weights start converging.
	bondedMbps, bondedBytes, err := measureDownload(ctx, *testURL, *streams, time.Duration(*seconds)*time.Second)
	if err != nil {
		return fmt.Errorf("bonded test failed: %w", err)
	}
	fmt.Printf("BONDED %-12s  %6.2f Mbps  (%.2f MB in %ds)\n", strings.Join(upLinks, "+"), bondedMbps, float64(bondedBytes)/1e6, *seconds)

	if len(upLinks) >= 2 && bestSolo > 0 {
		gain := ((bondedMbps - bestSolo) / bestSolo) * 100.0
		eff := (bondedMbps / sumSolo) * 100.0
		fmt.Printf("\nPROOF: best solo=%.2f Mbps | sum=%.2f Mbps | bonded=%.2f Mbps (%+.1f%% vs best solo, %.0f%% of combined sum)\n",
			bestSolo, sumSolo, bondedMbps, gain, eff)
	}
	return nil
}

func measureDownload(parent context.Context, targetURL string, streams int, dur time.Duration) (float64, int64, error) {
	ctx, cancel := context.WithTimeout(parent, dur)
	defer cancel()
	// Force IPv4 so macOS doesn't bypass the IPv4 tunnel over IPv6, ignore system SOCKS proxy,
	// and use 1.1.1.1 over the tunnel so DNS stays reliable even after macOS sleep/wake.
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "udp4", "1.1.1.1:53")
		},
	}
	dialer := &net.Dialer{Timeout: 6 * time.Second, Resolver: resolver}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
	}
	client := &http.Client{Transport: transport}
	var total atomic.Int64
	var firstErr error
	var errMu sync.Mutex
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 32*1024)
			for ctx.Err() == nil {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					return
				}
				req.Header.Set("User-Agent", "curl/8.7.1")
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						errMu.Unlock()
					}
					return
				}
				if resp.StatusCode != http.StatusOK {
					resp.Body.Close()
					errMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, targetURL)
					}
					errMu.Unlock()
					return
				}
				for {
					n, rerr := resp.Body.Read(buf)
					if n > 0 {
						total.Add(int64(n))
					}
					if rerr != nil {
						break
					}
				}
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	bytes := total.Load()
	if bytes == 0 && firstErr != nil {
		return 0, 0, firstErr
	}
	if elapsed <= 0 {
		elapsed = 1
	}
	return (float64(bytes) * 8.0) / (elapsed * 1_000_000.0), bytes, nil
}
