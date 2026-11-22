package main

import (
	"sync"
	"sync/atomic"
	"time"
)

const maxPendingPackets = 1024

type reassemblerStats struct {
	Delivered   uint64 `json:"delivered"`
	Reordered   uint64 `json:"reordered"`
	GapsSkipped uint64 `json:"gaps_skipped"`
	Duplicates  uint64 `json:"duplicates"`
	Pending     int    `json:"pending"`
}

type reassembler struct {
	mu          sync.Mutex
	next        uint64
	started     bool
	closed      bool
	pending     map[uint64][]byte
	wait        time.Duration
	timer       *time.Timer
	deliver     func([]byte) error
	delivered   atomic.Uint64
	reordered   atomic.Uint64
	gapsSkipped atomic.Uint64
	duplicates  atomic.Uint64
}

func newReassembler(wait time.Duration, deliver func([]byte) error) *reassembler {
	return &reassembler{pending: make(map[uint64][]byte), wait: wait, deliver: deliver}
}

func (r *reassembler) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	clear(r.pending)
}

func (r *reassembler) stats() reassemblerStats {
	r.mu.Lock()
	pending := len(r.pending)
	r.mu.Unlock()
	return reassemblerStats{
		Delivered:   r.delivered.Load(),
		Reordered:   r.reordered.Load(),
		GapsSkipped: r.gapsSkipped.Load(),
		Duplicates:  r.duplicates.Load(),
		Pending:     pending,
	}
}

func (r *reassembler) push(seq uint64, packet []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if !r.started {
		r.started = true
		if seq > 0 && seq > maxPendingPackets {
			r.next = seq
		}
	}
	if seq < r.next {
		r.duplicates.Add(1)
		return
	}
	if seq == r.next {
		r.deliver(packet)
		r.delivered.Add(1)
		r.next++
		r.flush()
	} else if _, exists := r.pending[seq]; !exists {
		r.pending[seq] = packet
		r.reordered.Add(1)
	} else {
		r.duplicates.Add(1)
	}
	if len(r.pending) == 0 {
		if r.timer != nil {
			r.timer.Stop()
			r.timer = nil
		}
		return
	}
	if len(r.pending) > maxPendingPackets {
		r.skipGap()
	}
	if len(r.pending) == 0 {
		if r.timer != nil {
			r.timer.Stop()
			r.timer = nil
		}
		return
	}
	if r.timer == nil {
		r.timer = time.AfterFunc(r.wait, r.expire)
	}
}

func (r *reassembler) flush() {
	for {
		packet, ok := r.pending[r.next]
		if !ok {
			return
		}
		r.deliver(packet)
		r.delivered.Add(1)
		delete(r.pending, r.next)
		r.next++
	}
}

func (r *reassembler) skipGap() {
	lowest := r.next
	found := false
	for seq := range r.pending {
		if !found || seq < lowest {
			lowest = seq
			found = true
		}
	}
	if !found {
		return
	}
	if lowest > r.next {
		r.gapsSkipped.Add(lowest - r.next)
	}
	r.next = lowest
	r.flush()
}

func (r *reassembler) resetTimer() {
	if r.timer == nil {
		r.timer = time.AfterFunc(r.wait, r.expire)
	} else {
		r.timer.Reset(r.wait)
	}
}

func (r *reassembler) expire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timer = nil
	if r.closed || len(r.pending) == 0 {
		return
	}
	r.skipGap()
	if len(r.pending) > 0 {
		r.resetTimer()
	}
}
