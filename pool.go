package main

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

var errNoLinks = errors.New("no active links")

type linkSnapshot struct {
	ID            byte    `json:"id"`
	Name          string  `json:"name"`
	Target        string  `json:"target,omitempty"`
	Connected     bool    `json:"connected"`
	Weight        int     `json:"weight"`
	WeightPct     float64 `json:"weight_pct"`
	RTTMs         float64 `json:"rtt_ms"`
	MinRTTMs      float64 `json:"min_rtt_ms"`
	LossPct       float64 `json:"loss_pct"`
	TxMbps        float64 `json:"tx_mbps"`
	RxMbps        float64 `json:"rx_mbps"`
	SentPackets   uint64  `json:"sent_packets"`
	RecvPackets   uint64  `json:"recv_packets"`
	SentBytes     uint64  `json:"sent_bytes"`
	RecvBytes     uint64  `json:"recv_bytes"`
	SendErrors    uint64  `json:"send_errors"`
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

	// Protected by linkPool.mu
	score         float64
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
}

func (l *bondLink) close(code quic.ApplicationErrorCode, reason string) {
	if l == nil {
		return
	}
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
	lastRebalance time.Time
}

func newLinkPool() *linkPool {
	return &linkPool{
		links:         make(map[byte]*bondLink),
		lastRebalance: time.Now(),
	}
}

func (p *linkPool) add(link *bondLink) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.links[link.id]; exists {
		return errors.New("link ID already connected")
	}
	link.score = 4.0
	link.weight = 2
	link.currentWeight = 0
	link.lastSample = time.Now()
	if link.conn != nil {
		st := link.conn.ConnectionStats()
		link.prevQSent = st.PacketsSent
		link.prevQLost = st.PacketsLost
	}
	p.links[link.id] = link
	p.rebuildOrderLocked()
	return nil
}

func (p *linkPool) remove(id byte, conn *quic.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.links[id] != nil && p.links[id].conn == conn {
		delete(p.links, id)
		p.rebuildOrderLocked()
	}
}

func (p *linkPool) rebuildOrderLocked() {
	p.order = p.order[:0]
	for id := range p.links {
		p.order = append(p.order, id)
	}
	sort.Slice(p.order, func(i, j int) bool { return p.order[i] < p.order[j] })
}

func (p *linkPool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.links)
}

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
	minScore := math.MaxFloat64
	for _, id := range p.order {
		l := p.links[id]
		dt := now.Sub(l.lastSample).Seconds()
		if dt < 0.05 {
			if l.score < minScore {
				minScore = l.score
			}
			continue
		}
		curTx := l.sentBytes.Load()
		curRx := l.receivedBytes.Load()
		l.txBps = float64(curTx-l.prevSentBytes) / dt
		l.rxBps = float64(curRx-l.prevRecvBytes) / dt
		l.prevSentBytes = curTx
		l.prevRecvBytes = curRx
		l.lastSample = now

		if l.conn != nil {
			st := l.conn.ConnectionStats()
			dSent := diffU64(st.PacketsSent, l.prevQSent)
			dLost := diffU64(st.PacketsLost, l.prevQLost)
			l.prevQSent = st.PacketsSent
			l.prevQLost = st.PacketsLost

			if dSent >= 5 {
				lossRate := float64(dLost) / float64(dSent)
				l.lossPct = l.lossPct*0.5 + lossRate*100.0*0.5
				rttInflated := st.MinRTT > 0 && st.LatestRTT > st.MinRTT*2 && (st.LatestRTT-st.MinRTT) > 40*time.Millisecond
				switch {
				case lossRate > 0.03:
					cut := 1.0 - math.Min(lossRate*3.0, 0.55)
					l.score *= cut
				case rttInflated:
					l.score *= 0.85
				default:
					l.score += 1.5
				}
				if l.score < 1.0 {
					l.score = 1.0
				} else if l.score > 32.0 {
					l.score = 32.0
				}
			} else {
				l.lossPct *= 0.7
			}
		}
		if l.score < minScore {
			minScore = l.score
		}
	}
	if minScore < 1.0 || minScore == math.MaxFloat64 {
		minScore = 1.0
	}
	for _, id := range p.order {
		l := p.links[id]
		w := int(math.Round((l.score / minScore) * 2.0))
		if w < 1 {
			w = 1
		} else if w > 16 {
			w = 16
		}
		l.weight = w
	}
}

func (p *linkPool) send(packet []byte) error {
	p.mu.Lock()
	if len(p.order) == 0 {
		p.mu.Unlock()
		return errNoLinks
	}
	p.rebalanceLocked(time.Now())
	link := p.pickLocked(nil)
	seq := p.seq
	p.seq++
	nLinks := len(p.order)
	p.mu.Unlock()

	frame := encodeFrame(seq, packet)
	var tried map[byte]bool
	var lastErr error
	for attempt := 0; attempt < nLinks && link != nil; attempt++ {
		err := link.conn.SendDatagram(frame)
		if err == nil {
			link.sent.Add(1)
			link.sentBytes.Add(uint64(len(packet)))
			return nil
		}
		lastErr = err
		link.sendErrors.Add(1)
		dead := link.conn.Context().Err() != nil
		if dead {
			p.remove(link.id, link.conn)
		}
		if tried == nil {
			tried = make(map[byte]bool, nLinks)
		}
		tried[link.id] = true

		p.mu.Lock()
		// Softly reduce score on send error so scheduler shifts away immediately.
		if cur := p.links[link.id]; cur != nil && cur.score > 1.0 {
			cur.score = math.Max(1.0, cur.score*0.7)
		}
		link = p.pickLocked(tried)
		p.mu.Unlock()
	}
	if lastErr == nil {
		return errNoLinks
	}
	return lastErr
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
