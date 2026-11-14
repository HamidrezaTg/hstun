// Package mux is the thin wrapper around hashicorp/yamux with the settings
// hstun uses. yamux gives per-stream receive windows, so one stalled stream
// cannot wedge the session or grow memory without bound.
package mux

import (
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"hstun/internal/netx"
	"hstun/internal/proto"
)

// ReadIdle is how long a session may receive nothing at all before it is
// declared dead. Pings every 25 s keep a healthy idle session well inside it,
// and it stays under Cloudflare's ~100 s idle timeout.
const ReadIdle = 80 * time.Second

// Option adjusts a session's yamux settings.
type Option func(*yamux.Config)

// SlowCarrier is for carriers that move data as request/response chunks (http,
// chunk). Their send buffer can legitimately need longer than the 10 s that
// suits a WebSocket to drain, and yamux would otherwise kill the whole session
// with "connection write timeout" while the carrier is only slow.
func SlowCarrier() Option {
	return func(c *yamux.Config) { c.ConnectionWriteTimeout = 45 * time.Second }
}

func config(keepAlive time.Duration, opts ...Option) *yamux.Config {
	c := yamux.DefaultConfig()
	c.AcceptBacklog = 64
	// The keepalive is ours (see keepalive): a fixed-period heartbeat is a
	// pattern a classifier can see, so the interval varies.
	c.EnableKeepAlive = false
	c.ConnectionWriteTimeout = 10 * time.Second
	c.MaxStreamWindowSize = 2 << 20
	c.StreamOpenTimeout = 10 * time.Second
	c.StreamCloseTimeout = 30 * time.Second
	c.LogOutput = io.Discard
	for _, o := range opts {
		o(c)
	}
	return c
}

func wrap(c net.Conn) net.Conn { return &netx.IdleConn{Conn: c, Idle: ReadIdle} }

// Streams may be opened by either end. The two ends of a session must use
// different yamux roles so stream ids never collide: the side that accepted
// the carrier uses Listener, the side that dialed it uses Dialer. Which of the
// two opens streams (entry) or serves them (exit) is independent of this.

// Listener wraps a carrier conn the local side accepted.
func Listener(c net.Conn, keepAlive time.Duration, opts ...Option) (*yamux.Session, error) {
	s, err := yamux.Client(wrap(c), config(keepAlive, opts...))
	if err == nil && keepAlive > 0 {
		go keepalive(s, c, keepAlive)
	}
	return s, err
}

// Dialer wraps a carrier conn the local side dialed.
func Dialer(c net.Conn, keepAlive time.Duration, opts ...Option) (*yamux.Session, error) {
	s, err := yamux.Server(wrap(c), config(keepAlive, opts...))
	if err == nil && keepAlive > 0 {
		go keepalive(s, c, keepAlive)
	}
	return s, err
}

// NextPing is the time to the next keepalive: the base interval varied by
// +-40 %, never above 55 s (the session is declared dead after 80 s of silence).
func NextPing(base time.Duration) time.Duration {
	d := time.Duration(float64(base) * (0.6 + 0.8*rand.Float64()))
	return min(d, 55*time.Second)
}

type padWriter interface{ WritePad(n int) error }

// keepalive pings the peer at jittered intervals until the session ends. A ping
// that gets no answer closes the session. When the connection can send padding
// frames, each ping travels with a random-size one, so even an idle tunnel has no
// constant record size.
func keepalive(s *yamux.Session, c net.Conn, base time.Duration) {
	pw, _ := c.(padWriter)
	for {
		t := time.NewTimer(NextPing(base))
		select {
		case <-s.CloseChan():
			t.Stop()
			return
		case <-t.C:
		}
		if pw != nil {
			_ = pw.WritePad(rand.IntN(600))
		}
		if _, err := s.Ping(); err != nil {
			_ = s.Close()
			return
		}
	}
}

// Announce tells the peer, on a stream of its own, what this end can read (padding).
// An older peer refuses the stream, which is how it says no. It never blocks.
func Announce(s *yamux.Session) {
	go func() {
		st, err := s.OpenStream()
		if err != nil {
			return
		}
		defer st.Close()
		_ = st.SetDeadline(time.Now().Add(10 * time.Second))
		if proto.WriteOpen(st, proto.OpenHeader{Kind: proto.KindCaps, Payload: proto.CapPadding}) == nil {
			_, _ = proto.ReadReply(st)
		}
	}()
}

// HandleCaps answers a KindCaps stream whose header was already read.
func HandleCaps(st net.Conn, hdr proto.OpenHeader, onCaps func(payload string)) {
	if onCaps != nil && strings.Contains(hdr.Payload, proto.CapPadding) {
		onCaps(hdr.Payload)
	}
	_ = proto.WriteReply(st, proto.ReplyOK)
	_ = st.Close()
}

// Refuse serves the streams of a peer that is not supposed to open any (an entry
// serves none): it answers the capability stream and closes the rest.
func Refuse(s *yamux.Session, onCaps func(payload string)) {
	for {
		st, err := s.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
			hdr, err := proto.ReadOpen(st)
			if err == nil && hdr.Kind == proto.KindCaps {
				HandleCaps(st, hdr, onCaps)
				return
			}
			_ = st.Close()
		}()
	}
}

// Stream is a yamux stream that can half-close. yamux's Close only sends a
// FIN, and the stream can still be read until the peer closes too, which is
// exactly CloseWrite. netx.Pipe uses it to end one direction cleanly without
// dropping data still in flight the other way.
type Stream struct{ *yamux.Stream }

// CloseWrite sends FIN and keeps the read side open.
func (s Stream) CloseWrite() error { return s.Stream.Close() }
