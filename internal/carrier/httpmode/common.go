// Package httpmode is the HTTP request/response carrier: a reliable byte
// stream built from ordinary POST requests over keep-alive HTTP/1.1.
//
//	/o  hello            -> challenge (+ X-Hs-Cid)
//	/a  proof            -> accept + session id
//	/u  upstream chunk   (client -> server, X-Hs-Seq, HMAC tag)
//	/d  downstream poll  (server -> client, long-poll, X-Hs-Ack, HMAC tag)
//	/c  close
//
// TLS and authentication happen once per session, not once per chunk. Chunks
// carry sequence numbers, so a lost response or a reset TCP connection is
// recovered by resending, and the session survives it.
package httpmode

import (
	"crypto/sha256"
	"encoding/binary"
	"net"
	"os"
	"sync"
	"time"

	"hstun/internal/auth"
)

const (
	pathHello = "/o"
	pathAuth  = "/a"
	pathUp    = "/u"
	pathDown  = "/d"
	pathClose = "/c"

	hdrCid = "X-Hs-Cid"
	hdrSid = "X-Hs-Sid"
	hdrSeq = "X-Hs-Seq"
	hdrAck = "X-Hs-Ack"
	hdrTag = "X-Hs-Tag"
	hdrMax = "X-Hs-Mx" // chunk mode: payload budget the client wants per downstream response

	upChunk      = 64 << 10  // largest upstream body
	downChunk    = 128 << 10 // largest downstream response payload
	minDownChunk = 4 << 10   // smallest budget a chunk-mode client may ask for
	window       = 32        // reorder window in chunks
	txLimit      = 2 << 20   // unsent + unacked bytes before Write blocks
	rxLimit      = 1 << 20   // undelivered bytes before we push back
	hold         = 50 * time.Second
	// Long-poll holds for chunk-mode clients. The client gives a poll up after 15 s;
	// the server must always answer first. A poll the client has abandoned can stay
	// alive at the origin (a CDN does not pass a client disconnect on promptly) and
	// would swallow the next downstream data, so while the session is busy the hold is
	// short and a healthy client never cancels a poll.
	chunkHoldBusy = 3 * time.Second
	chunkHoldIdle = 10 * time.Second
	chunkBusyFor  = 10 * time.Second        // a session is busy this long after it last moved data
	chunkTxLimit  = 192 << 10               // chunk-mode send buffer in bytes, about a second of traffic
	chunkRTO      = 1500 * time.Millisecond // chunk mode resends an unacked downstream chunk this soon (3 s otherwise)
	rto           = 3 * time.Second
	sessIdle      = 90 * time.Second

	maxPending      = 256 // pending handshakes
	maxPendingPerIP = 8   // pending handshakes one source may hold
	pendingTTL      = 10 * time.Second
	tagLen          = 16
)

// reqTag authenticates one request with the session key.
func reqTag(sk []byte, path string, sid []byte, seq, ack uint64, body []byte) []byte {
	var n [16]byte
	binary.BigEndian.PutUint64(n[:8], seq)
	binary.BigEndian.PutUint64(n[8:], ack)
	h := sha256.Sum256(body)
	return auth.Mac(sk, "hstun1-req", []byte(path), sid, n[:], h[:])[:tagLen]
}

// bcast wakes every waiter at once. Read C() while holding the state lock,
// then wait on it after unlocking; Signal() after changing the state.
type bcast struct {
	mu sync.Mutex
	ch chan struct{}
}

func newBcast() *bcast { return &bcast{ch: make(chan struct{})} }

func (b *bcast) C() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ch
}

func (b *bcast) Signal() {
	b.mu.Lock()
	close(b.ch)
	b.ch = make(chan struct{})
	b.mu.Unlock()
}

// deadline mirrors net.Pipe's deadline helper.
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{}
}

func newDeadline() deadline { return deadline{cancel: make(chan struct{})} }

func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel
	}
	d.timer = nil
	closed := isClosed(d.cancel)
	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		c := d.cancel
		d.timer = time.AfterFunc(dur, func() { close(c) })
		return
	}
	if !closed {
		close(d.cancel)
	}
}

