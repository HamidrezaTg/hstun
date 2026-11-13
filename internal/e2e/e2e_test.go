// Package e2e runs the server and client in-process over loopback TLS.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"hstun/internal/auth"
	"hstun/internal/client"
	"hstun/internal/config"
	"hstun/internal/server"
	"hstun/internal/testutil"
)

type env struct {
	srv       *server.Server
	cancel    context.CancelFunc
	echo      string
	fwd       string // server forward address
	socks     string
	secret    string
	certPath  string
	keyPath   string
	tunnel    string // server tunnel address
	srvCfg    *config.ServerFile
	log       *slog.Logger
	waitStops func()
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startServer(t *testing.T, carriers []string) *env {
	t.Helper()
	return startServerOpt(t, carriers, nil)
}

// startServerOpt is startServer with a hook to adjust the config before it starts.
func startServerOpt(t *testing.T, carriers []string, mut func(*config.ServerFile)) *env {
	t.Helper()
	e := &env{log: quiet()}
	e.echo = testutil.Echo(t)
	e.certPath, e.keyPath = testutil.Cert(t)
	e.secret = filepath.Join(t.TempDir(), "secret")
	k, _ := auth.Generate()
	if err := auth.WriteKeyFile(e.secret, k); err != nil {
		t.Fatal(err)
	}
	e.srvCfg = &config.ServerFile{
		Server: config.ServerSection{
			Listen: "127.0.0.1:0", TLSCert: e.certPath, TLSKey: e.keyPath,
			Carriers: carriers, WSPath: "/hs/ws", HTTPPath: "/hs/h",
			SecretFiles: []string{e.secret},
			MaxConns:    1024, MaxSessionsPerKey: 16, MaxStreamsPerSession: 256,
			AuthFailPerMinPerIP: 50, IdleTimeout: "30s",
		},
		Forward: []config.Forward{{Name: "x1", Listen: "127.0.0.1:0"}},
		Socks:   config.SocksSection{Enabled: true, Listen: "127.0.0.1:0"},
	}
	if mut != nil {
		mut(e.srvCfg)
	}
	srv, err := server.New(e.srvCfg, e.log)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.srv = srv
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(ctx) }()
	e.waitStops = wg.Wait
	t.Cleanup(func() { cancel(); wg.Wait() })
	e.tunnel = srv.Addr().String()
	e.fwd = srv.ForwardAddr("x1").String()
	e.socks = srv.SocksAddr().String()
	return e
}

func (e *env) startClient(t *testing.T, carrierName string, conns int, serverAddr, caPath string) context.CancelFunc {
	t.Helper()
	return e.startClientPing(t, carrierName, conns, serverAddr, caPath, "2s")
}

func (e *env) startClientPing(t *testing.T, carrierName string, conns int, serverAddr, caPath, ping string) context.CancelFunc {
	t.Helper()
	cfg := &config.ClientFile{
		Client: config.ClientSection{
			Server: serverAddr, Carrier: carrierName, Conns: conns,
			SecretFile: e.secret, Name: "test", WSPath: "/hs/ws", HTTPPath: "/hs/h",
			TLSVerify: "ca_file", CAFile: caPath, Ping: ping, IdleTimeout: "30s",
		},
		Forward:   []config.Forward{{Name: "x1", Target: e.echo}},
		SocksExit: config.SocksExit{Enabled: true, DenyPrivate: false},
	}
	c, err := client.New(cfg, e.log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, 10*time.Second, func() bool { return e.srv.Sessions() >= conns })
	return cancel
}

func waitFor(t *testing.T, d time.Duration, f func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// roundTrip sends n random bytes through addr (an echo path) and checks them.
func roundTrip(addr string, n int) error {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	want := make([]byte, n)
	_, _ = rand.Read(want)
	errc := make(chan error, 1)
	go func() {
		_, err := c.Write(want)
		errc <- err
	}()
	got := make([]byte, n)
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if err := <-errc; err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("data mismatch")
	}
	return nil
}

