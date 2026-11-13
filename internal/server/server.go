// Package server is the listening side of the tunnel. It accepts
// authenticated carrier connections (WebSocket or HTTP) and then acts as the
// configured role on them:
//   - entry (reverse mode, default): opens the user-facing forward and SOCKS5
//     ports and carries each connection over a stream to the dialing client;
//   - exit (direct mode): serves the streams the dialing client opens, to fixed
//     local targets or through the SOCKS exit.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"hstun/internal/auth"
	"hstun/internal/carrier/httpmode"
	"hstun/internal/carrier/ws"
	"hstun/internal/config"
	"hstun/internal/decoy"
	"hstun/internal/entry"
	"hstun/internal/exit"
	"hstun/internal/mux"
	"hstun/internal/netx"
	"hstun/internal/proto"
	"hstun/internal/seal"
)

type Server struct {
	cfg  *config.ServerFile
	log  *slog.Logger
	kr   *auth.Keyring
	hub  *entry.Hub
	fail *failLimiter
	cf   ranges
	// proxyHeader is the header that carries the real client IP when the TCP
	// peer is inside cf.
	proxyHeader string
	idle        time.Duration

	entry *entry.Entry // role entry
	exit  *exit.Exit   // role exit
	ctx   context.Context

	mainLn  net.Listener
	adminLn net.Listener

	httpSrv *httpmode.Server

	certs   *certReloader // nil when serving plain HTTP behind a local proxy
	started time.Time

	// counters for /stats
	hsOK, hsFail atomic.Int64
}

