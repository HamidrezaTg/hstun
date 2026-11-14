// Package netx has the network plumbing that keeps hstun from ever wedging:
// TCP keepalive and user timeout on every socket, connections with idle
// deadlines, a bidirectional pipe with half-close, and a connection limiter.
package netx

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
)

const (
	KeepAlive   = 15 * time.Second
	UserTimeout = 30 * time.Second
)

// Tune applies TCP keepalive and (on Linux) TCP_USER_TIMEOUT so a peer that
// vanished is noticed by the kernel even if the application is idle.
func Tune(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetNoDelay(true)
	_ = tc.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     KeepAlive,
		Interval: KeepAlive,
		Count:    3,
	})
	setUserTimeout(tc, UserTimeout)
}

// TunedListener tunes every accepted connection.
type TunedListener struct{ net.Listener }

func (l TunedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		Tune(c)
	}
	return c, err
}

// LimitListener caps concurrent connections. When full, new connections are
// closed immediately instead of queueing (queueing is how a service wedges).
type LimitListener struct {
	net.Listener
	sem chan struct{}
}

func NewLimitListener(l net.Listener, max int) *LimitListener {
	return &LimitListener{Listener: l, sem: make(chan struct{}, max)}
}

func (l *LimitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
		default:
			_ = c.Close()
		}
	}
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

// Activity records the last time anything moved on either side of a pipe, so
// the idle timeout means "no traffic in either direction", not "this direction
// is quiet". Without it a long download with a silent upstream would be cut.
type Activity struct{ last atomic.Int64 }

func (a *Activity) touch()               { a.last.Store(time.Now().UnixNano()) }
func (a *Activity) since() time.Duration { return time.Since(time.Unix(0, a.last.Load())) }
func NewActivity() *Activity             { a := &Activity{}; a.touch(); return a }

// IdleConn extends the read and write deadline on every operation, so a
// connection with no traffic in either direction for `idle` fails instead of
// hanging forever. With Act set, a read timeout is retried while the other
// direction (sharing Act) was active within the idle window.
type IdleConn struct {
	net.Conn
	Idle time.Duration
	Act  *Activity
}

func (c *IdleConn) Read(p []byte) (int, error) {
	for {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.Idle))
		n, err := c.Conn.Read(p)
		if n > 0 && c.Act != nil {
			c.Act.touch()
		}
		if n == 0 && err != nil && c.Act != nil && IsTimeout(err) && c.Act.since() < c.Idle {
			continue // the other direction is busy: not idle
		}
		return n, err
	}
}

func (c *IdleConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.Idle))
	n, err := c.Conn.Write(p)
	if n > 0 && c.Act != nil {
		c.Act.touch()
	}
	return n, err
}

type closeWriter interface{ CloseWrite() error }

// resetter is a connection that can tell its peer "this was cut" (a yamux stream).
type resetter interface{ Reset() error }

// Abort ends c abruptly so that the peer sees an error and not a clean end of
// stream: a yamux stream sends a reset, a TCP connection closes with RST
// (linger 0), anything else is closed. Used when a transfer was truncated, so
// the application on the other side does not mistake it for a complete one.
func Abort(c net.Conn) {
	for i := 0; i < 4; i++ {
		switch v := c.(type) {
		case resetter:
			_ = v.Reset()
			return
		case *net.TCPConn:
			_ = v.SetLinger(0)
			_ = v.Close()
			return
		case interface{ NetConn() net.Conn }: // tls.Conn and wrappers
			c = v.NetConn()
			continue
		}
		break
	}
	_ = c.Close()
}

// IsAbort reports whether a read error means the connection was cut (reset,
// peer vanished, session died) as opposed to ending normally or being closed
// locally. Idle timeouts are not aborts: they end the pipe quietly.
func IsAbort(err error) bool {
	return err != nil && (errors.Is(err, yamux.ErrConnectionReset) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, io.ErrUnexpectedEOF))
}

// copyDir copies src to dst and reports which side failed.
func copyDir(dst, src net.Conn) (readErr, writeErr error) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return nil, werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil, nil
			}
			return err, nil
		}
	}
}

// Pipe copies both ways between a and b until both directions end, with an
// idle timeout on each side, then closes both. Half-close is propagated when
// the connection supports it, so request/response protocols terminate cleanly.
// A direction that ends because a side was reset or cut is passed on as a
// reset (see Abort), so a truncated transfer is not reported as a complete one.
func Pipe(a, b net.Conn, idle time.Duration) {
	act := NewActivity()
	ia := &IdleConn{Conn: a, Idle: idle, Act: act}
	ib := &IdleConn{Conn: b, Idle: idle, Act: act}
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn, dstRaw, srcRaw net.Conn) {
		defer wg.Done()
		rerr, werr := copyDir(dst, src)
		switch {
		case werr != nil:
			// The data could not be delivered: neither side may think it was.
			Abort(srcRaw)
			Abort(dstRaw)
		case IsAbort(rerr):
			Abort(dstRaw)
		default:
			if cw, ok := dstRaw.(closeWriter); ok {
				_ = cw.CloseWrite()
			} else {
				// No half-close: end both directions so the other copy unblocks.
				_ = a.Close()
				_ = b.Close()
			}
		}
	}
	go cp(ib, ia, b, a)
	go cp(ia, ib, a, b)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

// IsTimeout reports whether err is a deadline error.
func IsTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// IPKey groups addresses for per-source limits: IPv6 by /64, since one host can
// rotate through a whole /64.
func IPKey(ip net.IP) string {
	if ip.To4() == nil && len(ip) == net.IPv6len {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

// IPLimiter counts concurrent connections per source key.
type IPLimiter struct {
	max int
	mu  sync.Mutex
	n   map[string]int
}

// NewIPLimiter makes a limiter; max <= 0 disables it.
func NewIPLimiter(max int) *IPLimiter { return &IPLimiter{max: max, n: map[string]int{}} }

// Acquire takes a slot for c's source and returns the function that frees it,
// or false when the source is at its cap.
func (l *IPLimiter) Acquire(c net.Conn) (release func(), ok bool) {
	if l == nil || l.max <= 0 {
		return func() {}, true
	}
	a, isTCP := c.RemoteAddr().(*net.TCPAddr)
	if !isTCP {
		return func() {}, true
	}
	key := IPKey(a.IP)
	l.mu.Lock()
	if l.n[key] >= l.max {
		l.mu.Unlock()
		return nil, false
	}
	l.n[key]++
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.n[key]--; l.n[key] <= 0 {
				delete(l.n, key)
			}
			l.mu.Unlock()
		})
	}, true
}
