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
				receiveDatagrams(conn, serverReassembler, &link.received)
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
			receiveDatagrams(link.conn, clientReassembler, &link.received)
			clientPool.remove(link.id, link.conn)
		}()
		defer link.conn.CloseWithError(0, "test complete")
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
	dead.conn.CloseWithError(0, "simulated link failure")
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
