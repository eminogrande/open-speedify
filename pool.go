package main

import (
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	linkSendQueueLen = 64
	linkSpillThresh  = 8
)

var errNoLinks = errors.New("no active links")

type linkSnapshot struct {
	ID          byte    `json:"id"`
	Name        string  `json:"name"`
	Target      string  `json:"target,omitempty"`
	Connected   bool    `json:"connected"`
	Weight      int     `json:"weight"`
	WeightPct   float64 `json:"weight_pct"`
	RTTMs       float64 `json:"rtt_ms"`
	MinRTTMs    float64 `json:"min_rtt_ms"`
	LossPct     float64 `json:"loss_pct"`
	TxMbps      float64 `json:"tx_mbps"`
	RxMbps      float64 `json:"rx_mbps"`
	SentPackets uint64  `json:"sent_packets"`
	RecvPackets uint64  `json:"recv_packets"`
	SentBytes   uint64  `json:"sent_bytes"`
	RecvBytes   uint64  `json:"recv_bytes"`
	SendErrors  uint64  `json:"send_errors"`
}

type bondLink struct {
	id            byte
	name          string
	target        string
	conn          *quic.Conn
	udp           io.Closer
	sent          atomic.Uint64
	received      atomic.Uint64
	sentBytes     atomic.Uint64
	receivedBytes atomic.Uint64
	sendErrors    atomic.Uint64

	// Async non-blocking send queue (MinRTT + QUIC backpressure spillover)
	txCh   chan []byte
	txStop chan struct{}
	once   sync.Once

	// Protected by linkPool.mu
	weight        int
	currentWeight int
	lastSample    time.Time
	prevSentBytes uint64
	prevRecvBytes uint64
	prevQSent     uint64
	prevQLost     uint64
	txBps         float64
	rxBps         float64
	lossPct       float64
	effRTTMs      float64
}

func (l *bondLink) startSender(p *linkPool) {
	if l.conn == nil || l.txCh != nil {
		return
	}
	l.txCh = make(chan []byte, linkSendQueueLen)
	l.txStop = make(chan struct{})
	go func() {
		for {
			select {
			case <-l.txStop:
				return
			case <-l.conn.Context().Done():
				p.remove(l.id, l.conn)
				return
			case frame := <-l.txCh:
				err := l.conn.SendDatagram(frame)
				if err == nil {
					l.sent.Add(1)
					if len(frame) > 8 {
						l.sentBytes.Add(uint64(len(frame) - 8))
					}
				} else {
					l.sendErrors.Add(1)
					if l.conn.Context().Err() != nil {
						p.remove(l.id, l.conn)
						return
					}
				}
			}
		}
	}()
}

func (l *bondLink) stopSender() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.txStop != nil {
			close(l.txStop)
		}
	})
}

func (l *bondLink) close(code quic.ApplicationErrorCode, reason string) {
	if l == nil {
		return
	}
	l.stopSender()
	if l.conn != nil {
		l.conn.CloseWithError(code, reason)
	}
	if l.udp != nil {
		l.udp.Close()
	}
}

type linkPool struct {
	mu            sync.Mutex
	links         map[byte]*bondLink
	order         []byte
	seq           uint64
	flowCursor    uint64
	flowPin       map[uint32]byte
	lastRebalance time.Time
}

func newLinkPool() *linkPool {
	return &linkPool{
		links:         make(map[byte]*bondLink),
		flowPin:       make(map[uint32]byte),
		lastRebalance: time.Now(),
	}
}

func (p *linkPool) add(link *bondLink) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.links[link.id]; exists {
		return errors.New("link ID already connected")
	}
	link.weight = 1
	link.effRTTMs = 100.0
	link.lastSample = time.Now()
	if link.conn != nil {
		st := link.conn.ConnectionStats()
		link.prevQSent = st.PacketsSent
		link.prevQLost = st.PacketsLost
		if st.SmoothedRTT > 0 {
			link.effRTTMs = float64(st.SmoothedRTT) / float64(time.Millisecond)
		}
		link.startSender(p)
	}
	p.links[link.id] = link
	p.rebuildOrderLocked()
	return nil
}

