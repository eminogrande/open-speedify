package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
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
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `open-speedify: experimental Wi-Fi + phone bonding VPN

  speedbond interfaces
  speedbond cert [-cert relay.crt] [-key relay.key]
  SPEEDBOND_TOKEN=... speedbond relay -listen :4545 -cert relay.crt -key relay.key
  sudo -E SPEEDBOND_TOKEN=... speedbond client -server VPS:4545 -links en0,en5 -pin FINGERPRINT

The client requires root and changes IPv4 default routes. Stop it to restore routes.
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
		MaxIdleTimeout:       5 * time.Second,
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
	var links []string
	seen := make(map[string]bool)
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return nil, fmt.Errorf("links must be distinct interface names")
		}
		seen[name] = true
		links = append(links, name)
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("at least one link is required")
	}
	return links, nil
}
