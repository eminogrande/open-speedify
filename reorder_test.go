package main

import (
	"sync"
	"testing"
	"time"
)

type delivery struct {
	mu      sync.Mutex
	packets []string
}

func (d *delivery) add(p []byte) error {
	d.mu.Lock()
	d.packets = append(d.packets, string(p))
	d.mu.Unlock()
	return nil
}

func (d *delivery) get() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.packets...)
}

func TestReassemblerOrdersAndSkipsLostPackets(t *testing.T) {
	var d delivery
	r := newReassembler(20*time.Millisecond, d.add)
	r.push(1, []byte("one"))
	r.push(0, []byte("zero"))
	if got := d.get(); len(got) != 2 || got[0] != "zero" || got[1] != "one" {
		t.Fatalf("unexpected order: %v", got)
	}
	r.push(3, []byte("three"))
	r.push(4, []byte("four"))
	time.Sleep(5 * time.Millisecond)
	if got := len(d.get()); got != 2 {
		t.Fatalf("gap delivered too early: %d packets", got)
	}
	time.Sleep(30 * time.Millisecond)
	got := d.get()
	if len(got) != 4 || got[2] != "three" || got[3] != "four" {
		t.Fatalf("gap did not expire: %v", got)
	}
	r.push(4, []byte("duplicate"))
	if got := len(d.get()); got != 4 {
		t.Fatalf("duplicate delivered: %d packets", got)
	}
}