func TestWSForwardAndConcurrency(t *testing.T) {
	e := startServer(t, []string{"ws"})
	e.startClient(t, "ws", 2, e.tunnel, e.certPath)
	if err := roundTrip(e.fwd, 64<<10); err != nil {
		t.Fatalf("single stream: %v", err)
	}
	if err := roundTrip(e.fwd, 8<<20); err != nil {
		t.Fatalf("8 MB: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := roundTrip(e.fwd, 100<<10); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent: %v", err)
	}
}

func socksConnect(socksAddr, target string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", socksAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	host, port, _ := net.SplitHostPort(target)
	var p int
	fmt.Sscanf(port, "%d", &p)
	_, _ = c.Write([]byte{5, 1, 0})
	var r [2]byte
	if _, err := io.ReadFull(c, r[:]); err != nil {
		return nil, err
	}
	req := []byte{5, 1, 0, 1}
	req = append(req, net.ParseIP(host).To4()...)
	req = append(req, byte(p>>8), byte(p))
	_, _ = c.Write(req)
	var rep [10]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		return nil, err
	}
	if rep[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks reply %d", rep[1])
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

func TestWSSocksExit(t *testing.T) {
	e := startServer(t, []string{"ws"})
	e.startClient(t, "ws", 1, e.tunnel, e.certPath)
	c, err := socksConnect(e.socks, e.echo)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("hello"))
	buf := make([]byte, 5)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("echo via socks: %q %v", buf, err)
	}
}

func TestBehindCDNProxy(t *testing.T) {
	e := startServer(t, []string{"ws"})
	proxyAddr, proxyCert := testutil.CDNProxy(t, e.tunnel)
	e.startClient(t, "ws", 2, proxyAddr, proxyCert)
	if err := roundTrip(e.fwd, 4<<20); err != nil {
		t.Fatalf("via CDN-like proxy: %v", err)
	}
}

func TestWrongSecretRejected(t *testing.T) {
	e := startServer(t, []string{"ws"})
	bad := filepath.Join(t.TempDir(), "bad")
	k, _ := auth.Generate()
	if err := auth.WriteKeyFile(bad, k); err != nil {
		t.Fatal(err)
	}
	cfg := &config.ClientFile{
		Client: config.ClientSection{
			Server: e.tunnel, Carrier: "ws", Conns: 1, SecretFile: bad, Name: "evil",
			WSPath: "/hs/ws", HTTPPath: "/hs/h", TLSVerify: "ca_file", CAFile: e.certPath,
			Ping: "2s", IdleTimeout: "30s",
		},
	}
	c, err := client.New(cfg, e.log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.Run(ctx)
	if n := e.srv.Sessions(); n != 0 {
		t.Fatalf("session established with wrong key: %d", n)
	}
}

func TestUnknownForwardName(t *testing.T) {
	e := startServer(t, []string{"ws"})
	e.startClient(t, "ws", 1, e.tunnel, e.certPath)
	// The client maps only "x1"; a server asking for another name is refused.
	st, code, err := e.srv.OpenForTest("nope")
	if st != nil || err != nil || code == 0 {
		t.Fatalf("unknown forward not refused: st=%v code=%d err=%v", st, code, err)
	}
}

func TestHTTPCarrierForward(t *testing.T) {
	e := startServer(t, []string{"ws", "http"})
	e.startClient(t, "http", 2, e.tunnel, e.certPath)
	if err := roundTrip(e.fwd, 64<<10); err != nil {
		t.Fatalf("64 KB: %v", err)
	}
	if err := roundTrip(e.fwd, 4<<20); err != nil {
		t.Fatalf("4 MB: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := roundTrip(e.fwd, 50<<10); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent: %v", err)
	}
}

func TestHTTPCarrierSocksAndCDN(t *testing.T) {
	e := startServer(t, []string{"http"})
	proxyAddr, proxyCert := testutil.CDNProxy(t, e.tunnel)
	e.startClient(t, "http", 1, proxyAddr, proxyCert)
	c, err := socksConnect(e.socks, e.echo)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("hello over http"))
	buf := make([]byte, 15)
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello over http" {
		t.Fatalf("echo via http carrier: %q %v", buf, err)
	}
	if err := roundTrip(e.fwd, 2<<20); err != nil {
		t.Fatalf("via CDN-like proxy: %v", err)
	}
}

