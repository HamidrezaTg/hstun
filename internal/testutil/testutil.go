// Package testutil has the fixtures for the loopback tests: a throwaway TLS
// certificate, TCP backends, and a CDN-like TLS-terminating reverse proxy.
package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Cert writes a self-signed ECDSA certificate for localhost and 127.0.0.1 and
// returns the cert path and key path (the cert doubles as its own CA file).
func Cert(t testing.TB) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// Echo starts a TCP echo server and returns its address.
func Echo(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().String()
}

// CDNProxy starts a TLS-terminating HTTP/1.1 reverse proxy in front of the
// origin (an https host:port, certificate not verified), the way a CDN like
// Cloudflare sits in front of the tunnel server. It returns the proxy address
// and the path of its certificate.
func CDNProxy(t testing.TB, origin string) (addr, certPath string) {
	addr, certPath, _ = CDNProxyKillable(t, origin)
	return addr, certPath
}

// tracking records accepted connections so a test can sever them all.
type tracking struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *tracking) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *tracking) kill() {
	l.mu.Lock()
	for _, c := range l.conns {
		c.Close()
	}
	l.conns = nil
	l.mu.Unlock()
}

// CDNProxyKillable is CDNProxy plus a function that resets every client
// connection currently open through the proxy (a network blip).
func CDNProxyKillable(t testing.TB, origin string) (addr, certPath string, kill func()) {
	t.Helper()
	certPath, keyPath := Cert(t)
	u, _ := url.Parse("https://" + origin)
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}},
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	rp.FlushInterval = -1
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tl := &tracking{Listener: raw}
	l := tls.NewListener(tl, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	srv := &http.Server{Handler: rp}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String(), certPath, tl.kill
}

// Counters count bytes through a Forwarder, in each direction.
type Counters struct {
	ToServer   atomic.Int64
	FromServer atomic.Int64
}

// Forwarder is a plain TCP forwarder in front of target. It counts bytes and
// can delay every chunk by `delay` in each direction (so the round trip adds
// 2*delay), imitating a long-distance path.
func Forwarder(t testing.TB, target string, delay time.Duration) (string, *Counters) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	cnt := &Counters{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				s, err := net.Dial("tcp", target)
				if err != nil {
					c.Close()
					return
				}
				go pump(s, c, delay, &cnt.ToServer)
				pump(c, s, delay, &cnt.FromServer)
			}()
		}
	}()
	return l.Addr().String(), cnt
}

type delayed struct {
	at time.Time
	b  []byte
}

// pump copies src to dst, holding each chunk for delay, and counts bytes.
func pump(dst, src net.Conn, delay time.Duration, n *atomic.Int64) {
	defer dst.Close()
	defer src.Close()
	q := make(chan delayed, 4096)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for d := range q {
			if w := time.Until(d.at); w > 0 {
				time.Sleep(w)
			}
			if _, err := dst.Write(d.b); err != nil {
				for range q { // drain so the reader never blocks
				}
				return
			}
		}
	}()
	buf := make([]byte, 32*1024)
	for {
		m, err := src.Read(buf)
		if m > 0 {
			n.Add(int64(m))
			q <- delayed{at: time.Now().Add(delay), b: append([]byte(nil), buf[:m]...)}
		}
		if err != nil {
			break
		}
	}
	close(q)
	<-done
}

// BudgetProxy is a TCP forwarder that imitates a path cutting every
// connection after a byte budget per direction (Iran to a Cloudflare customer
// name: about 20 KB upstream and 48 KB downstream). Once a direction exceeds
// its budget, the rest of that connection is swallowed silently (the socket
// stays open, nothing is forwarded). cuts counts the connections that hit a
// budget.
func BudgetProxy(t testing.TB, target string, upBudget, downBudget int) (addr string, cuts *atomic.Int64) {
	t.Helper()
	return FlakyBudgetProxy(t, target, upBudget, downBudget, 0, 0)
}

// FlakyBudgetProxy is BudgetProxy that also swallows `blackholePct` percent of
// the connections from their first byte (a path that silently eats some
// connections) and adds `delay` to every chunk in each direction (round trip
// 2*delay).
func FlakyBudgetProxy(t testing.TB, target string, upBudget, downBudget, blackholePct int, delay time.Duration) (addr string, cuts *atomic.Int64) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	cuts = &atomic.Int64{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				s, err := net.Dial("tcp", target)
				if err != nil {
					c.Close()
					return
				}
				var cut atomic.Bool
				hole := blackholePct > 0 && mrand.IntN(100) < blackholePct
				budgeted := func(dst, src net.Conn, budget int) {
					defer dst.Close()
					defer src.Close()
					buf := make([]byte, 16*1024)
					sent := 0
					for {
						n, err := src.Read(buf)
						if n > 0 {
							if delay > 0 {
								time.Sleep(delay)
							}
							if hole {
								continue // swallow everything on this connection
							}
							if sent+n > budget {
								if cut.CompareAndSwap(false, true) {
									cuts.Add(1)
								}
								// swallow: keep draining so the sender never sees an error
								continue
							}
							if _, werr := dst.Write(buf[:n]); werr != nil {
								return
							}
							sent += n
						}
						if err != nil {
							return
						}
					}
				}
				go budgeted(s, c, upBudget)
				budgeted(c, s, downBudget)
			}()
		}
	}()
	return l.Addr().String(), cuts
}
