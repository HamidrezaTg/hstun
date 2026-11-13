package server

import (
	"net"
	"testing"
	"time"
)

func needIPv6(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback on this machine")
	}
	_ = l.Close()
}

func TestLimiterKeyGroupsIPv6By64(t *testing.T) {
	a := limiterKey("2001:db8:1:2:aaaa::1")
	b := limiterKey("2001:db8:1:2:ffff:ffff:ffff:ffff")
	c := limiterKey("2001:db8:1:3::1")
	if a != b {
		t.Fatalf("two addresses of one /64 must share a key: %s vs %s", a, b)
	}
	if a == c {
		t.Fatal("different /64 networks must not share a key")
	}
	if limiterKey("192.0.2.7") != "192.0.2.7" {
		t.Fatal("IPv4 keys stay per address")
	}
}

func TestGuardAllowListIPv6(t *testing.T) {
	needIPv6(t)
	raw, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	g, err := newGuardListener(raw, []string{"2001:db8::/32"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = g.Accept() }()
	c, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("an IPv6 peer outside allow_from was kept open")
	}
}

// A dual-stack listener ("[::]:port") sees IPv4 peers as IPv4-mapped addresses:
// the IPv4 allow-list and the per-IP cap must still match them.
func TestGuardDualStackMatchesIPv4Peers(t *testing.T) {
	needIPv6(t)
	raw, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		t.Skip("dual-stack listener unavailable")
	}
	defer raw.Close()
	port := raw.Addr().(*net.TCPAddr).Port
	g, err := newGuardListener(raw, []string{"127.0.0.0/8"}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := g.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1 := dial()
	defer c1.Close()
	a1 := <-accepted // the IPv4 CIDR matched the IPv4-mapped peer
	c2 := dial()
	defer c2.Close()
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("the per-IP cap did not apply to an IPv4 peer on a dual-stack listener")
	}
	a1.Close()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