func (d *deadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

type addr string

func (a addr) Network() string { return "hstun-http" }
func (a addr) String() string  { return string(a) }

// sconn is the net.Conn both ends expose to yamux. Received bytes arrive via
// pushRx; written bytes are picked up by the transport via take, and confirm
// releases them once the peer has them.
type sconn struct {
	mu       sync.Mutex
	bc       *bcast
	rx       [][]byte
	rxLen    int
	tx       []byte
	inflight int
	limit    int // unsent + unacked bytes before Write blocks (txLimit, or chunkTxLimit)
	closed   bool
	eof      bool
	rd, wd   deadline
	local    addr
	remote   addr
	onClose  func()
	once     sync.Once
}

func newSconn(local, remote string, onClose func()) *sconn {
	return &sconn{bc: newBcast(), rd: newDeadline(), wd: newDeadline(), local: addr(local), remote: addr(remote), onClose: onClose, limit: txLimit}
}

// setTxLimit lowers the send buffer. A chunk-mode carrier moves a few Mbit/s, so
// the default 2 MB of queued bytes is seconds of delay in front of every control
// frame (window updates, acks) and starves the opposite direction.
func (c *sconn) setTxLimit(n int) {
	c.mu.Lock()
	c.limit = n
	c.mu.Unlock()
}

func (c *sconn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		c.mu.Lock()
		if len(c.rx) > 0 {
			n := copy(p, c.rx[0])
			if n == len(c.rx[0]) {
				c.rx[0] = nil
				c.rx = c.rx[1:]
			} else {
				c.rx[0] = c.rx[0][n:]
			}
			c.rxLen -= n
			c.mu.Unlock()
			c.bc.Signal()
			return n, nil
		}
		if c.closed || c.eof {
			c.mu.Unlock()
			return 0, errEOF
		}
		wake := c.bc.C()
		c.mu.Unlock()
		select {
		case <-wake:
		case <-c.rd.wait():
			return 0, os.ErrDeadlineExceeded
		}
	}
}

func (c *sconn) Write(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return 0, errClosed
		}
		if len(c.tx)+c.inflight < c.limit {
			c.tx = append(c.tx, p...)
			c.mu.Unlock()
			c.bc.Signal()
			return len(p), nil
		}
		wake := c.bc.C()
		c.mu.Unlock()
		select {
		case <-wake:
		case <-c.wd.wait():
			return 0, os.ErrDeadlineExceeded
		}
	}
}

// pushRx delivers received bytes to the reader.
func (c *sconn) pushRx(b []byte) {
	c.mu.Lock()
	if !c.closed {
		c.rx = append(c.rx, b)
		c.rxLen += len(b)
	}
	c.mu.Unlock()
	c.bc.Signal()
}

func (c *sconn) rxBuffered() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rxLen
}

// take removes up to max written-but-unsent bytes and counts them as in
// flight until confirm is called. It does not block.
func (c *sconn) take(max int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.tx)
	if n > max {
		n = max
	}
	if n == 0 {
		return nil
	}
	out := make([]byte, n)
	copy(out, c.tx[:n])
	rest := copy(c.tx, c.tx[n:])
	c.tx = c.tx[:rest]
	c.inflight += n
	return out
}

func (c *sconn) confirm(n int) {
	c.mu.Lock()
	c.inflight -= n
	if c.inflight < 0 {
		c.inflight = 0
	}
	c.mu.Unlock()
	c.bc.Signal()
}

func (c *sconn) hasTx() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.tx) > 0
}

func (c *sconn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *sconn) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.bc.Signal()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return nil
}

func (c *sconn) LocalAddr() net.Addr  { return c.local }
func (c *sconn) RemoteAddr() net.Addr { return c.remote }
func (c *sconn) SetDeadline(t time.Time) error {
	c.rd.set(t)
	c.wd.set(t)
	return nil
}
func (c *sconn) SetReadDeadline(t time.Time) error  { c.rd.set(t); return nil }
func (c *sconn) SetWriteDeadline(t time.Time) error { c.wd.set(t); return nil }

// nextChunk takes up to max unsent bytes and assigns the next upstream
// sequence number atomically, so sequence order equals byte order. When there
// is nothing to send it returns a channel to wait on.
func (c *sconn) nextChunk(max int, seq *uint64) (data []byte, s uint64, closed bool, wake <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, 0, true, nil
	}
	n := len(c.tx)
	if n == 0 {
		return nil, 0, false, c.bc.C()
	}
	if n > max {
		n = max
	}
	data = make([]byte, n)
	copy(data, c.tx[:n])
	rest := copy(c.tx, c.tx[n:])
	c.tx = c.tx[:rest]
	c.inflight += n
	*seq++
	return data, *seq, false, nil
}
