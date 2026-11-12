package entry

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"hstun/internal/config"
	"hstun/internal/netx"
	"hstun/internal/proto"
	"hstun/internal/socks5"
)

// Entry accepts user connections on the configured forward and SOCKS5
// listeners and pipes each one through a stream opened on the hub.
type Entry struct {
	Hub  *Hub
	log  *slog.Logger
	idle time.Duration

	forwards []config.Forward
	socks    config.SocksSection
	perIP    *netx.IPLimiter // per-source cap on user connections (nil/0 = off)

	fwdLn   map[string]net.Listener
	socksLn net.Listener

	// Streams counts streams opened, StreamErr the ones that failed to open.
	Streams, StreamErr atomic.Int64
}

func New(hub *Hub, logger *slog.Logger, idle time.Duration, forwards []config.Forward, socks config.SocksSection, maxConnsPerIP int) *Entry {
	return &Entry{
		Hub: hub, log: logger, idle: idle, forwards: forwards, socks: socks,
		perIP: netx.NewIPLimiter(maxConnsPerIP),
		fwdLn: map[string]net.Listener{},
	}
}

// Listen binds every listener so a busy port is reported before serving.
func (e *Entry) Listen() error {
	for _, f := range e.forwards {
		l, err := net.Listen("tcp", f.Listen)
		if err != nil {
			e.Close()
			return fmt.Errorf("forward %s: %w", f.Name, err)
		}
		e.fwdLn[f.Name] = l
	}
	if e.socks.Enabled {
		l, err := net.Listen("tcp", e.socks.Listen)
		if err != nil {
			e.Close()
			return fmt.Errorf("socks: %w", err)
		}
		e.socksLn = l
	}
	return nil
}

// Close closes the listeners.
func (e *Entry) Close() {
	for _, l := range e.fwdLn {
		_ = l.Close()
	}
	if e.socksLn != nil {
		_ = e.socksLn.Close()
	}
}

// ForwardAddr returns the address of a named forward listener.
func (e *Entry) ForwardAddr(name string) net.Addr {
	if l := e.fwdLn[name]; l != nil {
		return l.Addr()
	}
	return nil
}

// SocksAddr returns the SOCKS5 listener address, or nil.
func (e *Entry) SocksAddr() net.Addr {
	if e.socksLn == nil {
		return nil
	}
	return e.socksLn.Addr()
}

// Serve runs the listeners until ctx is cancelled, then closes them.
func (e *Entry) Serve(ctx context.Context) {
	var wg sync.WaitGroup
	for name, l := range e.fwdLn {
		name, l := name, l
		wg.Add(1)
		go func() { defer wg.Done(); e.serveForward(ctx, name, l) }()
	}
	if e.socksLn != nil {
		wg.Add(1)
		go func() { defer wg.Done(); e.serveSocks(ctx, e.socksLn) }()
	}
	<-ctx.Done()
	e.Close()
	wg.Wait()
}

func (e *Entry) serveForward(ctx context.Context, name string, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		netx.Tune(c)
		go func() {
			sctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			st, _, err := e.Hub.OpenStream(sctx, proto.OpenHeader{Kind: proto.KindForward, Payload: name})
			cancel()
			if err != nil || st == nil {
				e.StreamErr.Add(1)
				_ = c.Close()
				return
			}
			e.Streams.Add(1)
			netx.Pipe(c, st, e.idle)
		}()
	}
}

func (e *Entry) serveSocks(ctx context.Context, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		netx.Tune(c)
		go func() {
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			addr, err := socks5.ReadRequest(c)
			if err != nil {
				_ = c.Close()
				return
			}
			sctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			st, code, err := e.Hub.OpenStream(sctx, proto.OpenHeader{Kind: proto.KindConnect, Payload: addr})
			cancel()
			if err != nil || st == nil {
				e.StreamErr.Add(1)
				if err != nil {
					code = proto.ReplyFailure
				}
				_ = socks5.WriteReply(c, code)
				_ = c.Close()
				return
			}
			if err := socks5.WriteReply(c, socks5.OK); err != nil {
				_ = st.Close()
				_ = c.Close()
				return
			}
			_ = c.SetDeadline(time.Time{})
			e.Streams.Add(1)
			netx.Pipe(c, st, e.idle)
		}()
	}
}

// OpenForTest opens a forward stream by name; it exists for the loopback tests.
func (e *Entry) OpenForTest(name string) (net.Conn, byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return e.Hub.OpenStream(ctx, proto.OpenHeader{Kind: proto.KindForward, Payload: name})
}
