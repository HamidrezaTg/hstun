package seal

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"
)

type countConn struct {
	net.Conn
	n     int
	sizes []int
}

func (c *countConn) Write(p []byte) (int, error) {
	c.n += len(p)
	c.sizes = append(c.sizes, len(p))
	return c.Conn.Write(p)
}

func padPair(t *testing.T) (a, b Conn, ca *countConn) {
	t.Helper()
	x, y := net.Pipe()
	t.Cleanup(func() { x.Close(); y.Close() })
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	ca = &countConn{Conn: x}
	var err error
	if a, err = Wrap(ca, key, true); err != nil {
		t.Fatal(err)
	}
	if b, err = Wrap(y, key, false); err != nil {
		t.Fatal(err)
	}
	return a, b, ca
}

func sendAll(t *testing.T, a, b Conn, msgs [][]byte) {
	t.Helper()
	go func() {
		for _, m := range msgs {
			if _, err := a.Write(m); err != nil {
				return
			}
		}
	}()
	var want []byte
	for _, m := range msgs {
		want = append(want, m...)
	}
	got := make([]byte, len(want))
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("data changed by padding")
	}
}

func smallMessages(n int) [][]byte {
	var out [][]byte
	for i := 0; i < n; i++ {
		m := make([]byte, 20+i%50)
		_, _ = rand.Read(m)
		out = append(out, m)
	}
	return out
}

func TestNoPaddingUntilThePeerAnnounced(t *testing.T) {
	a, b, ca := padPair(t)
	a.SetPadding(PadStrong) // our policy is on, but the peer has not said it reads padding
	msgs := smallMessages(40)
	sendAll(t, a, b, msgs)
	want := 0
	for _, m := range msgs {
		want += hdrLen + len(m) + tagLen
	}
	if ca.n != want {
		t.Fatalf("wrote %d bytes, want exactly %d (no padding without the peer's announcement)", ca.n, want)
	}
	if err := a.WritePad(100); err != nil || ca.n != want {
		t.Fatal("WritePad wrote something before the peer announced padding")
	}
}

func TestPaddingIsInvisibleToTheReaderAndVariesTheSizes(t *testing.T) {
	for _, mode := range []string{PadLight, PadStrong} {
		a, b, ca := padPair(t)
		a.SetPadding(mode)
		a.SetPeerPadding(true)
		msgs := smallMessages(60)
		sendAll(t, a, b, msgs)
		plain := 0
		for _, m := range msgs {
			plain += hdrLen + len(m) + tagLen
		}
		if ca.n <= plain {
			t.Errorf("%s: no padding was added (%d bytes vs %d)", mode, ca.n, plain)
		}
		seen := map[int]bool{}
		for _, s := range ca.sizes {
			seen[s] = true
		}
		if len(seen) < 15 {
			t.Errorf("%s: only %d distinct record sizes in %d records", mode, len(seen), len(ca.sizes))
		}
		if mode == PadStrong {
			for i := 0; i < 16; i++ {
				if ca.sizes[i] < 900 || ca.sizes[i] > 1400+2*hdrLen+2*tagLen {
					t.Errorf("strong: record %d is %d bytes, want 900-1400", i, ca.sizes[i])
				}
			}
		}
	}
}

func TestBulkFramesAreNotPadded(t *testing.T) {
	a, b, ca := padPair(t)
	a.SetPadding(PadStrong)
	a.SetPeerPadding(true)
	big := bytes.Repeat([]byte{7}, MaxPlain)
	sendAll(t, a, b, [][]byte{big, big, big})
	if want := 3 * (hdrLen + MaxPlain + tagLen); ca.n != want {
		t.Fatalf("full frames were padded: %d vs %d", ca.n, want)
	}
}

func TestStandalonePadFrame(t *testing.T) {
	a, b, _ := padPair(t)
	a.SetPeerPadding(true)
	go func() {
		_ = a.WritePad(700)
		_, _ = a.Write([]byte("after"))
	}()
	got := make([]byte, 5)
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(b, got); err != nil || string(got) != "after" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A padding frame must be authenticated like any other: a forged one is an error.
func TestForgedPadFrameFails(t *testing.T) {
	x, y := net.Pipe()
	defer x.Close()
	defer y.Close()
	key := make([]byte, 32)
	b, _ := Wrap(y, key, false)
	go func() {
		frame := make([]byte, 2+40)
		frame[0], frame[1] = 0x80, 40 // padding frame, 40 bytes of "ciphertext"
		_, _ = x.Write(frame)
	}()
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := b.Read(make([]byte, 1)); err != ErrAuth {
		t.Fatalf("got %v, want ErrAuth", err)
	}
}