// The http carrier must survive its TCP connections being reset mid-transfer:
// the session is identified by id, not by connection, and chunks are resent.
func TestHTTPCarrierSurvivesConnectionResets(t *testing.T) {
	e := startServer(t, []string{"http"})
	proxyAddr, proxyCert, kill := testutil.CDNProxyKillable(t, e.tunnel)
	e.startClient(t, "http", 1, proxyAddr, proxyCert)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
				kill()
			}
		}
	}()
	defer close(stop)
	if err := roundTrip(e.fwd, 3<<20); err != nil {
		t.Fatalf("transfer across resets: %v", err)
	}
}

// A ws session dies with its connection, but the slot reconnects and new
// streams work again.
func TestWSReconnectsAfterReset(t *testing.T) {
	e := startServer(t, []string{"ws"})
	proxyAddr, proxyCert, kill := testutil.CDNProxyKillable(t, e.tunnel)
	e.startClient(t, "ws", 1, proxyAddr, proxyCert)
	if err := roundTrip(e.fwd, 1024); err != nil {
		t.Fatal(err)
	}
	kill()
	waitFor(t, 15*time.Second, func() bool {
		return roundTrip(e.fwd, 1024) == nil
	})
}

func tlsInsecure() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}
}

// A path that cuts every connection after a small byte budget per direction
// (what Iran does to Cloudflare customer names): the plain http carrier, which
// reuses long connections and sends 64 KB bodies, stalls; the chunk carrier,
// one connection per small request, carries the data and never hits the cut.
func TestChunkCarrierUnderPerConnectionBudget(t *testing.T) {
	e := startServer(t, []string{"ws", "http"})
	proxy, cuts := testutil.BudgetProxy(t, e.tunnel, 24<<10, 48<<10)
	e.startClientPing(t, "chunk", 1, proxy, e.certPath, "2s")
	if err := roundTrip(e.fwd, 1<<20); err != nil {
		t.Fatalf("1 MB through the chunk carrier under the budget: %v", err)
	}
	if err := roundTrip(e.fwd, 64<<10); err != nil {
		t.Fatalf("second transfer: %v", err)
	}
	if n := cuts.Load(); n != 0 {
		t.Fatalf("chunk carrier exceeded the per-connection budget on %d connections", n)
	}
}

func TestBudgetProxyBreaksThePlainHTTPCarrier(t *testing.T) {
	e := startServer(t, []string{"ws", "http"})
	proxy, cuts := testutil.BudgetProxy(t, e.tunnel, 24<<10, 48<<10)
	e.startClientPing(t, "http", 1, proxy, e.certPath, "2s")
	done := make(chan error, 1)
	go func() { done <- roundTrip(e.fwd, 1<<20) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("plain http carrier unexpectedly got 1 MB through a 24 KB per-connection budget")
		}
	case <-time.After(8 * time.Second):
	}
	if cuts.Load() == 0 {
		t.Fatal("the budget proxy never cut a connection, so it proves nothing")
	}
}

func TestChunkCarrierConcurrentStreams(t *testing.T) {
	e := startServer(t, []string{"ws", "http"})
	proxy, cuts := testutil.BudgetProxy(t, e.tunnel, 24<<10, 48<<10)
	e.startClientPing(t, "chunk", 1, proxy, e.certPath, "2s")
	errc := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { errc <- roundTrip(e.fwd, 2<<20) }()
	}
	for i := 0; i < 4; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("stream %d of 4 concurrent 2 MB transfers: %v (cuts=%d)", i, err, cuts.Load())
		}
	}
	if n := cuts.Load(); n != 0 {
		t.Fatalf("budget exceeded on %d connections", n)
	}
}

// The real path adds latency and silently eats a few percent of connections.
func TestChunkCarrierFlakyPath(t *testing.T) {
	e := startServer(t, []string{"ws", "http"})
	proxy, _ := testutil.FlakyBudgetProxy(t, e.tunnel, 24<<10, 48<<10, 5, 40*time.Millisecond)
	e.startClientPing(t, "chunk", 1, proxy, e.certPath, "2s")
	errc := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { errc <- roundTrip(e.fwd, 1<<20) }()
	}
	for i := 0; i < 4; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("stream %d of 4 concurrent 1 MB transfers over a flaky path: %v", i, err)
		}
	}
}
