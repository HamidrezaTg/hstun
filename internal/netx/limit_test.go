package netx

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time            { return f.t }
func (f *fakeClock) add(d time.Duration)       { f.t = f.t.Add(d) }
func newFake() *fakeClock                      { return &fakeClock{t: time.Unix(1000, 0)} }
func within(got, want, tol time.Duration) bool { return got >= want-tol && got <= want+tol }

func TestBucketReserve(t *testing.T) {
	c := newFake()
	b := newBucket(100, 200, c.now) // 100 per second, burst 200
	if w := b.Reserve(150); w != 0 {
		t.Fatalf("within the burst: waited %v", w)
	}
	if w := b.Reserve(100); !within(w, 500*time.Millisecond, time.Millisecond) { // 50 tokens left, 50 short
		t.Fatalf("50 tokens short at 100/s should wait 0.5s, got %v", w)
	}
	// The debt stays: the next caller queues behind the first.
	if w := b.Reserve(100); !within(w, 1500*time.Millisecond, time.Millisecond) {
		t.Fatalf("queued behind the first: want 1.5s, got %v", w)
	}
	c.add(10 * time.Second) // refill is capped at the burst
	if w := b.Reserve(200); w != 0 {
		t.Fatalf("after a long pause the whole burst is available, waited %v", w)
	}
	if w := b.Reserve(1); w == 0 {
		t.Fatal("the burst cap was exceeded")
	}
}

func TestBucketAllow(t *testing.T) {
	c := newFake()
	b := newBucket(2, 3, c.now)
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("token %d of the burst refused", i)
		}
	}
	if b.Allow() {
		t.Fatal("a 4th token was allowed")
	}
	c.add(500 * time.Millisecond) // one token at 2/s
	if !b.Allow() || b.Allow() {
		t.Fatal("refill should give exactly one token after 0.5s")
	}
}

func TestThrottleSlowsAConnection(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	tx := NewBucket(100<<10, 10<<10) // 100 KiB/s, 10 KiB burst
	w := Throttle(a, nil, tx)
	var got atomic.Int64
	go func() {
		n, _ := io.Copy(io.Discard, b)
		got.Store(n)
	}()
	start := time.Now()
	buf := make([]byte, 4<<10)
	for i := 0; i < 50; i++ { // 200 KiB
		if _, err := w.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	// (200 - 10) KiB at 100 KiB/s is about 1.9 s.
	if el := time.Since(start); el < 1500*time.Millisecond {
		t.Fatalf("200 KiB at 100 KiB/s took %v", el)
	}
}

func TestThrottleWithNoBucketsIsTransparent(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if Throttle(a, nil, nil) != a {
		t.Fatal("no buckets should return the connection itself")
	}
}

func TestWaitDrained(t *testing.T) {
	if !WaitDrained(time.Second, func() int { return 0 }) {
		t.Fatal("nothing active should return at once")
	}
	var n atomic.Int64
	n.Store(3)
	go func() { time.Sleep(150 * time.Millisecond); n.Store(0) }()
	if !WaitDrained(2*time.Second, func() int { return int(n.Load()) }) {
		t.Fatal("did not notice the drain")
	}
	start := time.Now()
	if WaitDrained(200*time.Millisecond, func() int { return 1 }) {
		t.Fatal("reported drained while something was active")
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("gave up too early")
	}
}
