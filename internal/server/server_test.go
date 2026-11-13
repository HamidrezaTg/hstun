package server

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testRanges(t *testing.T) ranges {
	p := filepath.Join(t.TempDir(), "cf.txt")
	if err := os.WriteFile(p, []byte("# cloudflare\n173.245.48.0/20\n2400:cb00::/32  # v6\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := loadRanges(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestClientIPTrustsHeaderOnlyFromCloudflare(t *testing.T) {
	s := &Server{cf: testRanges(t)}
	mk := func(remote, hdr string) *http.Request {
		r := &http.Request{RemoteAddr: remote, Header: http.Header{}}
		if hdr != "" {
			r.Header.Set("CF-Connecting-IP", hdr)
		}
		return r
	}
	if got := s.clientIPOf(mk("173.245.48.9:5555", "203.0.113.7")); got != "203.0.113.7" {
		t.Errorf("header from a CF peer ignored: %s", got)
	}
	if got := s.clientIPOf(mk("198.51.100.4:5555", "203.0.113.7")); got != "198.51.100.4" {
		t.Errorf("spoofed header trusted from a non-CF peer: %s", got)
	}
	if got := s.clientIPOf(mk("173.245.48.9:5555", "not-an-ip")); got != "173.245.48.9" {
		t.Errorf("bad header not ignored: %s", got)
	}
	plain := &Server{}
	if got := plain.clientIPOf(mk("198.51.100.4:1", "203.0.113.7")); got != "198.51.100.4" {
		t.Errorf("header trusted with trust_proxy off: %s", got)
	}
}

func TestClientIPLoopbackProxy(t *testing.T) {
	s := &Server{cf: loopbackRanges(), proxyHeader: "X-Real-IP"}
	mk := func(remote, name, hdr string) *http.Request {
		r := &http.Request{RemoteAddr: remote, Header: http.Header{}}
		if hdr != "" {
			r.Header.Set(name, hdr)
		}
		return r
	}
	if got := s.clientIPOf(mk("127.0.0.1:4000", "X-Real-IP", "203.0.113.7")); got != "203.0.113.7" {
		t.Errorf("X-Real-IP from the local proxy ignored: %s", got)
	}
	if got := s.clientIPOf(mk("[::1]:4000", "X-Real-IP", "2001:db8::5")); got != "2001:db8::5" {
		t.Errorf("X-Real-IP from an IPv6 loopback peer ignored: %s", got)
	}
	if got := s.clientIPOf(mk("198.51.100.4:4000", "X-Real-IP", "203.0.113.7")); got != "198.51.100.4" {
		t.Errorf("spoofed X-Real-IP trusted from a non-loopback peer: %s", got)
	}
	if got := s.clientIPOf(mk("127.0.0.1:4000", "CF-Connecting-IP", "203.0.113.7")); got != "127.0.0.1" {
		t.Errorf("wrong header trusted in loopback mode: %s", got)
	}
	if got := s.clientIPOf(mk("127.0.0.1:4000", "X-Real-IP", "garbage")); got != "127.0.0.1" {
		t.Errorf("bad header not ignored: %s", got)
	}
}

func TestClientIPCloudflaredFromLoopback(t *testing.T) {
	s := &Server{cf: loopbackRanges(), proxyHeader: "CF-Connecting-IP"}
	mk := func(remote, name, hdr string) *http.Request {
		r := &http.Request{RemoteAddr: remote, Header: http.Header{}}
		r.Header.Set(name, hdr)
		return r
	}
	if got := s.clientIPOf(mk("127.0.0.1:4000", "CF-Connecting-IP", "203.0.113.9")); got != "203.0.113.9" {
		t.Errorf("CF-Connecting-IP from the local cloudflared ignored: %s", got)
	}
	if got := s.clientIPOf(mk("198.51.100.4:4000", "CF-Connecting-IP", "203.0.113.9")); got != "198.51.100.4" {
		t.Errorf("spoofed header trusted from a non-loopback peer: %s", got)
	}
	if got := s.clientIPOf(mk("127.0.0.1:4000", "X-Real-IP", "203.0.113.9")); got != "127.0.0.1" {
		t.Errorf("the nginx header is trusted in cloudflared mode: %s", got)
	}
}

func TestLoadRangesRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.txt")
	_ = os.WriteFile(p, []byte("not-a-cidr\n"), 0o600)
	if _, err := loadRanges(p); err == nil {
		t.Fatal("garbage accepted")
	}
	e := filepath.Join(t.TempDir(), "empty.txt")
	_ = os.WriteFile(e, []byte("# nothing\n"), 0o600)
	if _, err := loadRanges(e); err == nil {
		t.Fatal("empty file accepted")
	}
}

func TestFailLimiter(t *testing.T) {
	f := newFailLimiter(3)
	for i := 0; i < 3; i++ {
		if !f.allow("1.2.3.4") {
			t.Fatalf("blocked too early at %d", i)
		}
		f.fail("1.2.3.4")
	}
	if f.allow("1.2.3.4") {
		t.Fatal("not blocked after the limit")
	}
	if !f.allow("5.6.7.8") {
		t.Fatal("other IP affected")
	}
}

func TestLimiterGroupsIPv6By64(t *testing.T) {
	f := newFailLimiter(3)
	for i := 0; i < 3; i++ {
		f.fail("2001:db8:1:2::" + string(rune('a'+i)))
	}
	if f.allow("2001:db8:1:2:ffff::1") {
		t.Fatal("a second address in the same /64 was not counted")
	}
	if !f.allow("2001:db8:1:3::1") {
		t.Fatal("a different /64 was affected")
	}
}

func TestLimiterNeverDropsLiveBuckets(t *testing.T) {
	f := newFailLimiter(2)
	f.fail("203.0.113.1")
	f.fail("203.0.113.1")
	for i := 0; i < 20000; i++ { // flood of distinct sources
		f.fail(net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)).String())
	}
	if f.allow("203.0.113.1") {
		t.Fatal("a flood of other IPs reset an attacker's own counter")
	}
}

func TestGuardListener(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	g, err := newGuardListener(raw, nil, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 8)
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
		c, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1, c2, c3 := dial(), dial(), dial()
	defer c1.Close()
	defer c2.Close()
	defer c3.Close()
	a1, a2 := <-accepted, <-accepted
	// the third connection from the same IP is closed by the guard
	_ = c3.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c3.Read(make([]byte, 1)); err == nil {
		t.Fatal("third connection from one IP was kept open")
	}
	// releasing one slot lets a new connection in
	a1.Close()
	c4 := dial()
	defer c4.Close()
	select {
	case a := <-accepted:
		a.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("slot not released after close")
	}
	a2.Close()
}

func TestGuardAllowList(t *testing.T) {
	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	defer raw.Close()
	g, err := newGuardListener(raw, []string{"198.51.100.0/24"}, 0, nil)
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
		t.Fatal("peer outside allow_from was kept open")
	}
}
