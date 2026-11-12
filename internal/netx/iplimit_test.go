package netx

import (
	"net"
	"testing"
)

func TestIPLimiter(t *testing.T) {
	l := NewIPLimiter(2)
	var conns []net.Conn
	for i := 0; i < 3; i++ {
		c1, c2 := tcpPair(t)
		defer c1.Close()
		defer c2.Close()
		conns = append(conns, c2) // c2's RemoteAddr is 127.0.0.1 for every pair
	}
	r1, ok1 := l.Acquire(conns[0])
	_, ok2 := l.Acquire(conns[1])
	_, ok3 := l.Acquire(conns[2])
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("cap 2: got %v %v %v, want true true false", ok1, ok2, ok3)
	}
	r1()
	r1() // releasing twice must not free a second slot
	if _, ok := l.Acquire(conns[2]); !ok {
		t.Fatal("slot not freed after release")
	}
	if _, ok := l.Acquire(conns[2]); ok {
		t.Fatal("double release freed an extra slot")
	}
	off := NewIPLimiter(0)
	for i := 0; i < 100; i++ {
		if _, ok := off.Acquire(conns[0]); !ok {
			t.Fatal("limiter with max 0 must be off")
		}
	}
}

func TestIPKeyGroupsIPv6By64(t *testing.T) {
	a := IPKey(net.ParseIP("2001:db8:1:2::1"))
	b := IPKey(net.ParseIP("2001:db8:1:2:ffff::9"))
	c := IPKey(net.ParseIP("2001:db8:1:3::1"))
	if a != b || a == c {
		t.Fatalf("keys: %s %s %s", a, b, c)
	}
	if IPKey(net.ParseIP("192.0.2.5")) != "192.0.2.5" {
		t.Fatal("ipv4 key changed")
	}
}
