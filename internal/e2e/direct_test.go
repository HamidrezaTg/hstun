package e2e

import (
	"context"
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

// Direct mode: the server listens and is the exit, the client dials and is
// the entry that owns the forward and SOCKS5 ports.

type direct struct {
	srv      *server.Server
	cli      *client.Client
	echo     string
	secret   string
	certPath string
	tunnel   string
	fwd      string
	socks    string
	log      *slog.Logger
	stop     func() // cancels the server and waits for Serve to return (idempotent)
}

func startDirectServer(t *testing.T, carriers []string, denyPrivate bool) *direct {
	t.Helper()
	return startDirectServerCap(t, carriers, denyPrivate, 256)
}

func startDirectServerCap(t *testing.T, carriers []string, denyPrivate bool, maxStreams int) *direct {
	t.Helper()
	return startDirectServerMut(t, carriers, denyPrivate, maxStreams, nil)
}

// startDirectServerMut is startDirectServerCap with a hook to change the server config.
func startDirectServerMut(t *testing.T, carriers []string, denyPrivate bool, maxStreams int, mut func(*config.ServerFile)) *direct {
	t.Helper()
	d := &direct{log: quiet()}
	d.echo = testutil.Echo(t)
	var keyPath string
	d.certPath, keyPath = testutil.Cert(t)
	d.secret = filepath.Join(t.TempDir(), "secret")
	k, _ := auth.Generate()
	if err := auth.WriteKeyFile(d.secret, k); err != nil {
		t.Fatal(err)
	}
	cfg := &config.ServerFile{
		Server: config.ServerSection{
			Role: config.RoleExit, Listen: "127.0.0.1:0", TLSCert: d.certPath, TLSKey: keyPath,
			Carriers: carriers, WSPath: "/hs/ws", HTTPPath: "/hs/h", SecretFiles: []string{d.secret},
			MaxConns: 1024, MaxSessionsPerKey: 16, MaxStreamsPerSession: maxStreams,
			AuthFailPerMinPerIP: 50, IdleTimeout: "30s",
		},
		Forward:   []config.Forward{{Name: "x1", Target: d.echo}},
		SocksExit: config.SocksExit{Enabled: true, DenyPrivate: denyPrivate},
	}
	if mut != nil {
		mut(cfg)
	}
	srv, err := server.New(cfg, d.log)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(ctx) }()
	d.stop = func() { cancel(); wg.Wait() }
	t.Cleanup(d.stop)
	d.srv, d.tunnel = srv, srv.Addr().String()
	return d
}

func (d *direct) clientCfg(carrierName string, conns int, serverAddr, caPath string) *config.ClientFile {
	return &config.ClientFile{
		Client: config.ClientSection{
			Role: config.RoleEntry, Server: serverAddr, Carrier: carrierName, Conns: conns,
			SecretFile: d.secret, Name: "entry", WSPath: "/hs/ws", HTTPPath: "/hs/h",
			TLSVerify: "ca_file", CAFile: caPath, Ping: "2s", IdleTimeout: "30s",
			MaxStreamsPerSession: 256,
		},
		Forward: []config.Forward{{Name: "x1", Listen: "127.0.0.1:0"}},
		Socks:   config.SocksSection{Enabled: true, Listen: "127.0.0.1:0"},
	}
}

func (d *direct) startClient(t *testing.T, carrierName string, conns int, serverAddr, caPath string) {
	t.Helper()
	c, err := client.New(d.clientCfg(carrierName, conns, serverAddr, caPath), d.log)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	d.cli = c
	d.fwd = c.ForwardAddr("x1").String()
	d.socks = c.SocksAddr().String()
	waitFor(t, 10*time.Second, func() bool { return c.Sessions() >= conns })
}