func (p *linkPool) remove(id byte, conn *quic.Conn) {
	p.mu.Lock()
	l := p.links[id]
	if l != nil && l.conn == conn {
		delete(p.links, id)
		p.rebuildOrderLocked()
		p.mu.Unlock()
		l.stopSender()
		return
	}
	p.mu.Unlock()
}

func (p *linkPool) rebuildOrderLocked() {
	p.order = p.order[:0]
	for id := range p.links {
		p.order = append(p.order, id)
	}
	sort.Slice(p.order, func(i, j int) bool {
		a, b := p.links[p.order[i]], p.links[p.order[j]]
		if a.effRTTMs != b.effRTTMs {
			return a.effRTTMs < b.effRTTMs
		}
		return a.id < b.id
	})
}

func (p *linkPool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.links)
}

// pickLocked performs smooth weighted round-robin (used by tests & fallback).
func (p *linkPool) pickLocked(exclude map[byte]bool) *bondLink {
	var chosen *bondLink
	totalWeight := 0
	for _, id := range p.order {
		if exclude != nil && exclude[id] {
			continue
		}
		l := p.links[id]
		w := l.weight
		if w < 1 {
			w = 1
		}
		l.currentWeight += w
		totalWeight += w
		if chosen == nil || l.currentWeight > chosen.currentWeight {
			chosen = l
		}
	}
	if chosen != nil {
		chosen.currentWeight -= totalWeight
	}
	return chosen
}

func (p *linkPool) rebalanceLocked(now time.Time) {
	if now.Sub(p.lastRebalance) < 500*time.Millisecond {
		return
	}
	p.lastRebalance = now
	if len(p.links) == 0 {
		return
	}
	var totalBps float64
	for _, id := range p.order {
		l := p.links[id]
		dt := now.Sub(l.lastSample).Seconds()
		if dt < 0.05 {
			continue
		}
		curTx := l.sentBytes.Load()
		curRx := l.receivedBytes.Load()
		l.txBps = float64(curTx-l.prevSentBytes) / dt
		l.rxBps = float64(curRx-l.prevRecvBytes) / dt
		l.prevSentBytes = curTx
		l.prevRecvBytes = curRx
		l.lastSample = now
		totalBps += l.txBps + l.rxBps

		if l.conn != nil {
			st := l.conn.ConnectionStats()
			dSent := diffU64(st.PacketsSent, l.prevQSent)
			dLost := diffU64(st.PacketsLost, l.prevQLost)
			l.prevQSent = st.PacketsSent
			l.prevQLost = st.PacketsLost

			if dSent >= 5 {
				lossRate := float64(dLost) / float64(dSent)
				l.lossPct = l.lossPct*0.5 + lossRate*100.0*0.5
			} else {
				l.lossPct *= 0.7
			}

			rttMs := float64(st.SmoothedRTT) / float64(time.Millisecond)
			if rttMs < 5.0 {
				rttMs = 5.0
			}
			// Penalize lossy links so clean, low-RTT links fill first and overflow spills cleanly.
			l.effRTTMs = rttMs * (1.0 + l.lossPct/15.0)
		}
	}
	for _, id := range p.order {
		l := p.links[id]
		if totalBps > 10_000 {
			share := ((l.txBps + l.rxBps) / totalBps) * 100.0
			w := int(share + 0.5)
			if w < 1 {
				w = 1
			}
			l.weight = w
		} else {
			l.weight = 1
		}
	}
	p.rebuildOrderLocked()
}

func ipv4FlowKey(packet []byte) (uint32, bool) {
	if len(packet) < 24 || packet[0]>>4 != 4 {
		return 0, false
	}
	ihl := int(packet[0]&0x0f) * 4
	proto := packet[9]
	if (proto != 6 && proto != 17) || ihl < 20 || len(packet) < ihl+4 {
		return 0, false
	}
	// FNV-1a over (proto, srcIP, dstIP, srcPort, dstPort)
	var h uint32 = 2166136261
	mix := func(b byte) {
		h ^= uint32(b)
		h *= 16777619
	}
	mix(proto)
	for i := 12; i < 20; i++ {
		mix(packet[i])
	}
	for i := ihl; i < ihl+4; i++ {
		mix(packet[i])
	}
	return h, true
}

