package e2e

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"hstun/internal/client"
	"hstun/internal/config"
)

// One key's sessions share one budget: two sessions moving data at once may not
// get twice the limit.
func TestPerKeyBandwidthIsSharedAcrossSessions(t *testing.T) {
	d := startDirectServerMut(t, []string{"ws"}, false, 256, func(c *config.ServerFile) {
		c.Server.MaxMbitPerKey = 8 // 1 MB/s each direction
	})
	d.startClient(t, "ws", 2, d.tunnel, d.certPath)
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- roundTrip(d.fwd, 2<<20) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// 4 MB at 1 MB/s with a quarter-second burst is about 3.75 s; two independent
	// per-session budgets would finish in about 2 s.
	if el := time.Since(start); el < 3*time.Second {
		t.Fatalf("4 MB through an 8 Mbit/s key took %v: the limit is not shared or not applied", el)
	}
}

func TestPerKeyNewStreamRate(t *testing.T) {
	d := startDirectServerMut(t, []string{"ws"}, false, 256, func(c *config.ServerFile) {
		c.Server.MaxNewStreamsPerSecPerKey = 2 // burst 10
	})
	d.startClient(t, "ws", 1, d.tunnel, d.certPath)
	var ok, bad atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if roundTrip(d.fwd, 1<<10) == nil {
				ok.Add(1)
			} else {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() < 8 || ok.Load() > 20 || bad.Load() < 15 {
		t.Fatalf("40 simultaneous streams at 2/s (burst 10): %d ok, %d refused", ok.Load(), bad.Load())
	}
	if d.srv.Throttled() < 15 {
		t.Fatalf("server counted %d throttled streams", d.srv.Throttled())
	}
	time.Sleep(1500 * time.Millisecond) // the bucket refills
	if err := roundTrip(d.fwd, 1<<10); err != nil {
		t.Fatalf("a stream after the refill: %v", err)
	}
}

// holdStream opens a stream through addr and makes sure it is established.
func holdStream(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	return c
}

func waitDone(ch chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestServerDrainsStreamsBeforeStopping(t *testing.T) {
	d := startDirectServerMut(t, []string{"ws"}, false, 256, func(c *config.ServerFile) { c.Server.DrainTimeout = "10s" })
	d.startClient(t, "ws", 1, d.tunnel, d.certPath)
	c := holdStream(t, d.fwd)
	defer c.Close()

	stopped := make(chan struct{})
	go func() { d.stop(); close(stopped) }()
	if waitDone(stopped, 800*time.Millisecond) {
		t.Fatal("the server stopped while a stream was still in flight")
	}
	// While draining: no new tunnel connections, and the exit refuses new streams.
	if conn, err := net.DialTimeout("tcp", d.tunnel, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("the tunnel port still accepts connections while draining")
	}
	if err := roundTrip(d.fwd, 1<<10); err == nil {
		t.Fatal("a new stream worked while the server was draining")
	}
	// The stream in flight still works.
	if _, err := c.Write([]byte("more")); err != nil {
		t.Fatalf("stream in flight broke during the drain: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "more" {
		t.Fatalf("stream in flight stopped echoing during the drain: %q %v", buf, err)
	}
	_ = c.Close()
	if !waitDone(stopped, 3*time.Second) {
		t.Fatal("the server did not stop after its last stream ended")
	}
}

// When the streams do not finish, the drain gives up after drain_timeout, and
// the user's connection is reset (not ended cleanly).
func TestServerDrainTimeoutResetsStreams(t *testing.T) {
	d := startDirectServerMut(t, []string{"ws"}, false, 256, func(c *config.ServerFile) { c.Server.DrainTimeout = "1s" })
	d.startClient(t, "ws", 1, d.tunnel, d.certPath)
	c := holdStream(t, d.fwd)
	defer c.Close()
	start := time.Now()
	stopped := make(chan struct{})
	go func() { d.stop(); close(stopped) }()
	if !waitDone(stopped, 5*time.Second) {
		t.Fatal("the server did not stop after the drain timeout")
	}
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Fatalf("stopped after %v: the drain did not wait", el)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := io.ReadAll(c)
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("the cut stream reached the user as %v, want a connection reset", err)
	}
}

func TestClientDrainsStreamsBeforeStopping(t *testing.T) {
	d := startDirectServer(t, []string{"ws"}, false)
	cfg := d.clientCfg("ws", 1, d.tunnel, d.certPath)
	cfg.Client.DrainTimeout = "10s"
	cli, err := client.New(cfg, d.log)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() { cli.Run(ctx); close(stopped) }()
	waitFor(t, 10*time.Second, func() bool { return cli.Sessions() >= 1 })
	addr := cli.ForwardAddr("x1").String()
	c := holdStream(t, addr)
	defer c.Close()

	cancel()
	if waitDone(stopped, 800*time.Millisecond) {
		t.Fatal("the client stopped while a stream was still in flight")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("the forward port still accepts connections while draining")
	}
	_ = c.Close()
	if !waitDone(stopped, 3*time.Second) {
		t.Fatal("the client did not stop after its last stream ended")
	}
}

// resetTarget is a forward target that sends some data and then resets (RST).
func resetTarget(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("partial"))
			time.Sleep(200 * time.Millisecond)
			_ = c.(*net.TCPConn).SetLinger(0)
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// A reset on the target side must arrive at the user as a reset, not as a
// clean end that looks like a complete transfer.
func TestTargetResetReachesUserThroughTheTunnel(t *testing.T) {
	target := resetTarget(t)
	d := startDirectServerMut(t, []string{"ws"}, false, 256, func(c *config.ServerFile) { c.Forward[0].Target = target })
	d.startClient(t, "ws", 1, d.tunnel, d.certPath)
	c, err := net.DialTimeout("tcp", d.fwd, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err = io.ReadAll(c)
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("user of a reset target got %v, want a connection reset", err)
	}
}

// And the other way: a user that resets reaches the target as a reset.
func TestUserResetReachesTargetThroughTheTunnel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	result := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			result <- err
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err = io.ReadAll(c)
		result <- err
	}()
	d := startDirectServerMut(t, []string{"ws"}, false, 256, func(c *config.ServerFile) { c.Forward[0].Target = ln.Addr().String() })
	d.startClient(t, "ws", 1, d.tunnel, d.certPath)
	u, err := net.DialTimeout("tcp", d.fwd, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = u.Write([]byte("request"))
	time.Sleep(300 * time.Millisecond)
	_ = u.(*net.TCPConn).SetLinger(0)
	_ = u.Close()
	select {
	case err := <-result:
		if !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("target of a reset user got %v, want a connection reset", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the target never noticed that the user went away")
	}
}
