//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

type tunDevice struct {
	file *os.File
	name string
}

func openTUN(name string) (*tunDevice, error) {
	if name == "" || name == "auto" {
		name = "speedbond%d"
	}
	file, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var ifreq [unix.IFNAMSIZ + 2]byte
	copy(ifreq[:unix.IFNAMSIZ-1], name)
	*(*uint16)(unsafe.Pointer(&ifreq[unix.IFNAMSIZ])) = unix.IFF_TUN | unix.IFF_NO_PI
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), unix.TUNSETIFF, uintptr(unsafe.Pointer(&ifreq[0]))); errno != 0 {
		file.Close()
		return nil, errno
	}
	actual := string(ifreq[:unix.IFNAMSIZ])
	if i := strings.IndexByte(actual, 0); i >= 0 {
		actual = actual[:i]
	}
	return &tunDevice{file: file, name: actual}, nil
}

func (t *tunDevice) Name() string { return t.name }
func (t *tunDevice) Close() error { return t.file.Close() }
func (t *tunDevice) Read(p []byte) (int, error) {
	return unix.Read(int(t.file.Fd()), p)
}
func (t *tunDevice) Write(p []byte) (int, error) {
	return unix.Write(int(t.file.Fd()), p)
}

func bindToInterface(fd uintptr, name string) error {
	return unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
}

func configureRelayHost(ip net.IP, interfaceName string) (func(), error) {
	out, err := exec.Command("ip", "-o", "-4", "route", "show", "default", "dev", interfaceName).Output()
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		return nil, fmt.Errorf("no gateway for relay on %s", interfaceName)
	}
	if err := run("ip", "route", "add", ip.String()+"/32", "via", fields[2], "dev", interfaceName); err != nil {
		if strings.Contains(err.Error(), "File exists") {
			return func() {}, nil
		}
		return nil, err
	}
	return func() {
		exec.Command("ip", "route", "del", ip.String()+"/32", "via", fields[2], "dev", interfaceName).Run()
	}, nil
}

func configureClient(name string, ip net.IP) (func(), error) {
	if err := run("ip", "addr", "add", ip.String()+"/16", "dev", name); err != nil {
		return nil, err
	}
	if err := run("ip", "link", "set", "dev", name, "mtu", fmt.Sprint(tunnelMTU), "up"); err != nil {
		return nil, err
	}
	for _, network := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		if err := run("ip", "route", "replace", network, "dev", name); err != nil {
			return nil, err
		}
	}
	return func() {
		for _, network := range []string{"128.0.0.0/1", "0.0.0.0/1"} {
			exec.Command("ip", "route", "del", network, "dev", name).Run()
		}
	}, nil
}

func updateClientIP(name string, oldIP, newIP net.IP) error {
	if err := run("ip", "addr", "replace", newIP.String()+"/16", "dev", name); err != nil {
		return err
	}
	if oldIP != nil && !oldIP.Equal(newIP) {
		exec.Command("ip", "addr", "del", oldIP.String()+"/16", "dev", name).Run()
	}
	for _, network := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		if err := run("ip", "route", "replace", network, "dev", name); err != nil {
			return err
		}
	}
	return nil
}

func configureRelay(name string, port int) (func(), error) {
	out, err := exec.Command("ip", "-o", "-4", "route", "show", "default").Output()
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(out))
	var external string
	for i := range fields {
		if fields[i] == "dev" && i+1 < len(fields) {
			external = fields[i+1]
			break
		}
	}
	if external == "" {
		return nil, fmt.Errorf("no default route found")
	}
	forward, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return nil, err
	}
	restoreForward := strings.TrimSpace(string(forward)) != "1"
	commands := [][]string{
		{"ip", "addr", "add", "10.77.0.1/16", "dev", name},
		{"ip", "link", "set", "dev", name, "mtu", fmt.Sprint(tunnelMTU), "up"},
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
	}
	commands = append(commands, relayRules(name, external, port)...)
	cleanup := func(completed int) {
		cleanupRelay(name, external, port, completed-3)
		if restoreForward {
			os.WriteFile("/proc/sys/net/ipv4/ip_forward", forward, 0644)
		}
	}
	for i, command := range commands {
		if err := run(command[0], command[1:]...); err != nil {
			cleanup(i)
			return nil, err
		}
	}
	return func() { cleanup(len(commands)) }, nil
}

func relayRules(name, external string, port int) [][]string {
	return [][]string{
		{"iptables", "-I", "INPUT", "1", "-p", "udp", "--dport", fmt.Sprint(port), "-m", "comment", "--comment", "open-speedify", "-j", "ACCEPT"},
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "10.77.0.0/16", "-o", external, "-m", "comment", "--comment", "open-speedify", "-j", "MASQUERADE"},
		{"iptables", "-I", "FORWARD", "1", "-i", name, "-o", external, "-m", "comment", "--comment", "open-speedify", "-j", "ACCEPT"},
		{"iptables", "-I", "FORWARD", "1", "-i", external, "-o", name, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-m", "comment", "--comment", "open-speedify", "-j", "ACCEPT"},
		{"iptables", "-t", "mangle", "-I", "FORWARD", "1", "-i", name, "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-m", "comment", "--comment", "open-speedify", "-j", "TCPMSS", "--clamp-mss-to-pmtu"},
		{"iptables", "-t", "mangle", "-I", "FORWARD", "1", "-o", name, "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-m", "comment", "--comment", "open-speedify", "-j", "TCPMSS", "--clamp-mss-to-pmtu"},
	}
}

func cleanupRelay(name, external string, port, count int) {
	rules := relayRules(name, external, port)
	if count > len(rules) {
		count = len(rules)
	}
	for i := count - 1; i >= 0; i-- {
		rule := append([]string{}, rules[i]...)
		position := 1
		if rule[1] == "-t" {
			position = 3
		}
		insert := rule[position] == "-I"
		rule[position] = "-D"
		if insert {
			rule = append(rule[:position+1], rule[position+2:]...)
		}
		exec.Command(rule[0], rule[1:]...).Run()
	}
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}
