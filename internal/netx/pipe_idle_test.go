package netx

import (
	"io"
	"net"
	"testing"
	"time"
)

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); ch <- c }()
	c1, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c1, <-ch
}

// A silent upstream must not end the pipe while the downstream is busy: the
// idle timeout is about traffic in either direction.
func TestPipeIdleCountsBothDirections(t *testing.T) {
	user, userEnd := tcpPair(t)
	targetEnd, target := tcpPair(t)
	defer user.Close()
	defer target.Close()
	go Pipe(userEnd, targetEnd, 700*time.Millisecond)

	go io.Copy(io.Discard, user) // the user only reads
	sawEOF := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, target); close(sawEOF) }()

	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) { // downstream traffic well past the idle time
		if _, err := target.Write([]byte("data")); err != nil {
			t.Fatalf("write failed while downstream active: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
	}
	select {
	case <-sawEOF:
		t.Fatal("target saw EOF on its upstream while downstream was still active")
	default:
	}
}

// A pipe with no traffic at all in either direction still ends after idle.
func TestPipeTrulyIdleEnds(t *testing.T) {
	user, userEnd := tcpPair(t)
	targetEnd, target := tcpPair(t)
	defer user.Close()
	defer target.Close()
	done := make(chan struct{})
	go func() { Pipe(userEnd, targetEnd, 400*time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("idle pipe did not end")
	}
}
