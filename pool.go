package main

import (
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/quic-go/quic-go"
)

var errNoLinks = errors.New("no active links")

type bondLink struct {
	id       byte
	name     string
	conn     *quic.Conn
	sent     atomic.Uint64
	received atomic.Uint64
}

type linkPool struct {
	mu     sync.Mutex
	links  map[byte]*bondLink
	order  []byte
	cursor int
	seq    uint64
}

func newLinkPool() *linkPool {
	return &linkPool{links: make(map[byte]*bondLink)}
}

func (p *linkPool) add(link *bondLink) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.links[link.id]; exists {
		return errors.New("link ID already connected")
	}
	p.links[link.id] = link
	p.order = p.order[:0]
	for id := range p.links {
		p.order = append(p.order, id)
	}
	sort.Slice(p.order, func(i, j int) bool { return p.order[i] < p.order[j] })
	return nil
}

func (p *linkPool) remove(id byte, conn *quic.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.links[id] != nil && p.links[id].conn == conn {
		delete(p.links, id)
		p.order = p.order[:0]
		for id := range p.links {
			p.order = append(p.order, id)
		}
		sort.Slice(p.order, func(i, j int) bool { return p.order[i] < p.order[j] })
	}
}

func (p *linkPool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.links)
}

func (p *linkPool) send(packet []byte) error {
	p.mu.Lock()
	if len(p.order) == 0 {
		p.mu.Unlock()
		return errNoLinks
	}
	id := p.order[p.cursor%len(p.order)]
	p.cursor++
	link := p.links[id]
	seq := p.seq
	p.seq++
	p.mu.Unlock()

	err := link.conn.SendDatagram(encodeFrame(seq, packet))
	if err == nil {
		link.sent.Add(1)
	} else if link.conn.Context().Err() != nil {
		p.remove(id, link.conn)
	}
	return err
}

func receiveDatagrams(conn *quic.Conn, r *reassembler, received *atomic.Uint64) error {
	for {
		frame, err := conn.ReceiveDatagram(conn.Context())
		if err != nil {
			return err
		}
		if len(frame) < 9 || len(frame)-8 > tunnelMTU {
			continue
		}
		received.Add(1)
		r.push(binary.BigEndian.Uint64(frame), frame[8:])
	}
}

func runUntilError(cancel func(), workers ...func() error) error {
	errs := make(chan error, len(workers))
	for _, worker := range workers {
		go func() { errs <- worker() }()
	}
	err := <-errs
	cancel()
	for range workers[1:] {
		<-errs
	}
	return err
}

func closeAll(c ...io.Closer) {
	for _, closer := range c {
		closer.Close()
	}
}
