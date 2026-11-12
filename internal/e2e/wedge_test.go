package e2e

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestWedge holds hundreds of half-dead connections of several kinds against
// the server and checks the service stays responsive for a legitimate client
// and that everything is released afterwards. This is the NipoVPN failure
// class: connections that die mid-protocol must never pin the service.
func TestWedge(t *testing.T) {
	if testing.Short() {
		t.Skip("wedge test takes ~40 s")
	}
	e := startServer(t, []string{"ws"})
	e.startClient(t, "ws", 2, e.tunnel, e.certPath)
	if err := roundTrip(e.fwd, 1024); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	base := runtime.NumGoroutine()

	insecure := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}
	var held []net.Conn
	var mu sync.Mutex
	keep := func(c net.Conn) { mu.Lock(); held = append(held, c); mu.Unlock() }
	var wg sync.WaitGroup
	spawn := func(n int, f func()) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); f() }()
		}
	}
	// 1. silent TCP: never sends a byte
	spawn(150, func() {
		if c, err := net.Dial("tcp", e.tunnel); err == nil {
			keep(c)
		}
	})
	// 2. TLS completed, then silent
	spawn(100, func() {
		if c, err := tls.Dial("tcp", e.tunnel, insecure); err == nil {
			keep(c)
		}
	})
	// 3. websocket upgraded, no hello
	// 4. websocket upgraded, partial hello
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: insecure, ForceAttemptHTTP2: false,
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}}
	wsConn := func() net.Conn {
		c, _, err := websocket.Dial(context.Background(), "wss://"+e.tunnel+"/hs/ws", &websocket.DialOptions{HTTPClient: hc})
		if err != nil {
			return nil
		}
		return websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	}
	spawn(100, func() {
		if c := wsConn(); c != nil {
			keep(c)
		}
	})
	spawn(100, func() {
		if c := wsConn(); c != nil {
			_, _ = c.Write([]byte{1, 0, 55, 'H', 'S', 'T'}) // hello header, truncated body
			keep(c)
		}
	})
	// 5. slowloris: HTTP request line and one header, then silence
	spawn(50, func() {
		c, err := tls.Dial("tcp", e.tunnel, insecure)
		if err == nil {
			_, _ = c.Write([]byte("GET /hs/ws HTTP/1.1\r\nHost: x\r\nX-A: "))
			keep(c)
		}
	})
	wg.Wait()
	mu.Lock()
	t.Logf("holding %d half-dead connections", len(held))
	mu.Unlock()

	// A legitimate client must stay fast while the junk is held.
	var lat []time.Duration
	for i := 0; i < 30; i++ {
		start := time.Now()
		if err := roundTrip(e.fwd, 32<<10); err != nil {
			t.Fatalf("legit request %d failed during attack: %v", i, err)
		}
		lat = append(lat, time.Since(start))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p99 := lat[len(lat)-1]
	t.Logf("legit latency under attack: median %v, p99 %v", lat[len(lat)/2], p99)
	if p99 > time.Second {
		t.Fatalf("p99 latency %v > 1s under attack", p99)
	}

	// Server must release the junk on its own (handshake and read deadlines).
	deadline := time.Now().Add(40 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		n = runtime.NumGoroutine()
		if n <= base+60 { // client-side attacker goroutines linger until we close
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	mu.Lock()
	for _, c := range held {
		c.Close()
	}
	mu.Unlock()
	time.Sleep(2 * time.Second)
	n = runtime.NumGoroutine()
	t.Logf("goroutines: baseline %d, after %d", base, n)
	if n > base+30 {
		t.Fatalf("goroutines did not return to baseline: %d vs %d", n, base)
	}
	if err := roundTrip(e.fwd, 1<<20); err != nil {
		t.Fatalf("service unhealthy after attack: %v", err)
	}
}