// send combines flow-sticky distribution (for zero intra-flow reordering across
// multi-stream traffic) with MinRTT + QUIC backpressure spillover (so single
// streams still bond across all links and slow links never block fast ones).
func (p *linkPool) send(packet []byte) error {
	p.mu.Lock()
	if len(p.order) == 0 {
		p.mu.Unlock()
		return errNoLinks
	}
	p.rebalanceLocked(time.Now())

	var chosen *bondLink

	// Pass 0: for TCP/UDP flows, pin each new flow round-robin to an active link so
	// concurrent streams spread evenly across ALL links without intra-flow reordering,
	// spilling over only when that link's send queue backs up (>= linkSpillThresh).
	if fk, ok := ipv4FlowKey(packet); ok {
		id, pinned := p.flowPin[fk]
		if !pinned || p.links[id] == nil {
			if len(p.flowPin) > 2048 {
				clear(p.flowPin)
			}
			id = p.order[p.flowCursor%uint64(len(p.order))]
			p.flowCursor++
			p.flowPin[fk] = id
		}
		if l := p.links[id]; l != nil && l.txCh != nil && len(l.txCh) < linkSpillThresh {
			chosen = l
		}
	}

	// Pass 1: lowest-effective-RTT link with < linkSpillThresh queued.
	if chosen == nil {
		for _, id := range p.order {
			l := p.links[id]
			if l.txCh != nil && len(l.txCh) < linkSpillThresh {
				chosen = l
				break
			}
		}
	}
	// Pass 2: all links busy — pick least-queued link below cap so every link fills to 100% cwnd.
	if chosen == nil {
		bestLen := linkSendQueueLen
		for _, id := range p.order {
			l := p.links[id]
			if l.txCh != nil && len(l.txCh) < bestLen {
				bestLen = len(l.txCh)
				chosen = l
			}
		}
	}
	if chosen == nil {
		// All links are currently saturated at their QUIC congestion windows;
		// tail-drop cleanly without advancing seq or blocking the TUN loop.
		p.mu.Unlock()
		return nil
	}
	seq := p.seq
	p.seq++
	frame := encodeFrame(seq, packet)
	select {
	case chosen.txCh <- frame:
		p.mu.Unlock()
		return nil
	default:
		// Race with sender: rollback seq only if nothing else incremented it.
		if p.seq == seq+1 {
			p.seq = seq
		}
		p.mu.Unlock()
		return nil
	}
}

func (p *linkPool) snapshot() []linkSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rebalanceLocked(time.Now())
	totalWeight := 0
	for _, id := range p.order {
		w := p.links[id].weight
		if w < 1 {
			w = 1
		}
		totalWeight += w
	}
	out := make([]linkSnapshot, 0, len(p.order))
	for _, id := range p.order {
		l := p.links[id]
		var rttMs, minRttMs float64
		if l.conn != nil {
			st := l.conn.ConnectionStats()
			rttMs = float64(st.SmoothedRTT) / float64(time.Millisecond)
			minRttMs = float64(st.MinRTT) / float64(time.Millisecond)
		}
		wpct := 0.0
		if totalWeight > 0 {
			wpct = float64(l.weight) * 100.0 / float64(totalWeight)
		}
		out = append(out, linkSnapshot{
			ID:          l.id,
			Name:        l.name,
			Target:      l.target,
			Connected:   true,
			Weight:      l.weight,
			WeightPct:   wpct,
			RTTMs:       rttMs,
			MinRTTMs:    minRttMs,
			LossPct:     l.lossPct,
			TxMbps:      (l.txBps * 8.0) / 1_000_000.0,
			RxMbps:      (l.rxBps * 8.0) / 1_000_000.0,
			SentPackets: l.sent.Load(),
			RecvPackets: l.received.Load(),
			SentBytes:   l.sentBytes.Load(),
			RecvBytes:   l.receivedBytes.Load(),
			SendErrors:  l.sendErrors.Load(),
		})
	}
	return out
}

func diffU64(cur, prev uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	return 0
}

func receiveDatagrams(conn *quic.Conn, r *reassembler, received, receivedBytes *atomic.Uint64) error {
	for {
		frame, err := conn.ReceiveDatagram(conn.Context())
		if err != nil {
			return err
		}
		if len(frame) < 9 || len(frame)-8 > maxPacketSize {
			continue
		}
		received.Add(1)
		if receivedBytes != nil {
			receivedBytes.Add(uint64(len(frame) - 8))
		}
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
