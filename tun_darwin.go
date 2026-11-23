//go:build darwin

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

type tunDevice struct {
	file   *os.File
	name   string
	closed atomic.Bool
}

func openTUN(name string) (*tunDevice, error) {
	if name != "" && name != "auto" {
		return nil, fmt.Errorf("macOS chooses the utun name; use --tun auto")
	}
	for unit := uint32(1); unit <= 64; unit++ {
		fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2)
		if err != nil {
			return nil, err
		}
		info := unix.CtlInfo{Name: ctlName("com.apple.net.utun_control")}
		if err = unix.IoctlCtlInfo(fd, &info); err == nil {
			err = unix.Connect(fd, &unix.SockaddrCtl{ID: info.Id, Unit: unit})
		}
		if err == nil {
			return &tunDevice{file: os.NewFile(uintptr(fd), "utun"), name: fmt.Sprintf("utun%d", unit-1)}, nil
		}
		unix.Close(fd)
		if err != syscall.EADDRINUSE && err != syscall.EBUSY {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no free utun device")
}

func ctlName(s string) (name [96]byte) {
	copy(name[:], s)
	return
}

func (t *tunDevice) Name() string { return t.name }
func (t *tunDevice) Close() error {
	if t.closed.CompareAndSwap(false, true) {
		return t.file.Close()
	}
	return nil
}

func (t *tunDevice) Read(p []byte) (int, error) {
	for {
		if t.closed.Load() {
			return 0, net.ErrClosed
		}
		fd := []unix.PollFd{{Fd: int32(t.file.Fd()), Events: unix.POLLIN}}
		ready, err := unix.Poll(fd, 250)
		if err != nil && err != syscall.EINTR {
			return 0, err
		}
		if ready == 0 {
			continue
		}
		buf := make([]byte, len(p)+4)
		n, err := t.file.Read(buf)
		if n < 4 {
			return 0, err
		}
		return copy(p, buf[4:n]), err
	}
}

func (t *tunDevice) Write(p []byte) (int, error) {
	buf := make([]byte, len(p)+4)
	family := uint32(unix.AF_INET)
	if len(p) > 0 && p[0]>>4 == 6 {
		family = unix.AF_INET6
	}
	binary.BigEndian.PutUint32(buf, family)
	copy(buf[4:], p)
	n, err := t.file.Write(buf)
	if n >= 4 {
		n -= 4
	}
	return n, err
}

func bindToInterface(fd uintptr, name string) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, iface.Index)
}

func configureRelayHost(ip net.IP, interfaceName string) (func(), error) {
	// Look up the interface's current default gateway (not ip) so a stale host route
	// from a previous Wi-Fi network or subnet can never shadow the new gateway.
	out, err := exec.Command("route", "-n", "get", "-ifscope", interfaceName, "default").CombinedOutput()
	if err != nil {
		out, err = exec.Command("route", "-n", "get", "-ifscope", interfaceName, ip.String()).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("find gateway for %s: %w: %s", interfaceName, err, out)
		}
	}
	var gateway string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			gateway = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		}
	}
	if gateway == "" {
		return nil, fmt.Errorf("no gateway for relay on %s", interfaceName)
	}
	if strings.HasPrefix(gateway, "index:") {
		return func() {}, nil
	}
	// Delete any stale host route for this interface before adding the current gateway.
	exec.Command("route", "-n", "delete", "-host", "-ifscope", interfaceName, ip.String()).Run()
	if err := run("route", "-n", "add", "-host", "-ifscope", interfaceName, ip.String(), gateway); err != nil {
		if strings.Contains(err.Error(), "File exists") {
			return func() {}, nil
		}
		return nil, err
	}
	return func() {
		exec.Command("route", "-n", "delete", "-host", "-ifscope", interfaceName, ip.String(), gateway).Run()
	}, nil
}

func configureClient(name string, ip net.IP) (func(), error) {
	if err := run("ifconfig", name, "inet", ip.String(), "10.77.0.1", "netmask", "255.255.0.0", "mtu", fmt.Sprint(tunnelMTU), "up"); err != nil {
		return nil, err
	}
	added := 0
	for _, network := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		if err := run("route", "-n", "add", "-net", network, "-interface", name); err != nil {
			cleanupClient(name, added)
			return nil, err
		}
		added++
	}
	return func() { cleanupClient(name, added) }, nil
}

func cleanupClient(name string, routes int) {
	networks := []string{"0.0.0.0/1", "128.0.0.0/1"}
	for i := routes - 1; i >= 0; i-- {
		exec.Command("route", "-n", "delete", "-net", networks[i], "-interface", name).Run()
	}
}

func configureRelay(name string, port int) (func(), error) {
	return nil, fmt.Errorf("relay mode is supported on Linux only")
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}
