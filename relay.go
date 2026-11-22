package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

type relaySession struct {
	id          [16]byte
	ip          net.IP
	pool        *linkPool
	reassembler *reassembler
	emptySince  atomic.Int64
}

type relayServer struct {
	token       [32]byte
	tun         *tunDevice
	writePacket func([]byte) error
	reorderWait time.Duration
	mu          sync.Mutex
	sessions    map[[16]byte]*relaySession
	byIP        map[string]*relaySession
	nextIP      uint32
}

func runRelay(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("relay", flag.ExitOnError)
	listen := flags.String("listen", ":4545", "UDP listen address")
	cert := flags.String("cert", "relay.crt", "TLS certificate")
	key := flags.String("key", "relay.key", "TLS private key")
	tunName := flags.String("tun", "speedbond%d", "TUN interface")
	configure := flags.Bool("configure", true, "configure TUN, forwarding and iptables NAT")
	reorderWait := flags.Duration("reorder-wait", 0, "optional cross-link reorder wait (0 = direct delivery, recommended)")
	flags.Parse(args)
	token, err := requiredToken()
	if err != nil {
		return err
	}
	tlsConfig, err := relayTLS(*cert, *key)
	if err != nil {
		return err
	}
	tun, err := openTUN(*tunName)
	if err != nil {
		return fmt.Errorf("open tunnel (run as root): %w", err)
	}
	defer tun.Close()
	listenAddr, err := net.ResolveUDPAddr("udp4", *listen)
	if err != nil {
		return err
	}
	if *configure {
		cleanup, err := configureRelay(tun.Name(), listenAddr.Port)
		if err != nil {
			return fmt.Errorf("configure relay: %w", err)
		}
		defer cleanup()
	}
	udp, err := net.ListenUDP("udp4", listenAddr)
	if err != nil {
		return err
	}
	defer udp.Close()
	listener, err := quic.Listen(udp, tlsConfig, quicConfig())
	if err != nil {
		return err
	}
	defer listener.Close()
	server := newRelayServer(tokenHash(token), tun, *reorderWait, func(packet []byte) error {
		_, err := tun.Write(packet)
		return err
	})
	go server.expireSessions(ctx)
	go func() {
		<-ctx.Done()
		listener.Close()
		tun.Close()
	}()
	go server.readTUN(ctx)
	log.Printf("relay listening=%s tunnel=%s mtu=%d reorder=%s", listener.Addr(), tun.Name(), tunnelMTU, *reorderWait)
	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			return err
		}
		go server.handleConn(ctx, conn)
	}
}

func newRelayServer(token [32]byte, tun *tunDevice, reorderWait time.Duration, writePacket func([]byte) error) *relayServer {
	return &relayServer{
		token:       token,
		tun:         tun,
		writePacket: writePacket,
		reorderWait: reorderWait,
		sessions:    make(map[[16]byte]*relaySession),
		byIP:        make(map[string]*relaySession),
		nextIP:      2,
	}
}

func (s *relayServer) handleConn(ctx context.Context, conn *quic.Conn) {
	defer conn.CloseWithError(0, "connection closed")
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return
	}
	stream.SetDeadline(time.Now().Add(5 * time.Second))
	h, err := readHello(stream)
	if err != nil || !h.authorized(s.token) {
		writeHelloReply(stream, nil, fmt.Errorf("unauthorized"))
		return
	}
	session, err := s.sessionFor(h.Session)
	link := &bondLink{id: h.Link, conn: conn, name: conn.RemoteAddr().String()}
	if err == nil {
		err = session.pool.add(link)
	}
	if err != nil {
		writeHelloReply(stream, nil, err)
		return
	}
	session.emptySince.Store(0)
	if err = writeHelloReply(stream, session.ip, nil); err != nil {
		session.pool.remove(h.Link, conn)
		return
	}
	stream.Close()
	log.Printf("session=%x link=%d remote=%s connected address=%s active_links=%d",
		h.Session[:4], h.Link, conn.RemoteAddr(), session.ip, session.pool.count())
	err = receiveDatagrams(conn, session.reassembler, &link.received, &link.receivedBytes)
	session.pool.remove(h.Link, conn)
	if session.pool.count() == 0 {
		session.emptySince.Store(time.Now().UnixNano())
	}
	log.Printf("session=%x link=%d disconnected: %v active_links=%d",
		h.Session[:4], h.Link, err, session.pool.count())
}

func (s *relayServer) sessionFor(id [16]byte) (*relaySession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session := s.sessions[id]; session != nil {
		return session, nil
	}
	for attempts := 0; attempts < 65533; attempts++ {
		n := s.nextIP
		s.nextIP++
		if s.nextIP > 65534 {
			s.nextIP = 2
		}
		ip := net.IPv4(10, 77, byte(n>>8), byte(n))
		if s.byIP[ip.String()] != nil {
			continue
		}
		sessionIP := ip
		session := &relaySession{
			id:   id,
			ip:   sessionIP,
			pool: newLinkPool(),
			reassembler: newReassembler(s.reorderWait, func(packet []byte) error {
				if len(packet) < 20 || packet[0]>>4 != 4 || !net.IP(packet[12:16]).Equal(sessionIP) {
					return fmt.Errorf("invalid source address")
				}
				return s.writePacket(packet)
			}),
		}
		s.sessions[id] = session
		s.byIP[sessionIP.String()] = session
		return session, nil
	}
	return nil, fmt.Errorf("no tunnel addresses available")
}

func (s *relayServer) readTUN(ctx context.Context) {
	buf := make([]byte, maxPacketSize)
	for {
		n, err := s.tun.Read(buf)
		if err != nil {
			log.Printf("relay tunnel read stopped: %v", err)
			return
		}
		if n < 20 || buf[0]>>4 != 4 {
			continue
		}
		s.mu.Lock()
		session := s.byIP[net.IP(buf[16:20]).String()]
		s.mu.Unlock()
		if session != nil {
			session.pool.send(buf[:n])
		}
	}
}

func (s *relayServer) expireSessions(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for _, session := range s.sessions {
				session.reassembler.close()
			}
			s.mu.Unlock()
			return
		case now := <-ticker.C:
			s.mu.Lock()
			for id, session := range s.sessions {
				empty := session.emptySince.Load()
				if empty != 0 && now.Sub(time.Unix(0, empty)) > 2*time.Minute {
					session.reassembler.close()
					delete(s.sessions, id)
					delete(s.byIP, session.ip.String())
				}
			}
			s.mu.Unlock()
		}
	}
}
