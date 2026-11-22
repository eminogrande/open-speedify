package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

const (
	protocolName  = "open-speedify/1"
	helloSize     = 16 + 1 + sha256.Size
	tunnelMTU     = 1150
	maxPacketSize = 1400
)

type hello struct {
	Session [16]byte
	Link    byte
	Token   [sha256.Size]byte
}

func newSession() ([16]byte, error) {
	var id [16]byte
	_, err := rand.Read(id[:])
	return id, err
}

func tokenHash(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

func (h hello) writeTo(w io.Writer) error {
	var b [helloSize]byte
	copy(b[:16], h.Session[:])
	b[16] = h.Link
	copy(b[17:], h.Token[:])
	_, err := w.Write(b[:])
	return err
}

func readHello(r io.Reader) (hello, error) {
	var h hello
	var b [helloSize]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return h, err
	}
	copy(h.Session[:], b[:16])
	h.Link = b[16]
	copy(h.Token[:], b[17:])
	return h, nil
}

func (h hello) authorized(want [sha256.Size]byte) bool {
	return subtle.ConstantTimeCompare(h.Token[:], want[:]) == 1
}

func writeHelloReply(w io.Writer, ip net.IP, err error) error {
	status := byte(0)
	if err != nil {
		status = 1
	}
	reply := []byte{status, 0, 0, 0, 0}
	if ip != nil {
		copy(reply[1:], ip.To4())
	}
	_, writeErr := w.Write(reply)
	return writeErr
}

func readHelloReply(r io.Reader) (net.IP, error) {
	var reply [5]byte
	if _, err := io.ReadFull(r, reply[:]); err != nil {
		return nil, err
	}
	if reply[0] != 0 {
		return nil, errors.New("relay rejected link (check token and duplicate link IDs)")
	}
	ip := net.IPv4(reply[1], reply[2], reply[3], reply[4])
	if ip.IsUnspecified() {
		return nil, fmt.Errorf("relay returned invalid tunnel address %v", ip)
	}
	return ip, nil
}

func encodeFrame(seq uint64, packet []byte) []byte {
	frame := make([]byte, 8+len(packet))
	binary.BigEndian.PutUint64(frame, seq)
	copy(frame[8:], packet)
	return frame
}
