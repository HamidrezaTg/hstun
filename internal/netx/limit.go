package netx

import (
	"net"
	"sync"
	"time"
)

// Bucket is a token bucket. Tokens refill at `rate` per second up to `burst`.
// It is safe for concurrent use and can be shared by many connections, which
// then share one budget.
type Bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// NewBucket makes a full bucket. rate must be positive.
func NewBucket(rate, burst float64) *Bucket {
	return newBucket(rate, burst, time.Now)
}

func newBucket(rate, burst float64, now func() time.Time) *Bucket {
	return &Bucket{rate: rate, burst: burst, tokens: burst, last: now(), now: now}
}

func (b *Bucket) refill() {
	t := b.now()
	b.tokens += t.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = t
}

// Reserve takes n tokens now and returns how long the caller must wait before
// using them (0 when they were available). The debt is kept, so concurrent
// callers queue up behind each other in the right order.
func (b *Bucket) Reserve(n float64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	b.tokens -= n
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// Allow takes one token if there is one and reports whether it did. It never waits.
func (b *Bucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// throttled limits the byte rate of a connection with shared buckets (nil =
// unlimited in that direction). Reads are paced after the data arrived, writes
// before they are sent, so deadlines set on the connection keep their meaning
// and the sender is slowed down by ordinary TCP back-pressure.
type throttled struct {
	net.Conn
	rx, tx *Bucket
}

// Throttle wraps c so that reads draw from rx and writes from tx. With both
// nil it returns c unchanged.
func Throttle(c net.Conn, rx, tx *Bucket) net.Conn {
	if rx == nil && tx == nil {
		return c
	}
	return &throttled{Conn: c, rx: rx, tx: tx}
}

func (t *throttled) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 && t.rx != nil {
		time.Sleep(t.rx.Reserve(float64(n)))
	}
	return n, err
}

func (t *throttled) Write(p []byte) (int, error) {
	if t.tx != nil {
		time.Sleep(t.tx.Reserve(float64(len(p))))
	}
	return t.Conn.Write(p)
}

// NetConn lets Abort reach the connection underneath.
func (t *throttled) NetConn() net.Conn { return t.Conn }

// WaitDrained polls active until it returns 0 or timeout passes, and reports
// whether it reached 0. It returns at once when nothing is active.
func WaitDrained(timeout time.Duration, active func() int) bool {
	deadline := time.Now().Add(timeout)
	for {
		if active() == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
