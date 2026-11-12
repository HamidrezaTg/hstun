package mux

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"hstun/internal/netx"
)

// A client that half-closes after sending must still receive everything the
// target echoes back: Pipe may not turn the half-close into a full close.
func TestPipeHalfCloseYamux(t *testing.T) {
	a, b := net.Pipe()
	cli, err := yamux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := yamux.Server(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	defer srv.Close()

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()

	go func() {
		st, err := srv.AcceptStream()
		if err != nil {
			return
		}
		tc, err := net.Dial("tcp", echo.Addr().String())
		if err != nil {
			_ = st.Close()
			return
		}
		netx.Pipe(Stream{Stream: st}, tc, 10*time.Second)
	}()

	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("hstun-halfclose-"), 1<<16) // 1 MiB
	go func() {
		_, _ = st.Write(payload)
		_ = st.Close() // half-close: FIN, reads stay open
	}()
	_ = st.SetReadDeadline(time.Now().Add(15 * time.Second))
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}
