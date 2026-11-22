package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

type bridgeSession struct {
	clientAddr *net.UDPAddr
	upstream   *net.UDPConn
	lastSeen   time.Time
}

func runBridge(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("bridge", flag.ExitOnError)
	listen := flags.String("listen", ":4546", "local UDP listen address")
	target := flags.String("target", "", "relay UDP address host:port")
	iface := flags.String("iface", "", "outbound network interface to bind (optional)")
	flags.Parse(args)
	if *target == "" {
		return fmt.Errorf("-target host:port is required")
	}
	return serveUDPBridge(ctx, *listen, *target, *iface)
}

func serveUDPBridge(ctx context.Context, listenAddr, targetAddr, ifaceName string) error {
	laddr, err := net.ResolveUDPAddr("udp4", listenAddr)
	if err != nil {
		return err
	}
	raddr, err := net.ResolveUDPAddr("udp4", targetAddr)
	if err != nil {
		return err
	}
	front, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return err
	}
	_ = front.SetReadBuffer(2 << 20)
	_ = front.SetWriteBuffer(2 << 20)
	defer front.Close()

	go func() {
		<-ctx.Done()
		front.Close()
	}()

	var (
		mu       sync.Mutex
		sessions = make(map[string]*bridgeSession)
	)
	defer func() {
		mu.Lock()
		for _, s := range sessions {
			s.upstream.Close()
		}
		mu.Unlock()
	}()

	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				mu.Lock()
				for k, s := range sessions {
					if now.Sub(s.lastSeen) > 60*time.Second {
						s.upstream.Close()
						delete(sessions, k)
					}
				}
				mu.Unlock()
			}
		}
	}()

	log.Printf("bridge listening=%s target=%s iface=%s", front.LocalAddr(), raddr, ifaceName)
	buf := make([]byte, 2048)
	for {
		n, src, err := front.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		key := src.String()
		now := time.Now()
		mu.Lock()
		sess := sessions[key]
		if sess != nil {
			sess.lastSeen = now
		}
		mu.Unlock()

		if sess == nil {
			up, err := dialBridgeUpstream(raddr, ifaceName)
			if err != nil {
				log.Printf("bridge upstream open failed (%s): %v", ifaceName, err)
				continue
			}
			sess = &bridgeSession{clientAddr: src, upstream: up, lastSeen: now}
			mu.Lock()
			if existing := sessions[key]; existing != nil {
				up.Close()
				sess = existing
				sess.lastSeen = now
			} else {
				sessions[key] = sess
				go func(k string, s *bridgeSession) {
					rbuf := make([]byte, 2048)
					for {
						rn, err := s.upstream.Read(rbuf)
						if err != nil {
							mu.Lock()
							if sessions[k] == s {
								delete(sessions, k)
							}
							mu.Unlock()
							return
						}
						mu.Lock()
						s.lastSeen = time.Now()
						mu.Unlock()
						front.WriteToUDP(rbuf[:rn], s.clientAddr)
					}
				}(key, sess)
			}
			mu.Unlock()
		}
		if _, err := sess.upstream.Write(buf[:n]); err != nil {
			mu.Lock()
			if sessions[key] == sess {
				sess.upstream.Close()
				delete(sessions, key)
			}
			mu.Unlock()
		}
	}
}

func dialBridgeUpstream(remote *net.UDPAddr, ifaceName string) (*net.UDPConn, error) {
	var localIP net.IP
	if ifaceName != "" {
		ip, err := interfaceIPv4(ifaceName)
		if err != nil {
			return nil, err
		}
		localIP = ip
	}
	conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: localIP, Port: 0}, remote)
	if err != nil {
		return nil, err
	}
	if ifaceName != "" {
		raw, err := conn.SyscallConn()
		if err == nil {
			var bindErr error
			err = raw.Control(func(fd uintptr) { bindErr = bindToInterface(fd, ifaceName) })
			if err == nil {
				err = bindErr
			}
		}
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("bind %s: %w", ifaceName, err)
		}
	}
	return conn, nil
}
