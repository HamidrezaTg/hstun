package e2e

import (
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"hstun/internal/testutil"
)

// bulk measures MB/s for pushing n bytes through the forward and getting them
// echoed back (both directions at once).
func bulk(t *testing.T, addr string, n int) float64 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(120 * time.Second))
	data := make([]byte, n)
	_, _ = rand.Read(data)
	start := time.Now()
	go func() { _, _ = c.Write(data) }()
	if _, err := io.CopyN(io.Discard, c, int64(n)); err != nil {
		t.Fatal(err)
	}
	return float64(n) / 1e6 / time.Since(start).Seconds()
}

// TestThroughputWithLatency reports speed with 100 ms RTT between client and
// server. The numbers are logged; the floors only catch a broken carrier.
func TestThroughputWithLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for _, tc := range []struct {
		carrier string
		floor   float64
	}{{"ws", 2.0}, {"http", 0.5}} {
		t.Run(tc.carrier, func(t *testing.T) {
			e := startServer(t, []string{"ws", "http"})
			fwd, _ := testutil.Forwarder(t, e.tunnel, 50*time.Millisecond)
			e.startClient(t, tc.carrier, 4, fwd, e.certPath)
			mbps := bulk(t, e.fwd, 16<<20)
			t.Logf("%s, 100 ms RTT, 16 MB echoed (16 up + 16 down): %.2f MB/s each way", tc.carrier, mbps)
			if mbps < tc.floor {
				t.Fatalf("%s throughput %.2f MB/s is below the %.1f floor", tc.carrier, mbps, tc.floor)
			}
		})
	}
}

// TestIdleCost measures bytes on the wire for an idle session over 60 s with
// the production ping interval. Idle tunnels must stay (nearly) silent: the
// L3 tunnels once burned a traffic quota while unused.
func TestIdleCost(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 60 s")
	}
	for _, carrierName := range []string{"ws", "http"} {
		carrierName := carrierName
		t.Run(carrierName, func(t *testing.T) {
			t.Parallel()
			e := startServer(t, []string{"ws", "http"})
			fwd, cnt := testutil.Forwarder(t, e.tunnel, 0)
			e.startClientPing(t, carrierName, 1, fwd, e.certPath, "25s")
			time.Sleep(2 * time.Second)
			s0, c0 := cnt.ToServer.Load(), cnt.FromServer.Load()
			time.Sleep(60 * time.Second)
			up, down := cnt.ToServer.Load()-s0, cnt.FromServer.Load()-c0
			t.Logf("%s idle for 60 s: %d B up, %d B down", carrierName, up, down)
			if up+down > 20*1024 {
				t.Fatalf("idle cost %d B/min is too high", up+down)
			}
		})
	}
}

// Oversize and malformed requests to the http carrier are refused cheaply.
func TestHTTPCarrierRefusesOversize(t *testing.T) {
	e := startServer(t, []string{"http"})
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: tlsInsecure(),
	}}
	req, _ := http.NewRequest("POST", "https://"+e.tunnel+"/hs/h/u", io.NopCloser(io.LimitReader(zero{}, 1<<30)))
	req.ContentLength = 1 << 40
	req.Header.Set("X-Hs-Sid", "00000000000000000000000000000000")
	start := time.Now()
	resp, err := hc.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusGone {
			t.Fatalf("status %d for oversize body", resp.StatusCode)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("oversize request took %v to refuse", d)
	}
}

type zero struct{}

func (zero) Read(p []byte) (int, error) { return len(p), nil }
