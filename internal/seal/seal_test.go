package seal

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func key() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}

// tcpPair returns two connected loopback TCP conns.
func tcpPair(t *testing.T) (a, b net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); ch <- c }()
	a, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b = <-ch
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestRoundTripBothDirections(t *testing.T) {
	a, b := tcpPair(t)
	k := key()
	sa, _ := Wrap(a, k, true)
	sb, _ := Wrap(b, k, false)
	want := make([]byte, 3<<20+7)
	_, _ = rand.Read(want)
	var wg sync.WaitGroup
	wg.Add(2)
	send := func(c net.Conn) { defer wg.Done(); _, _ = c.Write(want) }
	go send(sa)
	go send(sb)
	for _, c := range []net.Conn{sa, sb} {
		got := make([]byte, len(want))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("data mismatch")
		}
	}
	wg.Wait()
}

func TestCiphertextHidesPlaintext(t *testing.T) {
	a, b := tcpPair(t)
	k := key()
	sa, _ := Wrap(a, k, true)
	secret := bytes.Repeat([]byte("TOP-SECRET-PAYLOAD "), 100)
	go func() { _, _ = sa.Write(secret) }()
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	raw := make([]byte, len(secret)+64)
	n, _ := io.ReadAtLeast(b, raw, 100)
	if bytes.Contains(raw[:n], []byte("TOP-SECRET")) {
		t.Fatal("plaintext visible on the wire")
	}
}

// mitm sits between two conns and lets the test rewrite the bytes of the
// a->b direction.
func mitm(t *testing.T, mutate func([]byte) [][]byte) (client, server net.Conn) {
	t.Helper()
	c1, m1 := tcpPair(t)
	m2, s2 := tcpPair(t)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := m1.Read(buf)
			if n > 0 {
				for _, out := range mutate(append([]byte(nil), buf[:n]...)) {
					if _, werr := m2.Write(out); werr != nil {
						return
					}
				}
			}
			if err != nil {
				m2.Close()
				return
			}
		}
	}()
	go func() { _, _ = io.Copy(m1, m2) }()
	return c1, s2
}

func readErr(t *testing.T, c net.Conn) error {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := io.ReadAll(c)
	return err
}

func TestTamperDetected(t *testing.T) {
	k := key()
	c, s := mitm(t, func(b []byte) [][]byte {
		b[len(b)-1] ^= 1 // flip one bit in the tag
		return [][]byte{b}
	})
	sc, _ := Wrap(c, k, true)
	ss, _ := Wrap(s, k, false)
	go func() { _, _ = sc.Write([]byte("hello hello hello")) }()
	if err := readErr(t, ss); err != ErrAuth {
		t.Fatalf("tampered frame accepted or wrong error: %v", err)
	}
}

func TestReplayAndReorderDetected(t *testing.T) {
	k := key()
	var mu sync.Mutex
	var first []byte
	c, s := mitm(t, func(b []byte) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		if first == nil {
			first = b
			return [][]byte{b, b} // deliver the first frame twice
		}
		return [][]byte{b}
	})
	sc, _ := Wrap(c, k, true)
	ss, _ := Wrap(s, k, false)
	go func() { _, _ = sc.Write([]byte("frame-one-frame-one")) }()
	buf := make([]byte, 19)
	_ = ss.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(ss, buf); err != nil {
		t.Fatalf("first copy should be accepted: %v", err)
	}
	if _, err := ss.Read(buf); err != ErrAuth {
		t.Fatalf("replayed frame accepted: %v", err)
	}
}

func TestReflectionAndWrongKeyDetected(t *testing.T) {
	// A frame sealed by the dialer must not open on the dialer's own read side
	// (reflection), nor with another key.
	a, b := tcpPair(t)
	k := key()
	sa, _ := Wrap(a, k, true)
	go func() { _, _ = sa.Write([]byte("hello hello hello")) }()
	raw := make([]byte, 100)
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := io.ReadAtLeast(b, raw, 20)
	frame := raw[:n]

	for name, tc := range map[string]struct {
		key    []byte
		dialer bool
	}{
		"reflected to dialer": {k, true},
		"wrong key":           {key(), false},
	} {
		x, y := tcpPair(t)
		sx, _ := Wrap(x, tc.key, tc.dialer)
		go func() { _, _ = y.Write(frame) }()
		_ = sx.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := sx.Read(make([]byte, 64)); err != ErrAuth {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

func TestBadLengthRejected(t *testing.T) {
	for _, hdr := range [][]byte{{0, 0}, {0, 5}, {0, 16}, {0xff, 0xff}} {
		a, b := tcpPair(t)
		s, _ := Wrap(a, key(), false)
		go func() { _, _ = b.Write(append(hdr, make([]byte, 64)...)) }()
		_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := s.Read(make([]byte, 8)); err != ErrFrame {
			t.Errorf("hdr %v: %v", hdr, err)
		}
	}
}

func FuzzRead(f *testing.F) {
	f.Add([]byte{0, 20, 1, 2, 3})
	f.Add(bytes.Repeat([]byte{0xff}, 64))
	k := key()
	f.Fuzz(func(t *testing.T, data []byte) {
		a, b := net.Pipe()
		s, _ := Wrap(a, k, false)
		go func() { _, _ = b.Write(data); b.Close() }()
		_ = a.SetReadDeadline(time.Now().Add(time.Second))
		_, _ = io.Copy(io.Discard, s) // must not panic or hang
		a.Close()
	})
}
