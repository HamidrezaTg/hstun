// Package mux is the thin wrapper around hashicorp/yamux with the settings
// hstun uses. yamux gives per-stream receive windows, so one stalled stream
// cannot wedge the session or grow memory without bound.
package mux

import (
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"

	"hstun/internal/netx"
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
	c.EnableKeepAlive = keepAlive > 0
	if keepAlive > 0 {
		c.KeepAliveInterval = keepAlive
	}
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
	return yamux.Client(wrap(c), config(keepAlive, opts...))
}

// Dialer wraps a carrier conn the local side dialed.
func Dialer(c net.Conn, keepAlive time.Duration, opts ...Option) (*yamux.Session, error) {
	return yamux.Server(wrap(c), config(keepAlive, opts...))
}

// Stream is a yamux stream that can half-close. yamux's Close only sends a
// FIN, and the stream can still be read until the peer closes too, which is
// exactly CloseWrite. netx.Pipe uses it to end one direction cleanly without
// dropping data still in flight the other way.
type Stream struct{ *yamux.Stream }

// CloseWrite sends FIN and keeps the read side open.
func (s Stream) CloseWrite() error { return s.Stream.Close() }
