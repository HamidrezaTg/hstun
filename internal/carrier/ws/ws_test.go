package ws_test

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"hstun/internal/carrier"
	"hstun/internal/carrier/ws"
	"hstun/internal/testutil"
)

type seen struct {
	mu     sync.Mutex
	alpn   [][]string
	proto  []string
	resume []bool
	ua     []string
}

// The websocket client has to look like an ordinary HTTPS client on the wire:
// it offers ALPN http/1.1 (net/http would drop it for upgrades), it resumes TLS
// sessions on later connections, and it sends a browser-style User-Agent.
func TestDialLooksLikeOrdinaryHTTPSClient(t *testing.T) {
	certPath, keyPath := testutil.Cert(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	var s seen
	base := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			s.mu.Lock()
			s.alpn = append(s.alpn, h.SupportedProtos)
			s.mu.Unlock()
			return base, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := &http.Server{Handler: ws.Handler(func(c net.Conn, r *http.Request) {
		s.mu.Lock()
		s.proto = append(s.proto, r.TLS.NegotiatedProtocol)
		s.resume = append(s.resume, r.TLS.DidResume)
		s.ua = append(s.ua, r.UserAgent())
		s.mu.Unlock()
		_ = c.Close()
	})}
	go srv.Serve(l)
	defer srv.Close()

	tc, err := carrier.ClientTLS("localhost", "ca_file", certPath, "", false)
	if err != nil {
		t.Fatal(err)
	}
	o := carrier.Options{Server: l.Addr().String(), SNI: "localhost", WSPath: "/ws", TLS: tc}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := ws.Dial(ctx, o)
		cancel()
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		_ = c.Close()
		time.Sleep(200 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.alpn) != 2 || len(s.proto) != 2 {
		t.Fatalf("expected 2 connections, got hello=%d upgrade=%d", len(s.alpn), len(s.proto))
	}
	for i := range s.alpn {
		if len(s.alpn[i]) != 1 || s.alpn[i][0] != "http/1.1" {
			t.Errorf("conn %d: ClientHello ALPN = %v, want [http/1.1]", i, s.alpn[i])
		}
		if s.proto[i] != "http/1.1" {
			t.Errorf("conn %d: negotiated %q", i, s.proto[i])
		}
		if s.ua[i] != carrier.DefaultUserAgent {
			t.Errorf("conn %d: User-Agent %q", i, s.ua[i])
		}
	}
	if s.resume[0] || !s.resume[1] {
		t.Errorf("TLS resumption: first=%v second=%v, want false then true", s.resume[0], s.resume[1])
	}
}

// A request that is not a websocket upgrade gets the ordinary 404 page, not a
// websocket-specific error that would confirm the path exists.
func TestHandlerHidesPathFromPlainRequests(t *testing.T) {
	h := ws.Handler(func(net.Conn, *http.Request) { t.Error("onConn called") })
	for _, m := range []string{"GET", "POST", "HEAD"} {
		rec := &recorder{h: http.Header{}}
		r, _ := http.NewRequest(m, "https://x/ws", nil)
		h.ServeHTTP(rec, r)
		if rec.code != 404 {
			t.Errorf("%s: status %d, want 404", m, rec.code)
		}
	}
}

type recorder struct {
	h    http.Header
	code int
}

func (r *recorder) Header() http.Header         { return r.h }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(c int)           { r.code = c }
