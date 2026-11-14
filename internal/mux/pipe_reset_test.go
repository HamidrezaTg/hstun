package mux

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"hstun/internal/netx"
)

// resetRig is a yamux pair where the server side pipes every stream to a TCP
// target the test controls, like the exit does.
type resetRig struct {
	cli     *yamux.Session
	srv     *yamux.Session
	targets chan net.Conn // the target-side end of each piped stream
}

func newResetRig(t *testing.T) *resetRig {
	t.Helper()
	a, b := net.Pipe()
	cli, err := yamux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := yamux.Server(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	rig := &resetRig{cli: cli, srv: srv, targets: make(chan net.Conn, 4)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			rig.targets <- c
		}
	}()
	go func() {
		for {
			st, err := srv.AcceptStream()
			if err != nil {
				return
			}
			tc, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				_ = st.Close()
				continue
			}
			go netx.Pipe(Stream{Stream: st}, tc, 10*time.Second)
		}
	}()
	return rig
}

func (r *resetRig) open(t *testing.T) (*yamux.Stream, net.Conn) {
	t.Helper()
	st, err := r.cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte("hi")); err != nil { // creates the stream on the far side
		t.Fatal(err)
	}
	select {
	case tc := <-r.targets:
		buf := make([]byte, 2)
		if _, err := io.ReadFull(tc, buf); err != nil {
			t.Fatal(err)
		}
		return st, tc
	case <-time.After(5 * time.Second):
		t.Fatal("the target never got the connection")
		return nil, nil
	}
}

// The target resets mid-transfer: the stream's reader must see a reset, not an
// EOF that looks like a complete transfer.
func TestTargetResetReachesStreamReader(t *testing.T) {
	rig := newResetRig(t)
	st, tc := rig.open(t)
	_, _ = tc.Write([]byte("partial"))
	_ = tc.(*net.TCPConn).SetLinger(0)
	_ = tc.Close() // RST
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.ReadAll(st)
	if !errors.Is(err, yamux.ErrConnectionReset) {
		t.Fatalf("reader of a stream whose target was reset got %v, want ErrConnectionReset", err)
	}
}

// The stream is reset: the target's TCP connection must see RST, not FIN.
func TestStreamResetReachesTarget(t *testing.T) {
	rig := newResetRig(t)
	st, tc := rig.open(t)
	if err := st.Reset(); err != nil {
		t.Fatal(err)
	}
	_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.ReadAll(tc)
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("target of a reset stream got %v, want ECONNRESET", err)
	}
}

// A clean end still ends cleanly on both sides.
func TestCleanCloseStaysClean(t *testing.T) {
	rig := newResetRig(t)
	st, tc := rig.open(t)
	_, _ = tc.Write([]byte("whole"))
	_ = tc.Close()
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(st)
	if err != nil || string(got) != "whole" {
		t.Fatalf("clean close: got %q, %v", got, err)
	}
}

// The tunnel session dies in the middle of a transfer: the target must see a
// reset, and the stream's reader an error, not a clean end.
func TestSessionDeathIsAReset(t *testing.T) {
	rig := newResetRig(t)
	st, tc := rig.open(t)
	_ = rig.cli.Close()
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.ReadAll(st)
	if err == nil {
		t.Fatal("a stream cut by a dying session ended with a clean EOF")
	}
	_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(tc); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("target after the session died got %v, want ECONNRESET", err)
	}
}