func New(cfg *config.ServerFile, logger *slog.Logger) (*Server, error) {
	var keys [][]byte
	for _, p := range cfg.Server.SecretFiles {
		k, err := auth.LoadKeyFile(p)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	kr, err := auth.NewKeyring(keys...)
	if err != nil {
		return nil, err
	}
	idle, _ := time.ParseDuration(cfg.Server.IdleTimeout)
	s := &Server{
		cfg:     cfg,
		log:     logger,
		started: time.Now(),
		kr:      kr,
		hub:     entry.NewHub(cfg.Server.MaxSessionsPerKey, cfg.Server.MaxStreamsPerSession),
		fail:    newFailLimiter(cfg.Server.AuthFailPerMinPerIP),
		idle:    idle,
		ctx:     context.Background(),
	}
	if cfg.Server.Role == config.RoleExit {
		s.exit = exit.New(cfg.Forward, cfg.SocksExit, idle, cfg.Server.MaxStreamsPerSession)
	} else {
		s.entry = entry.New(s.hub, logger, idle, cfg.Forward, cfg.Socks, cfg.Server.ForwardMaxConnsPerIP)
	}
	if cfg.Server.WSPath == config.DefaultWSPath || cfg.Server.HTTPPath == config.DefaultHTTPPath {
		logger.Warn("ws_path or http_path is still the default, which is public in the source repository; set your own paths")
	}
	switch cfg.Server.TrustProxy {
	case "cloudflare":
		if s.cf, err = loadRanges(cfg.Server.CFRangesFile); err != nil {
			return nil, err
		}
		s.proxyHeader = "CF-Connecting-IP"
	case "loopback":
		s.cf, s.proxyHeader = loopbackRanges(), "X-Real-IP"
	case "cloudflared":
		s.cf, s.proxyHeader = loopbackRanges(), "CF-Connecting-IP"
	}
	return s, nil
}

func (s *Server) hasCarrier(name string) bool {
	for _, c := range s.cfg.Server.Carriers {
		if c == name {
			return true
		}
	}
	return false
}

// Listen binds every listener, so a busy port is reported before serving.
func (s *Server) Listen() error {
	var tc *tls.Config // nil: plain HTTP on loopback behind a local proxy (trust_proxy = "loopback")
	if s.cfg.Server.TLSCert != "" {
		certs, err := newCertReloader(s.cfg.Server.TLSCert, s.cfg.Server.TLSKey, s.log, certCheckEvery)
		if err != nil {
			return err
		}
		s.certs = certs
		tc = &tls.Config{
			GetCertificate: certs.get, // a renewed certificate is picked up without a restart
			MinVersion:     tls.VersionTLS12,
			NextProtos:     []string{"http/1.1"},
		}
	}
	raw, err := net.Listen("tcp", s.cfg.Server.Listen)
	if err != nil {
		return err
	}
	guard, err := newGuardListener(raw, s.cfg.Server.AllowFrom, s.cfg.Server.MaxConnsPerIP, s.cf)
	if err != nil {
		_ = raw.Close()
		return err
	}
	lim := netx.NewLimitListener(netx.TunedListener{Listener: guard}, s.cfg.Server.MaxConns)
	if tc != nil {
		s.mainLn = tls.NewListener(lim, tc)
	} else {
		s.mainLn = lim
	}

	if s.entry != nil {
		if err := s.entry.Listen(); err != nil {
			_ = s.mainLn.Close()
			return err
		}
	}
	if s.cfg.Server.AdminListen != "" {
		if s.adminLn, err = net.Listen("tcp", s.cfg.Server.AdminListen); err != nil {
			return fmt.Errorf("admin: %w", err)
		}
	}
	return nil
}

// Addr is the tunnel listener address.
func (s *Server) Addr() net.Addr { return s.mainLn.Addr() }

// ForwardAddr returns the address of a named forward listener (entry role).
func (s *Server) ForwardAddr(name string) net.Addr { return s.entry.ForwardAddr(name) }

// SocksAddr returns the SOCKS5 listener address, or nil (entry role).
func (s *Server) SocksAddr() net.Addr { return s.entry.SocksAddr() }

// Serve runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	s.ctx = ctx
	mux := http.NewServeMux()
	if s.hasCarrier("ws") {
		mux.Handle(s.cfg.Server.WSPath, ws.Handler(s.onWS))
	}
	if s.hasCarrier("http") {
		s.httpSrv = httpmode.NewServer(s.kr, httpmode.Hooks{
			OnSession: s.onHTTPSession,
			IP:        s.clientIPOf,
			Allow:     s.fail.allow,
			Fail:      s.fail.fail,
		}, s.log)
		mux.Handle(s.cfg.Server.HTTPPath+"/", http.StripPrefix(s.cfg.Server.HTTPPath, s.httpSrv.Handler()))
	}
	mux.Handle("/", decoy.Handler(s.cfg.Server.DecoyDir))
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout also bounds the TLS handshake (net/http applies it there);
		// without it a silent TCP connection could hold a slot forever. A
		// hijacked websocket gets its deadlines cleared by net/http.
		ReadTimeout:    20 * time.Second,
		WriteTimeout:   20 * time.Second,
		MaxHeaderBytes: 8 << 10,
		IdleTimeout:    120 * time.Second,
		ErrorLog:       log.New(io.Discard, "", 0),
		ConnState:      func(c net.Conn, st http.ConnState) {},
	}
	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	run(func() {
		if err := srv.Serve(s.mainLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("tunnel listener stopped", "err", err)
		}
	})
	if s.entry != nil {
		run(func() { s.entry.Serve(ctx) })
	}
	if s.adminLn != nil {
		run(func() {
			h := http.NewServeMux()
			h.HandleFunc("/stats", s.stats)
			as := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
			go func() { <-ctx.Done(); _ = as.Close() }()
			_ = as.Serve(s.adminLn)
		})
	}
	s.log.Info("server ready", "role", s.cfg.Server.Role, "tunnel", s.mainLn.Addr().String(), "carriers", s.cfg.Server.Carriers)
	<-ctx.Done()
	_ = srv.Close()
	_ = s.mainLn.Close()
	wg.Wait()
	return nil
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{
		"role":           s.cfg.Server.Role,
		"sessions":       s.hub.Snapshot(),
		"handshake_ok":   s.hsOK.Load(),
		"handshake_fail": s.hsFail.Load(),
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
	}
	if s.certs != nil {
		out["cert_not_after"] = s.certs.expiry().UTC().Format(time.RFC3339)
	}
	if s.entry != nil {
		out["streams_opened"] = s.entry.Streams.Load()
		out["stream_errors"] = s.entry.StreamErr.Load()
	}
	_ = json.NewEncoder(w).Encode(out)
}

