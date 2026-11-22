package main

import (
	"sync"
	"time"
)

type reassembler struct {
	mu      sync.Mutex
	next    uint64
	pending map[uint64][]byte
	wait    time.Duration
	timer   *time.Timer
	deliver func([]byte) error
}

func newReassembler(wait time.Duration, deliver func([]byte) error) *reassembler {
	return &reassembler{pending: make(map[uint64][]byte), wait: wait, deliver: deliver}
}

func (r *reassembler) push(seq uint64, packet []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if seq < r.next {
		return
	}
	if seq == r.next {
		r.deliver(packet)
		r.next++
		r.flush()
	} else if _, exists := r.pending[seq]; !exists {
		r.pending[seq] = packet
	}
	if len(r.pending) == 0 {
		if r.timer != nil {
			r.timer.Stop()
			r.timer = nil
		}
		return
	}
	if len(r.pending) > 256 {
		r.skipGap()
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
		delete(r.pending, r.next)
		r.next++
	}
}

func (r *reassembler) skipGap() {
	lowest := r.next
	for seq := range r.pending {
		if lowest == r.next || seq < lowest {
			lowest = seq
		}
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
	if len(r.pending) == 0 {
		return
	}
	r.skipGap()
	if len(r.pending) > 0 {
		r.resetTimer()
	}
}
