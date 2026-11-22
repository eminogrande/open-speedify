package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

func runClient(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("client", flag.ExitOnError)
	server := flags.String("server", "", "relay address host:port")
	linkNames := flags.String("links", "", "comma-separated interfaces, e.g. en0,en5")
	pin := flags.String("pin", "", "relay certificate SHA-256 fingerprint")
	tunName := flags.String("tun", "auto", "tunnel interface")
	route := flags.Bool("route", true, "route IPv4 internet traffic through tunnel")
	reorderWait := flags.Duration("reorder-wait", 30*time.Millisecond, "maximum wait for missing sequence numbers")
	flags.Parse(args)
	token, err := requiredToken()
	if err != nil {
		return err
	}
	names, err := splitLinks(*linkNames)
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
	hostCleanup := func() {}
	if *route {
		hostCleanup, err = configureRelayHost(remote.IP, names[0])
		if err != nil {
			return fmt.Errorf("preserve direct relay route: %w", err)
		}
	}
	defer hostCleanup()
	pool := newLinkPool()
	reassembler := newReassembler(*reorderWait, func(packet []byte) error {
		_, err := tun.Write(packet)
		return err
	})
	ready := make(chan net.IP, 1)
	for i, name := range names {
		go manageClientLink(ctx, remote, tlsConfig, token, session, byte(i), name, pool, reassembler, ready)
	}
	var ip net.IP
	select {
	case ip = <-ready:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
		return fmt.Errorf("no link connected within 15 seconds")
	}
	cleanup := func() {}
	if *route {
		cleanup, err = configureClient(tun.Name(), ip)
		if err != nil {
			return fmt.Errorf("configure tunnel routes: %w", err)
		}
	}
	defer cleanup()
	log.Printf("connected: tunnel=%s address=%s links=%d", tun.Name(), ip, pool.count())
	logLinkStats(ctx, pool)
	return readClientTUN(ctx, tun, pool)
}

func manageClientLink(ctx context.Context, remote *net.UDPAddr, tlsConfig *tls.Config, token string, session [16]byte, id byte, name string, pool *linkPool, reassembler *reassembler, ready chan<- net.IP) {
	for ctx.Err() == nil {
		link, ip, err := dialClientLink(ctx, remote, tlsConfig, token, session, id, name)
		if err == nil {
			err = pool.add(link)
		}
		if err != nil {
			log.Printf("link=%s connect failed: %v", name, err)
			if link != nil {
				link.conn.CloseWithError(1, "setup failed")
			}
			if !sleepContext(ctx, time.Second) {
				return
			}
			continue
		}
		select {
		case ready <- ip:
		default:
		}
		log.Printf("link=%s connected rtt=%s", name, link.conn.ConnectionStats().SmoothedRTT)
		err = receiveDatagrams(link.conn, reassembler, &link.received)
		pool.remove(id, link.conn)
		link.conn.CloseWithError(0, "reconnecting")
		log.Printf("link=%s disconnected: %v; reconnecting", name, err)
		if !sleepContext(ctx, 500*time.Millisecond) {
			return
		}
	}
}

func dialClientLink(ctx context.Context, remote *net.UDPAddr, tlsConfig *tls.Config, token string, session [16]byte, id byte, name string) (*bondLink, net.IP, error) {
	localIP, err := interfaceIPv4(name)
	if err != nil {
		return nil, nil, err
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: localIP, Port: 0})
	if err != nil {
		return nil, nil, err
	}
	raw, err := udp.SyscallConn()
	if err == nil {
		var bindErr error
		err = raw.Control(func(fd uintptr) { bindErr = bindToInterface(fd, name) })
		if err == nil {
			err = bindErr
		}
	}
	if err != nil {
		udp.Close()
		return nil, nil, fmt.Errorf("bind %s: %w", name, err)
	}
	conn, err := quic.Dial(ctx, udp, remote, tlsConfig, quicConfig())
	if err != nil {
		udp.Close()
		return nil, nil, err
	}
	link := &bondLink{id: id, name: name, conn: conn}
	fail := func(err error) (*bondLink, net.IP, error) {
		conn.CloseWithError(1, "handshake failed")
		return link, nil, err
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
	buf := make([]byte, tunnelMTU)
	for {
		n, err := tun.Read(buf)
		if err != nil {
			return err
		}
		if n < 20 {
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
				pool.mu.Lock()
				for _, link := range pool.links {
					log.Printf("link=%s rtt=%s sent=%d received=%d", link.name, link.conn.ConnectionStats().SmoothedRTT, link.sent.Load(), link.received.Load())
				}
				pool.mu.Unlock()
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