// clientIPOf returns the real source IP: the proxy header (CF-Connecting-IP, or
// X-Real-IP behind a loopback proxy) only when the TCP peer is inside the
// trusted ranges, otherwise the peer itself.
func (s *Server) clientIPOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.cf != nil {
		if peer := net.ParseIP(host); peer != nil && s.cf.contains(peer) {
			hdr := s.proxyHeader
			if hdr == "" {
				hdr = "CF-Connecting-IP"
			}
			if real := net.ParseIP(r.Header.Get(hdr)); real != nil {
				return real.String()
			}
		}
	}
	return host
}

func (s *Server) onWS(c net.Conn, r *http.Request) {
	ip := s.clientIPOf(r)
	if !s.fail.allow(ip) {
		_ = c.Close()
		return
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	res, err := proto.ServerHandshake(c, s.kr)
	if err != nil {
		s.hsFail.Add(1)
		s.fail.fail(ip)
		s.log.Warn("handshake failed", "ip", ip, "carrier", "ws", "reason", err.Error())
		time.Sleep(proto.RejectDelay())
		_ = c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	s.runSession(c, res, ip)
}

func (s *Server) onHTTPSession(c net.Conn, res *proto.ServerResult, ip string) {
	s.runSession(c, res, ip)
}

// runSession owns an authenticated carrier conn until the mux session ends.
func (s *Server) runSession(c net.Conn, res *proto.ServerResult, ip string) {
	defer c.Close()
	sealed, err := seal.Wrap(c, res.SessionKey, false)
	if err != nil {
		s.log.Error("seal setup failed", "err", err)
		return
	}
	var mopts []mux.Option
	if res.Carrier == proto.CarrierHTTP {
		mopts = append(mopts, mux.SlowCarrier())
	}
	ms, err := mux.Listener(sealed, 25*time.Second, mopts...)
	if err != nil {
		s.log.Error("mux setup failed", "err", err)
		return
	}
	sess := &entry.Session{Name: res.Name, KeyID: res.KeyID, Carrier: res.Carrier, IP: ip, Since: time.Now(), Mux: ms}
	if err := s.hub.Add(sess); err != nil {
		s.log.Warn("session refused", "ip", ip, "name", res.Name, "err", err)
		_ = ms.Close()
		return
	}
	s.hsOK.Add(1)
	s.log.Info("session up", "name", res.Name, "ip", ip, "role", s.cfg.Server.Role, "sessions", s.hub.Count())
	if s.exit != nil {
		go func() { _ = s.exit.Serve(s.ctx, ms) }()
	} else {
		go refuseStreams(ms)
	}
	<-ms.CloseChan()
	s.hub.Remove(sess)
	s.log.Info("session down", "name", res.Name, "sessions", s.hub.Count())
}

// refuseStreams closes every stream the peer opens: an entry never serves.
func refuseStreams(ms interface {
	AcceptStream() (*yamux.Stream, error)
}) {
	for {
		st, err := ms.AcceptStream()
		if err != nil {
			return
		}
		_ = st.Close()
	}
}

// Sessions returns the number of live tunnel sessions.
func (s *Server) Sessions() int { return s.hub.Count() }

// SessionList describes the live sessions (for tests and tools).
func (s *Server) SessionList() []entry.SessionInfo { return s.hub.Snapshot() }

// OpenForTest opens a forward stream by name (entry role); for loopback tests.
func (s *Server) OpenForTest(name string) (net.Conn, byte, error) { return s.entry.OpenForTest(name) }
