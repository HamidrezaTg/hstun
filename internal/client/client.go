// Package client is the dialing side of the tunnel. It opens a pool of
// authenticated carrier sessions to the server, keeps them alive, and acts as
// the configured role on them:
//   - exit (reverse mode, default): serves the streams the server opens, to
//     fixed local targets or through the SOCKS exit;
//   - entry (direct mode): owns the forward and SOCKS5 listeners and opens a
//     stream per user connection.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"time"

	"hstun/internal/auth"
	"hstun/internal/carrier"
	"hstun/internal/carrier/httpmode"
	"hstun/internal/carrier/ws"
	"hstun/internal/config"
	"hstun/internal/entry"
	"hstun/internal/exit"
	"hstun/internal/mux"
	"hstun/internal/netx"
	"hstun/internal/proto"
	"hstun/internal/seal"
)

type Client struct {
	cfg  *config.ClientFile
	log  *slog.Logger
	key  []byte
	opts carrier.Options
	ping time.Duration
	idle time.Duration
	// stagger is the longest random delay before a non-first session's first dial.
	stagger time.Duration
	// drain is how long a stopping client lets the streams in flight finish.
	drain time.Duration

	pick *picker

	hub   *entry.Hub   // role entry
	entry *entry.Entry // role entry
	exit  *exit.Exit   // role exit
}

func New(cfg *config.ClientFile, logger *slog.Logger) (*Client, error) {
	key, err := auth.LoadKeyFile(cfg.Client.SecretFile)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(cfg.Client.Server)
	if err != nil {
		return nil, err
	}
	sni := cfg.Client.SNI
	if sni == "" {
		sni = host
	}
	tc, err := carrier.ClientTLS(sni, cfg.Client.TLSVerify, cfg.Client.CAFile, cfg.Client.SPKISHA256, cfg.Client.TLSMLKEM)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil && cfg.Client.SNI == "" {
		// Go sends no SNI for an IP literal, and some paths reset TLS without SNI
		// (seen on one real path: curl without SNI was reset, with SNI it worked).
		if cfg.Client.TLSVerify == "spki" {
			logger.Info("server is an IP address, so no TLS SNI is sent (normal with a pinned certificate)", "server", cfg.Client.Server)
		} else {
			logger.Warn("server is an IP address, so no TLS SNI is sent; if connections are reset right after connecting, set sni = \"some.hostname\"", "server", cfg.Client.Server)
		}
	}
	ping, _ := time.ParseDuration(cfg.Client.Ping)
	idle, _ := time.ParseDuration(cfg.Client.IdleTimeout)
	stagger, _ := time.ParseDuration(cfg.Client.DialStagger)
	drain, _ := time.ParseDuration(cfg.Client.DrainTimeout)
	c := &Client{
		cfg: cfg, log: logger, key: key, ping: ping, idle: idle, stagger: stagger, drain: drain,
		opts: carrier.Options{
			Server: cfg.Client.Server, Connect: cfg.Client.Connect, SNI: sni,
			WSPath: cfg.Client.WSPath, HTTPPath: cfg.Client.HTTPPath, TLS: tc, UserAgent: cfg.Client.UserAgent,
			Fingerprint: cfg.Client.TLSFingerprint,
			ChunkUp:     cfg.Client.ChunkUpBytes,
			ChunkDown:   cfg.Client.ChunkDownBytes, ChunkPar: cfg.Client.ChunkParallel,
		},
	}
	order := []string{cfg.Client.Carrier}
	if cfg.Client.Carrier == "auto" {
		order = defaultAutoOrder
		if len(cfg.Client.AutoOrder) > 0 {
			order = cfg.Client.AutoOrder
		}
	}
	c.pick = newPicker(order, logger)
	if cfg.Client.Role == config.RoleEntry {
		c.hub = entry.NewHub(0, cfg.Client.MaxStreamsPerSession)
		c.entry = entry.New(c.hub, logger, idle, cfg.Forward, cfg.Socks, cfg.Client.ForwardMaxConnsPerIP)
	} else {
		c.exit = exit.New(cfg.Forward, cfg.SocksExit, idle, cfg.Client.MaxStreamsPerSession)
	}
	return c, nil
}

