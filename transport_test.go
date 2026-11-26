package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func TestTwoQUICLinksAndFailover(t *testing.T) {
	dir := t.TempDir()
	if err := generateCert(dir+"/relay.crt", dir+"/relay.key"); err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := relayTLS(dir+"/relay.crt", dir+"/relay.key")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	listener, err := quic.Listen(udp, tlsConfig, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token := "integration-test-secret"
	serverPool := newLinkPool()
	serverPackets := make(chan uint64, 128)
	serverReassembler := newReassembler(20*time.Millisecond, numberedDelivery(serverPackets))
	defer serverReassembler.close()
	go func() {
		for id := byte(0); id < 2; id++ {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			stream, err := conn.AcceptStream(ctx)
			if err != nil {
				return
			}
			h, err := readHello(stream)
			if err != nil || !h.authorized(tokenHash(token)) {
				t.Error("invalid client hello")
				return
			}
			link := &bondLink{id: h.Link, conn: conn, name: "server"}
			if err := serverPool.add(link); err != nil {
				t.Error(err)
				return
			}
			if err := writeHelloReply(stream, net.IPv4(10, 77, 0, 2), nil); err != nil {
				return
			}
			stream.Close()
			go func() {
				receiveDatagrams(conn, serverReassembler, &link.received, &link.receivedBytes)
				serverPool.remove(link.id, conn)
			}()
		}
	}()

	cert, _ := tls.LoadX509KeyPair(dir+"/relay.crt", dir+"/relay.key")
	pin := fmt.Sprintf("%x", sha256.Sum256(cert.Certificate[0]))
	clientTLSConfig, err := clientTLS(pin)
	if err != nil {
		t.Fatal(err)
	}
	clientPool := newLinkPool()
	clientPackets := make(chan uint64, 128)
	clientReassembler := newReassembler(20*time.Millisecond, numberedDelivery(clientPackets))
	defer clientReassembler.close()
	session, _ := newSession()
	for id := byte(0); id < 2; id++ {
		link, _, err := dialClientLink(ctx, listener.Addr().(*net.UDPAddr), clientTLSConfig, token, session, id, "lo0")
		if err != nil {
			t.Fatal(err)
		}
		if err := clientPool.add(link); err != nil {
			t.Fatal(err)
		}
		go func() {
			receiveDatagrams(link.conn, clientReassembler, &link.received, &link.receivedBytes)
			clientPool.remove(link.id, link.conn)
		}()
		defer link.close(0, "test complete")
	}
	for i := uint64(0); i < 40; i++ {
		if err := clientPool.send(numberedPacket(i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := uint64(0); i < 40; i++ {
		if got := receiveNumber(t, serverPackets); got != i {
			t.Fatalf("packet %d arrived as %d", i, got)
		}
	}

	clientPool.mu.Lock()
	dead := clientPool.links[0]
	clientPool.mu.Unlock()
	dead.close(0, "simulated link failure")
	waitFor(t, func() bool { return clientPool.count() == 1 && serverPool.count() == 1 })
	for i := uint64(40); i < 60; i++ {
		if err := clientPool.send(numberedPacket(i)); err != nil {
			t.Fatal(err)
		}
		if err := serverPool.send(numberedPacket(i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := uint64(40); i < 60; i++ {
		if got := receiveNumber(t, serverPackets); got != i {
			t.Fatalf("post-failover upload %d arrived as %d", i, got)
		}
		if got := receiveNumber(t, clientPackets); got != i {
			t.Fatalf("post-failover download %d arrived as %d", i, got)
		}
	}
}

func TestRelaySharedSessionReassemblyAndBridge(t *testing.T) {
	dir := t.TempDir()
	if err := generateCert(dir+"/relay.crt", dir+"/relay.key"); err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := relayTLS(dir+"/relay.crt", dir+"/relay.key")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	listener, err := quic.Listen(udp, tlsConfig, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	token := "relay-shared-session-secret"
	delivered := make(chan uint64, 128)
	server := newRelayServer(tokenHash(token), nil, 50*time.Millisecond, func(pkt []byte) error {
		delivered <- binary.BigEndian.Uint64(pkt[20:28])
		return nil
	})
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go server.handleConn(ctx, conn)
		}
	}()

	// Also test UDP bridge for the second link (same path used for Alfa VM).
	bridgeProbe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	bridgeAddr := bridgeProbe.LocalAddr().String()
	bridgeProbe.Close()
	go serveUDPBridge(ctx, bridgeAddr, listener.Addr().String(), "")
	time.Sleep(50 * time.Millisecond)

	cert, _ := tls.LoadX509KeyPair(dir+"/relay.crt", dir+"/relay.key")
	pin := fmt.Sprintf("%x", sha256.Sum256(cert.Certificate[0]))
	clientTLSConfig, err := clientTLS(pin)
	if err != nil {
		t.Fatal(err)
	}

	session, _ := newSession()
	pool := newLinkPool()
	link0, ip0, err := dialClientLink(ctx, listener.Addr().(*net.UDPAddr), clientTLSConfig, token, session, 0, "lo0")
	if err != nil {
		t.Fatal(err)
	}
	defer link0.close(0, "done")
	link1, ip1, err := dialClientLink(ctx, listener.Addr().(*net.UDPAddr), clientTLSConfig, token, session, 1, "alfa="+bridgeAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer link1.close(0, "done")
	if !ip0.Equal(ip1) {
		t.Fatalf("expected same session IP, got %v vs %v", ip0, ip1)
	}
	pool.add(link0)
	pool.add(link1)

	for i := uint64(0); i < 40; i++ {
		pkt := ipv4TestPacket(ip0, i)
		if err := pool.send(pkt); err != nil {
			t.Fatal(err)
		}
	}
	for i := uint64(0); i < 40; i++ {
		got := receiveNumber(t, delivered)
		if got != i {
			t.Fatalf("relay packet %d arrived as %d (reassembler not shared!)", i, got)
		}
	}

	server.mu.Lock()
	sess := server.sessions[session]
	server.mu.Unlock()
	if st := sess.reassembler.stats(); st.GapsSkipped != 0 {
		t.Fatalf("expected 0 skipped gaps on shared relay reassembler, got %d", st.GapsSkipped)
	}
}

func TestSmoothWeightedRoundRobin(t *testing.T) {
	p := newLinkPool()
	a := &bondLink{id: 0, name: "fast"}
	b := &bondLink{id: 1, name: "slow"}
	p.add(a)
	p.add(b)
	p.mu.Lock()
	a.weight = 3
	b.weight = 1
	counts := map[byte]int{}
	for i := 0; i < 40; i++ {
		l := p.pickLocked(nil)
		counts[l.id]++
	}
	p.mu.Unlock()
	if counts[0] != 30 || counts[1] != 10 {
		t.Fatalf("unexpected weighted distribution: %v", counts)
	}
}

func TestClientSyncTunnelIP(t *testing.T) {
	mgr := &clientManager{
		tunName:  "utun99",
		tunnelIP: net.IPv4(10, 77, 0, 29),
		route:    false,
	}
	// Same IP is a no-op
	mgr.syncTunnelIP(net.IPv4(10, 77, 0, 29))
	if !mgr.tunnelIP.Equal(net.IPv4(10, 77, 0, 29)) {
		t.Fatalf("unexpected ip: %v", mgr.tunnelIP)
	}
	// New IP after relay session expiry updates tunnelIP
	mgr.syncTunnelIP(net.IPv4(10, 77, 0, 42))
	if !mgr.tunnelIP.Equal(net.IPv4(10, 77, 0, 42)) {
		t.Fatalf("expected updated ip 10.77.0.42, got %v", mgr.tunnelIP)
	}
}

func TestPickFastestRelay(t *testing.T) {
	dir := t.TempDir()
	if err := generateCert(dir+"/relay.crt", dir+"/relay.key"); err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := relayTLS(dir+"/relay.crt", dir+"/relay.key")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := tls.LoadX509KeyPair(dir+"/relay.crt", dir+"/relay.key")
	pin := fmt.Sprintf("%x", sha256.Sum256(cert.Certificate[0]))
	token := "pick-fastest-relay-secret"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start one live relay on loopback
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	listener, err := quic.Listen(udp, tlsConfig, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	srv := newRelayServer(tokenHash(token), nil, 0, func([]byte) error { return nil })
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go srv.handleConn(ctx, conn)
		}
	}()

	relays := []relayEntry{
		{Name: "Dead", Server: "127.0.0.1:1", Pin: pin, Token: token},
		{Name: "Live", Server: listener.Addr().String(), Pin: pin, Token: token},
	}
	picked := pickBestRelay(ctx, relays, "auto", 2*time.Second)
	if picked.Best == nil || picked.Best.Name != "Live" || !picked.Best.OK {
		t.Fatalf("expected Live relay to be picked, got %+v", picked)
	}
	// If preferred relay is dead, automatically falls back to fastest healthy relay
	fallback := pickBestRelay(ctx, relays, "Dead", 1*time.Second)
	if fallback.Best == nil || fallback.Best.Name != "Live" {
		t.Fatalf("expected fallback to Live relay, got %+v", fallback)
	}
}

func ipv4TestPacket(srcIP net.IP, seq uint64) []byte {
	pkt := make([]byte, 28)
	pkt[0] = 0x45 // IPv4, IHL=5
	copy(pkt[12:16], srcIP.To4())
	copy(pkt[16:20], net.IPv4(1, 1, 1, 1).To4())
	binary.BigEndian.PutUint64(pkt[20:28], seq)
	return pkt
}

func numberedPacket(n uint64) []byte {
	packet := make([]byte, 20)
	binary.BigEndian.PutUint64(packet, n)
	return packet
}

func numberedDelivery(ch chan<- uint64) func([]byte) error {
	return func(packet []byte) error {
		ch <- binary.BigEndian.Uint64(packet)
		return nil
	}
}

func receiveNumber(t *testing.T, ch <-chan uint64) uint64 {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for packet")
		return 0
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
