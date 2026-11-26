package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

type linkSpec struct {
	Raw       string `json:"raw"`
	Name      string `json:"name"`
	Interface string `json:"interface,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
}

func parseLinkSpec(raw string) (linkSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return linkSpec{}, fmt.Errorf("empty link spec")
	}
	if idx := strings.IndexByte(raw, '='); idx >= 0 {
		name := strings.TrimSpace(raw[:idx])
		endpoint := strings.TrimSpace(raw[idx+1:])
		if name == "" || endpoint == "" {
			return linkSpec{}, fmt.Errorf("invalid bridge link %q (expected name=host:port)", raw)
		}
		if _, _, err := net.SplitHostPort(endpoint); err != nil {
			return linkSpec{}, fmt.Errorf("invalid bridge endpoint %q: %w", endpoint, err)
		}
		return linkSpec{Raw: raw, Name: name, Endpoint: endpoint}, nil
	}
	return linkSpec{Raw: raw, Name: raw, Interface: raw}, nil
}

func parseLinkSpecs(value string) ([]linkSpec, error) {
	var specs []linkSpec
	seen := make(map[string]bool)
	for _, part := range strings.Split(value, ",") {
		spec, err := parseLinkSpec(part)
		if err != nil {
			return nil, err
		}
		if seen[spec.Name] {
			return nil, fmt.Errorf("duplicate link name %q", spec.Name)
		}
		seen[spec.Name] = true
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("at least one link is required")
	}
	return specs, nil
}

type managedLink struct {
	id     byte
	spec   linkSpec
	cancel context.CancelFunc
	paused atomic.Bool
	wake   chan struct{}

	mu           sync.Mutex
	active       *bondLink
	state        string
	lastErr      string
	routeCleanup func()
}

type linkStatusJSON struct {
	ID          byte    `json:"id"`
	Name        string  `json:"name"`
	Spec        string  `json:"spec"`
	Interface   string  `json:"interface,omitempty"`
	Endpoint    string  `json:"endpoint,omitempty"`
	State       string  `json:"state"`
	Paused      bool    `json:"paused"`
	LastError   string  `json:"last_error,omitempty"`
	Weight      int     `json:"weight"`
	WeightPct   float64 `json:"weight_pct"`
	RTTMs       float64 `json:"rtt_ms"`
	MinRTTMs    float64 `json:"min_rtt_ms"`
	LossPct     float64 `json:"loss_pct"`
	TxMbps      float64 `json:"tx_mbps"`
	RxMbps      float64 `json:"rx_mbps"`
	SentPackets uint64  `json:"sent_packets"`
	RecvPackets uint64  `json:"recv_packets"`
	SentBytes   uint64  `json:"sent_bytes"`
	RecvBytes   uint64  `json:"recv_bytes"`
	SendErrors  uint64  `json:"send_errors"`
}

type clientStatusJSON struct {
	Running       bool             `json:"running"`
	Server        string           `json:"server"`
	RelayName     string           `json:"relay_name,omitempty"`
	Tunnel        string           `json:"tunnel"`
	TunnelIP      string           `json:"tunnel_ip"`
	MTU           int              `json:"mtu"`
	Routing       bool             `json:"routing"`
	UptimeSec     int64            `json:"uptime_sec"`
	ActiveLinks   int              `json:"active_links"`
	TotalLinks    int              `json:"total_links"`
	TotalTxMbps   float64          `json:"total_tx_mbps"`
	TotalRxMbps   float64          `json:"total_rx_mbps"`
	Reassembler   reassemblerStats `json:"reassembler"`
	Links         []linkStatusJSON `json:"links"`
}

type clientManager struct {
	ctx         context.Context
	cancelAll   context.CancelFunc
	serverStr   string
	relayName   string
	remote      *net.UDPAddr
	tlsConfig   *tls.Config
	token       string
	session     [16]byte
	route       bool
	tunName     string
	tunnelIP    net.IP
	startedAt   time.Time
	pool        *linkPool
	reassembler *reassembler
	ready       chan net.IP

	mu     sync.Mutex
	links  map[string]*managedLink
	usedID map[byte]bool
}

func runClient(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("client", flag.ExitOnError)
	server := flags.String("server", "", "relay address host:port")
	relayName := flags.String("relay-name", "", "friendly relay name for UI (optional)")
	linkNames := flags.String("links", "", "comma-separated links, e.g. en0,feth50,alfa=127.0.0.1:4546")
	pin := flags.String("pin", "", "relay certificate SHA-256 fingerprint")
	tunName := flags.String("tun", "auto", "tunnel interface")
	route := flags.Bool("route", true, "route IPv4 internet traffic through tunnel")
	reorderWait := flags.Duration("reorder-wait", 0, "optional cross-link reorder wait (0 = direct delivery, recommended)")
	controlAddr := flags.String("control", "127.0.0.1:19454", "local control HTTP address (empty to disable)")
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
	tun, err := openTUN(*tunName)
	if err != nil {
		return fmt.Errorf("open tunnel (run with sudo): %w", err)
	}
	defer tun.Close()

	runCtx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	pool := newLinkPool()
	reasm := newReassembler(*reorderWait, func(packet []byte) error {
		_, err := tun.Write(packet)
		return err
	})
	defer reasm.close()

	mgr := &clientManager{
		ctx:         runCtx,
		cancelAll:   cancelAll,
		serverStr:   *server,
		relayName:   strings.TrimSpace(*relayName),
		remote:      remote,
		tlsConfig:   tlsConfig,
		token:       token,
		session:     session,
		route:       *route,
		tunName:     tun.Name(),
		startedAt:   time.Now(),
		pool:        pool,
		reassembler: reasm,
		ready:       make(chan net.IP, 1),
		links:       make(map[string]*managedLink),
		usedID:      make(map[byte]bool),
	}
	defer mgr.cleanupAllLinks()

	for _, spec := range specs {
		if err := mgr.addLink(spec); err != nil {
			return err
		}
	}

	var ip net.IP
	select {
	case ip = <-mgr.ready:
		mgr.mu.Lock()
		mgr.tunnelIP = ip
		mgr.mu.Unlock()
	case <-runCtx.Done():
		return runCtx.Err()
	case <-time.After(20 * time.Second):
		return fmt.Errorf("no link connected within 20 seconds")
	}

	cleanup := func() {}
	if *route {
		cleanup, err = configureClient(tun.Name(), ip)
		if err != nil {
			return fmt.Errorf("configure tunnel routes: %w", err)
		}
	}
	defer cleanup()

	if *controlAddr != "" {
		srv, err := mgr.startControlServer(*controlAddr)
		if err != nil {
			log.Printf("warning: control API on %s failed: %v", *controlAddr, err)
		} else {
			defer srv.Close()
			log.Printf("control API listening on http://%s", *controlAddr)
		}
	}

	log.Printf("connected: tunnel=%s address=%s mtu=%d links=%d", tun.Name(), ip, tunnelMTU, pool.count())
	logLinkStats(runCtx, pool)
	return readClientTUN(runCtx, tun, pool)
}

func (m *clientManager) addLink(spec linkSpec) error {
	m.mu.Lock()
	if _, exists := m.links[spec.Name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("link %q already exists", spec.Name)
	}
	id := byte(0)
	found := false
	for cand := 0; cand < 255; cand++ {
		if !m.usedID[byte(cand)] {
			id = byte(cand)
			m.usedID[id] = true
			found = true
			break
		}
	}
	if !found {
		m.mu.Unlock()
		return fmt.Errorf("maximum number of links reached")
	}
	linkCtx, cancel := context.WithCancel(m.ctx)
	ml := &managedLink{
		id:     id,
		spec:   spec,
		cancel: cancel,
		wake:   make(chan struct{}, 1),
		state:  "CONNECTING",
	}
	m.links[spec.Name] = ml
	m.mu.Unlock()

	go m.manageLink(linkCtx, ml)
	return nil
}

func (m *clientManager) removeLink(name string) error {
	m.mu.Lock()
	ml, ok := m.links[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("link %q not found", name)
	}
	delete(m.links, name)
	delete(m.usedID, ml.id)
	m.mu.Unlock()

	ml.cancel()
	ml.mu.Lock()
	active := ml.active
	ml.active = nil
	cleanup := ml.routeCleanup
	ml.routeCleanup = nil
	ml.mu.Unlock()

	if active != nil {
		m.pool.remove(ml.id, active.conn)
		active.close(0, "link removed")
	}
	if cleanup != nil {
		cleanup()
	}
	log.Printf("link=%s removed", name)
	return nil
}

func (m *clientManager) setPaused(name string, paused bool) error {
	m.mu.Lock()
	ml, ok := m.links[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("link %q not found", name)
	}
	ml.paused.Store(paused)
	if paused {
		ml.mu.Lock()
		ml.state = "PAUSED"
		active := ml.active
		ml.active = nil
		ml.mu.Unlock()
		if active != nil {
			m.pool.remove(ml.id, active.conn)
			active.close(0, "paused")
		}
		log.Printf("link=%s paused", name)
	} else {
		ml.mu.Lock()
		ml.state = "CONNECTING"
		ml.mu.Unlock()
		select {
		case ml.wake <- struct{}{}:
		default:
		}
		log.Printf("link=%s resumed", name)
	}
	return nil
}

func (m *clientManager) cleanupAllLinks() {
	m.mu.Lock()
	all := make([]*managedLink, 0, len(m.links))
	for _, ml := range m.links {
		all = append(all, ml)
	}
	m.mu.Unlock()

	for _, ml := range all {
		ml.cancel()
		ml.mu.Lock()
		active := ml.active
		ml.active = nil
		cleanup := ml.routeCleanup
		ml.routeCleanup = nil
		ml.mu.Unlock()
		if active != nil {
			m.pool.remove(ml.id, active.conn)
			active.close(0, "shutdown")
		}
		if cleanup != nil {
			cleanup()
		}
	}
}

func (m *clientManager) manageLink(ctx context.Context, ml *managedLink) {
	defer func() {
		ml.mu.Lock()
		cleanup := ml.routeCleanup
		ml.routeCleanup = nil
		ml.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
	}()

	for ctx.Err() == nil {
		if ml.paused.Load() {
			ml.mu.Lock()
			ml.state = "PAUSED"
			ml.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-ml.wake:
				continue
			}
		}

		// Ensure interface-scoped relay route before dialing physical interfaces.
		if m.route && ml.spec.Interface != "" {
			ml.mu.Lock()
			hasRoute := ml.routeCleanup != nil
			ml.mu.Unlock()
			if !hasRoute {
				rc, err := configureRelayHost(m.remote.IP, ml.spec.Interface)
				if err != nil {
					ml.mu.Lock()
					ml.state = "DOWN"
					ml.lastErr = fmt.Sprintf("route: %v", err)
					ml.mu.Unlock()
					log.Printf("link=%s route setup failed: %v", ml.spec.Name, err)
					if !sleepOrWake(ctx, time.Second, ml.wake) {
						return
					}
					continue
				}
				ml.mu.Lock()
				ml.routeCleanup = rc
				ml.mu.Unlock()
			}
		}

		ml.mu.Lock()
		ml.state = "CONNECTING"
		ml.mu.Unlock()

		link, ip, err := dialClientLinkSpec(ctx, m.remote, m.tlsConfig, m.token, m.session, ml.id, ml.spec)
		if err == nil {
			if ml.paused.Load() || ctx.Err() != nil {
				link.close(0, "paused")
				continue
			}
			err = m.pool.add(link)
		}
		if err != nil {
			ml.mu.Lock()
			ml.state = "DOWN"
			ml.lastErr = err.Error()
			// Drop cached host route on failure so Wi-Fi/subnet switches pick up the new gateway on retry.
			rc := ml.routeCleanup
			ml.routeCleanup = nil
			ml.mu.Unlock()
			if rc != nil {
				rc()
			}
			log.Printf("link=%s connect failed: %v", ml.spec.Name, err)
			if link != nil {
				link.close(1, "setup failed")
			}
			if !sleepOrWake(ctx, time.Second, ml.wake) {
				return
			}
			continue
		}

		ml.mu.Lock()
		ml.active = link
		ml.state = "UP"
		ml.lastErr = ""
		ml.mu.Unlock()

		m.syncTunnelIP(ip)

		select {
		case m.ready <- ip:
		default:
		}
		log.Printf("link=%s connected ip=%s rtt=%s", ml.spec.Name, ip, link.conn.ConnectionStats().SmoothedRTT)
		err = receiveDatagrams(link.conn, m.reassembler, &link.received, &link.receivedBytes)

		m.pool.remove(ml.id, link.conn)
		ml.mu.Lock()
		if ml.active == link {
			ml.active = nil
		}
		if ml.paused.Load() {
			ml.state = "PAUSED"
		} else {
			ml.state = "DOWN"
			if err != nil {
				ml.lastErr = err.Error()
			}
		}
		// Refresh host route on reconnect in case this interface switched Wi-Fi networks or subnets.
		rc := ml.routeCleanup
		ml.routeCleanup = nil
		ml.mu.Unlock()
		if rc != nil {
			rc()
		}
		link.close(0, "reconnecting")

		if ml.paused.Load() || ctx.Err() != nil {
			continue
		}
		log.Printf("link=%s disconnected: %v; reconnecting", ml.spec.Name, err)
		if !sleepOrWake(ctx, 500*time.Millisecond, ml.wake) {
			return
		}
	}
}

func (m *clientManager) syncTunnelIP(ip net.IP) {
	if ip == nil {
		return
	}
	m.mu.Lock()
	oldIP := m.tunnelIP
	if oldIP == nil || oldIP.Equal(ip) {
		m.mu.Unlock()
		return
	}
	tunName := m.tunName
	doRoute := m.route
	m.tunnelIP = ip
	m.mu.Unlock()

	log.Printf("tunnel IP changed %s -> %s (session refreshed); updating %s", oldIP, ip, tunName)
	if doRoute {
		if err := updateClientIP(tunName, oldIP, ip); err != nil {
			log.Printf("warning: failed updating tunnel IP on %s: %v", tunName, err)
		}
	}
}

func (m *clientManager) status() clientStatusJSON {
	snaps := m.pool.snapshot()
	byID := make(map[byte]linkSnapshot, len(snaps))
	var totalTx, totalRx float64
	for _, s := range snaps {
		byID[s.ID] = s
		totalTx += s.TxMbps
		totalRx += s.RxMbps
	}

	m.mu.Lock()
	tunIP := ""
	if m.tunnelIP != nil {
		tunIP = m.tunnelIP.String()
	}
	managed := make([]*managedLink, 0, len(m.links))
	for _, ml := range m.links {
		managed = append(managed, ml)
	}
	m.mu.Unlock()

	sort.Slice(managed, func(i, j int) bool { return managed[i].id < managed[j].id })
	linksOut := make([]linkStatusJSON, 0, len(managed))
	activeCount := 0
	for _, ml := range managed {
		ml.mu.Lock()
		st := ml.state
		lastErr := ml.lastErr
		ml.mu.Unlock()

		item := linkStatusJSON{
			ID:        ml.id,
			Name:      ml.spec.Name,
			Spec:      ml.spec.Raw,
			Interface: ml.spec.Interface,
			Endpoint:  ml.spec.Endpoint,
			State:     st,
			Paused:    ml.paused.Load(),
			LastError: lastErr,
		}
		if snap, ok := byID[ml.id]; ok {
			activeCount++
			item.State = "UP"
			item.Weight = snap.Weight
			item.WeightPct = snap.WeightPct
			item.RTTMs = snap.RTTMs
			item.MinRTTMs = snap.MinRTTMs
			item.LossPct = snap.LossPct
			item.TxMbps = snap.TxMbps
			item.RxMbps = snap.RxMbps
			item.SentPackets = snap.SentPackets
			item.RecvPackets = snap.RecvPackets
			item.SentBytes = snap.SentBytes
			item.RecvBytes = snap.RecvBytes
			item.SendErrors = snap.SendErrors
		}
		linksOut = append(linksOut, item)
	}

	return clientStatusJSON{
		Running:     m.ctx.Err() == nil,
		Server:      m.serverStr,
		RelayName:   m.relayName,
		Tunnel:      m.tunName,
		TunnelIP:    tunIP,
		MTU:         tunnelMTU,
		Routing:     m.route,
		UptimeSec:   int64(time.Since(m.startedAt).Seconds()),
		ActiveLinks: activeCount,
		TotalLinks:  len(linksOut),
		TotalTxMbps: totalTx,
		TotalRxMbps: totalRx,
		Reassembler: m.reassembler.stats(),
		Links:       linksOut,
	}
}

func (m *clientManager) startControlServer(addr string) (*http.Server, error) {
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.status())
	})
	mux.HandleFunc("/links/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		spec, err := parseLinkSpec(r.URL.Query().Get("spec"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := m.addLink(spec); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"ok": "added", "name": spec.Name})
	})
	mux.HandleFunc("/links/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if err := m.removeLink(name); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"ok": "removed", "name": name})
	})
	mux.HandleFunc("/links/pause", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if err := m.setPaused(name, true); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"ok": "paused", "name": name})
	})
	mux.HandleFunc("/links/resume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if err := m.setPaused(name, false); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"ok": "resumed", "name": name})
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"ok": "stopping"})
		go m.cancelAll()
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	go srv.Serve(ln)
	return srv, nil
}

func dialClientLink(ctx context.Context, remote *net.UDPAddr, tlsConfig *tls.Config, token string, session [16]byte, id byte, name string) (*bondLink, net.IP, error) {
	spec, err := parseLinkSpec(name)
	if err != nil {
		return nil, nil, err
	}
	return dialClientLinkSpec(ctx, remote, tlsConfig, token, session, id, spec)
}

func dialClientLinkSpec(ctx context.Context, defaultRemote *net.UDPAddr, tlsConfig *tls.Config, token string, session [16]byte, id byte, spec linkSpec) (*bondLink, net.IP, error) {
	target := defaultRemote
	var udp *net.UDPConn
	var err error

	if spec.Endpoint != "" {
		target, err = net.ResolveUDPAddr("udp4", spec.Endpoint)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve %s: %w", spec.Endpoint, err)
		}
		udp, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			return nil, nil, err
		}
	} else {
		localIP, err := interfaceIPv4(spec.Interface)
		if err != nil {
			return nil, nil, err
		}
		udp, err = net.ListenUDP("udp4", &net.UDPAddr{IP: localIP, Port: 0})
		if err != nil {
			return nil, nil, err
		}
		raw, err := udp.SyscallConn()
		if err == nil {
			var bindErr error
			err = raw.Control(func(fd uintptr) { bindErr = bindToInterface(fd, spec.Interface) })
			if err == nil {
				err = bindErr
			}
		}
		if err != nil {
			udp.Close()
			return nil, nil, fmt.Errorf("bind %s: %w", spec.Interface, err)
		}
	}

	conn, err := quic.Dial(ctx, udp, target, tlsConfig, quicConfig())
	if err != nil {
		udp.Close()
		return nil, nil, err
	}
	link := &bondLink{id: id, name: spec.Name, target: target.String(), conn: conn, udp: udp}
	fail := func(err error) (*bondLink, net.IP, error) {
		link.close(1, "handshake failed")
		return nil, nil, err
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fail(err)
	}
	stream.SetDeadline(time.Now().Add(5 * time.Second))
	h := hello{Session: session, Link: id, Token: tokenHash(token)}
	if err = h.writeTo(stream); err == nil {
		var ip net.IP
		ip, err = readHelloReply(stream)
		if err == nil {
			stream.Close()
			return link, ip, nil
		}
	}
	return fail(err)
}

func interfaceIPv4(name string) (net.IP, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		var ip net.IP
		switch address := address.(type) {
		case *net.IPNet:
			ip = address.IP
		case *net.IPAddr:
			ip = address.IP
		}
		if ip4 := ip.To4(); ip4 != nil && !ip4.IsUnspecified() {
			return ip4, nil
		}
	}
	return nil, fmt.Errorf("interface %s has no active IPv4 address", name)
}

func readClientTUN(ctx context.Context, tun *tunDevice, pool *linkPool) error {
	go func() {
		<-ctx.Done()
		tun.Close()
	}()
	buf := make([]byte, maxPacketSize)
	for {
		n, err := tun.Read(buf)
		if err != nil {
			return err
		}
		if n < 20 || buf[0]>>4 != 4 {
			continue
		}
		if err := pool.send(buf[:n]); err != nil && err != errNoLinks {
			log.Printf("send dropped: %v", err)
		}
	}
}

func logLinkStats(ctx context.Context, pool *linkPool) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, s := range pool.snapshot() {
					log.Printf("link=%s weight=%.0f%% rtt=%.1fms loss=%.1f%% tx=%.2fMbps rx=%.2fMbps sent=%d recv=%d",
						s.Name, s.WeightPct, s.RTTMs, s.LossPct, s.TxMbps, s.RxMbps, s.SentPackets, s.RecvPackets)
				}
			}
		}
	}()
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func sleepOrWake(ctx context.Context, duration time.Duration, wake <-chan struct{}) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}