// Listen binds the entry-role listeners (forwards, SOCKS5, admin) so a busy
// port is reported before running. It does nothing for the exit role.
func (c *Client) Listen() error {
	if c.entry == nil {
		return nil
	}
	return c.entry.Listen()
}

// ForwardAddr and SocksAddr expose the entry-role listeners (nil for exit).
func (c *Client) ForwardAddr(name string) net.Addr { return c.entry.ForwardAddr(name) }
func (c *Client) SocksAddr() net.Addr              { return c.entry.SocksAddr() }

// Sessions is the number of live carrier sessions (entry role).
func (c *Client) Sessions() int {
	if c.hub == nil {
		return 0
	}
	return c.hub.Count()
}

// Run starts `conns` independent slots and blocks until ctx is cancelled and
// the client has shut down. Cancelling ctx starts a graceful stop: the user
// ports close, no new session is dialed or stream accepted, the streams in
// flight get drain_timeout to finish, then the sessions are closed.
func (c *Client) Run(ctx context.Context) {
	hard, hardStop := context.WithCancel(context.Background())
	defer hardStop()
	var bg sync.WaitGroup
	defer bg.Wait()
	if c.entry != nil {
		bg.Add(1)
		go func() { defer bg.Done(); c.entry.Serve(ctx) }()
		if a := c.cfg.Client.AdminListen; a != "" {
			bg.Add(1)
			go func() { defer bg.Done(); c.serveAdmin(ctx, a) }()
		}
	}
	if c.pick.auto() {
		bg.Add(1)
		go func() { defer bg.Done(); c.probeLoop(ctx) }()
	}
	bg.Add(1)
	go func() {
		defer bg.Done()
		select {
		case <-ctx.Done():
			c.drainStreams()
			hardStop()
		case <-hard.Done():
		}
	}()
	done := make(chan struct{}, c.cfg.Client.Conns)
	for i := 0; i < c.cfg.Client.Conns; i++ {
		go func(i int) {
			c.slot(ctx, hard, i)
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < c.cfg.Client.Conns; i++ {
		<-done
	}
}

// active is the number of streams in flight.
func (c *Client) active() int {
	if c.exit != nil {
		return c.exit.Active()
	}
	return c.hub.ActiveStreams()
}

// drainStreams refuses new streams and waits (at most drain_timeout) for the
// ones in flight.
func (c *Client) drainStreams() {
	if c.exit != nil {
		c.exit.Drain()
	}
	n := c.active()
	if n == 0 || c.drain <= 0 {
		return
	}
	c.log.Info("draining: waiting for streams to finish", "streams", n, "timeout", c.drain.String())
	if netx.WaitDrained(c.drain, c.active) {
		c.log.Info("drained")
	} else {
		c.log.Warn("drain timeout: closing with streams still open", "streams", c.active())
	}
}

// slot keeps one carrier session alive, reconnecting with jittered backoff.
//
// ctx ends the loop (a stop was requested); hard ends the running session, which
// is later than ctx so streams in flight can drain.
func (c *Client) slot(ctx, hard context.Context, i int) {
	if d := staggerDelay(c.stagger, i); d > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		name, _ := c.pick.current()
		up, err := c.session(hard, i)
		if ctx.Err() != nil {
			return
		}
		if c.pick.auto() {
			if c.pick.report(name, up) {
				backoff = time.Second // fresh start on the new carrier
			}
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second // it was healthy for a while
		}
		wait := backoff/2 + time.Duration(rand.Int64N(int64(backoff)))
		c.log.Warn("session ended", "slot", i, "err", errString(err), "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// staggerDelay is how long slot i waits before its first dial: nothing for
// slot 0 or when staggering is off, otherwise a random time in [max/4, max].
// It only spreads connection set-up; it does not touch traffic timing.
func staggerDelay(max time.Duration, slot int) time.Duration {
	if slot == 0 || max <= 0 {
		return 0
	}
	lo := max / 4
	return lo + time.Duration(rand.Int64N(int64(max-lo)+1))
}

func errString(err error) string {
	if err == nil {
		return "closed"
	}
	return err.Error()
}

// dialCarrier opens one carrier session, runs the handshake and seals the
// stream with the session key.
func (c *Client) dialCarrier(ctx context.Context, name string) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var conn net.Conn
	var sk []byte
	var err error
	opts := c.opts
	opts.Chunk = name == "chunk"
	if name == "http" || name == "chunk" {
		conn, sk, err = httpmode.Dial(dctx, opts, c.key, c.cfg.Client.Name)
		if err != nil {
			return nil, err
		}
	} else {
		if conn, err = ws.Dial(dctx, opts); err != nil {
			return nil, err
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		res, err := proto.ClientHandshake(conn, c.key, proto.CarrierWS, c.cfg.Client.Name)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("handshake: %w", err)
		}
		_ = conn.SetDeadline(time.Time{})
		sk = res.SessionKey
	}
	sealed, err := seal.Wrap(conn, sk, true)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return sealed, nil
}

// session runs one carrier session on the carrier the picker names. up is
// true when the session got established (so the attempt counts as a success).
func (c *Client) session(ctx context.Context, slot int) (up bool, err error) {
	name, changed := c.pick.current()
	conn, err := c.dialCarrier(ctx, name)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	var mopts []mux.Option
	if carrierID(name) == proto.CarrierHTTP {
		mopts = append(mopts, mux.SlowCarrier())
	}
	ms, err := mux.Dialer(conn, c.ping, mopts...)
	if err != nil {
		return false, err
	}
	role := c.cfg.Client.Role
	c.log.Info("session up", "slot", slot, "carrier", name, "role", role)
	go func() {
		select {
		case <-ctx.Done():
			_ = ms.Close()
		case <-changed: // a better (or another) carrier was chosen: end this session so the slot redials on it
			_ = ms.Close()
		case <-ms.CloseChan():
		}
	}()
	if c.exit != nil {
		return true, c.exit.Serve(ctx, ms)
	}
	sess := &entry.Session{
		Name: c.cfg.Client.Name, Carrier: carrierID(name),
		IP: c.cfg.Client.Server, Since: time.Now(), Mux: ms,
	}
	if err := c.hub.Add(sess); err != nil {
		return true, err
	}
	defer c.hub.Remove(sess)
	// An entry never serves streams; refuse any the server opens.
	for {
		st, err := ms.AcceptStream()
		if err != nil {
			return true, err
		}
		_ = st.Close()
	}
}

// probeLoop, while the client sits on a carrier that is not the first choice,
// tries the better ones now and then (a full handshake, then closes) and moves
// back as soon as one works.
func (c *Client) probeLoop(ctx context.Context) {
	t := time.NewTicker(autoProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, name := range c.pick.better() {
			conn, err := c.dialCarrier(ctx, name)
			if err != nil {
				c.log.Debug("probe failed", "carrier", name, "err", err)
				continue
			}
			_ = conn.Close()
			c.pick.promote(name)
			break
		}
	}
}

func carrierID(name string) byte {
	if name == "http" || name == "chunk" {
		return proto.CarrierHTTP
	}
	return proto.CarrierWS
}

func (c *Client) serveAdmin(ctx context.Context, addr string) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		c.log.Error("admin listen failed", "err", err)
		return
	}
	h := http.NewServeMux()
	h.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"role":           c.cfg.Client.Role,
			"sessions":       c.hub.Snapshot(),
			"streams_opened": c.entry.Streams.Load(),
			"stream_errors":  c.entry.StreamErr.Load(),
			"carrier_mode":   c.cfg.Client.Carrier,
			"carrier":        currentName(c.pick),
		})
	})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	_ = srv.Serve(l)
}

// OpenForTest opens a forward stream by name (entry role); for loopback tests.
func (c *Client) OpenForTest(name string) (net.Conn, byte, error) { return c.entry.OpenForTest(name) }

func currentName(p *picker) string { n, _ := p.current(); return n }