func TestDirectWSForwardAndConcurrency(t *testing.T) {
	d := startDirectServer(t, []string{"ws"}, false)
	d.startClient(t, "ws", 2, d.tunnel, d.certPath)
	if err := roundTrip(d.fwd, 8<<20); err != nil {
		t.Fatalf("8 MB: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := roundTrip(d.fwd, 100<<10); err != nil {
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

func TestDirectHTTPCarrierForward(t *testing.T) {
	d := startDirectServer(t, []string{"http"}, false)
	d.startClient(t, "http", 2, d.tunnel, d.certPath)
	if err := roundTrip(d.fwd, 4<<20); err != nil {
		t.Fatalf("4 MB: %v", err)
	}
}

func TestDirectSocksExitAndDenyPrivate(t *testing.T) {
	// deny_private on: the echo target is on loopback, so it must be refused.
	d := startDirectServer(t, []string{"ws"}, true)
	d.startClient(t, "ws", 1, d.tunnel, d.certPath)
	if c, err := socksConnect(d.socks, d.echo); err == nil {
		c.Close()
		t.Fatal("exit connected to a loopback address with deny_private on")
	}
	// A forward still works: it is mapped to a fixed target in the exit's config.
	if err := roundTrip(d.fwd, 4096); err != nil {
		t.Fatalf("forward: %v", err)
	}
}

func TestDirectSocksExit(t *testing.T) {
	d := startDirectServer(t, []string{"ws"}, false)
	d.startClient(t, "ws", 1, d.tunnel, d.certPath)
	c, err := socksConnect(d.socks, d.echo)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("hello direct"))
	buf := make([]byte, 12)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello direct" {
		t.Fatalf("echo via socks: %q %v", buf, err)
	}
}

func TestDirectBehindCDNProxy(t *testing.T) {
	for _, carrierName := range []string{"ws", "http"} {
		t.Run(carrierName, func(t *testing.T) {
			d := startDirectServer(t, []string{carrierName}, false)
			proxyAddr, proxyCert := testutil.CDNProxy(t, d.tunnel)
			d.startClient(t, carrierName, 2, proxyAddr, proxyCert)
			if err := roundTrip(d.fwd, 2<<20); err != nil {
				t.Fatalf("via CDN-like proxy: %v", err)
			}
		})
	}
}

func TestDirectWrongSecretRejected(t *testing.T) {
	d := startDirectServer(t, []string{"ws"}, false)
	bad := filepath.Join(t.TempDir(), "bad")
	k, _ := auth.Generate()
	if err := auth.WriteKeyFile(bad, k); err != nil {
		t.Fatal(err)
	}
	cfg := d.clientCfg("ws", 1, d.tunnel, d.certPath)
	cfg.Client.SecretFile = bad
	c, err := client.New(cfg, d.log)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.Run(ctx)
	if n := d.srv.Sessions(); n != 0 || c.Sessions() != 0 {
		t.Fatalf("session established with wrong key: server %d client %d", n, c.Sessions())
	}
}

func TestDirectUnknownForwardRefused(t *testing.T) {
	d := startDirectServer(t, []string{"ws"}, false)
	d.startClient(t, "ws", 1, d.tunnel, d.certPath)
	st, code, err := d.cli.OpenForTest("nope")
	if st != nil || err != nil || code == 0 {
		t.Fatalf("unknown forward not refused: st=%v code=%d err=%v", st, code, err)
	}
}

func TestDirectReconnectsAfterReset(t *testing.T) {
	d := startDirectServer(t, []string{"ws"}, false)
	proxyAddr, proxyCert, kill := testutil.CDNProxyKillable(t, d.tunnel)
	d.startClient(t, "ws", 1, proxyAddr, proxyCert)
	if err := roundTrip(d.fwd, 1024); err != nil {
		t.Fatal(err)
	}
	kill()
	waitFor(t, 15*time.Second, func() bool { return roundTrip(d.fwd, 1024) == nil })
}

func TestDirectHTTPSurvivesResets(t *testing.T) {
	d := startDirectServer(t, []string{"http"}, false)
	proxyAddr, proxyCert, kill := testutil.CDNProxyKillable(t, d.tunnel)
	d.startClient(t, "http", 1, proxyAddr, proxyCert)
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
	if err := roundTrip(d.fwd, 3<<20); err != nil {
		t.Fatalf("transfer across resets: %v", err)
	}
}

// The exit caps concurrent streams per session, so a compromised entry cannot
// make it open unbounded outbound connections.
func TestExitStreamCap(t *testing.T) {
	const cap = 3
	d := startDirectServerCap(t, []string{"ws"}, false, cap)
	d.startClient(t, "ws", 1, d.tunnel, d.certPath)
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	working := 0
	for i := 0; i < cap+5; i++ {
		c, err := net.DialTimeout("tcp", d.fwd, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("x")); err != nil {
			continue
		}
		b := make([]byte, 1)
		if _, err := io.ReadFull(c, b); err == nil {
			working++
		}
	}
	if working != cap {
		t.Fatalf("%d streams worked with the exit cap at %d", working, cap)
	}
}
