package e2e

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"hstun/internal/testutil"
)

// tapProxy is a TLS-terminating relay (like a CDN) that records every
// decrypted byte it forwards, in both directions.
type tapProxy struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (p *tapProxy) record(b []byte) {
	p.mu.Lock()
	p.buf.Write(b)
	p.mu.Unlock()
}

func (p *tapProxy) seen() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.buf.Bytes()...)
}

func startTap(t *testing.T, origin string) (*tapProxy, string, string) {
	t.Helper()
	certPath, keyPath := testutil.Cert(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	p := &tapProxy{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				o, err := tls.Dial("tcp", origin, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
				if err != nil {
					return
				}
				defer o.Close()
				cp := func(dst, src net.Conn) {
					buf := make([]byte, 32<<10)
					for {
						n, err := src.Read(buf)
						if n > 0 {
							p.record(buf[:n])
							if _, werr := dst.Write(buf[:n]); werr != nil {
								return
							}
						}
						if err != nil {
							_ = dst.Close()
							return
						}
					}
				}
				go cp(o, c)
				cp(c, o)
			}()
		}
	}()
	return p, l.Addr().String(), certPath
}

// A TLS-terminating middlebox sees the HTTP upgrade and the handshake, but
// never the payload: it is sealed end to end.
func TestMiddleboxCannotReadPayload(t *testing.T) {
	marker := bytes.Repeat([]byte("PLAINTEXT-MARKER-0123456789 "), 40)
	for _, mode := range []string{"reverse", "direct"} {
		for _, carrierName := range []string{"ws", "http"} {
			t.Run(mode+"/"+carrierName, func(t *testing.T) {
				var fwd string
				var tap *tapProxy
				if mode == "reverse" {
					e := startServer(t, []string{carrierName})
					var addr, cert string
					tap, addr, cert = startTap(t, e.tunnel)
					e.startClient(t, carrierName, 1, addr, cert)
					fwd = e.fwd
				} else {
					d := startDirectServer(t, []string{carrierName}, false)
					var addr, cert string
					tap, addr, cert = startTap(t, d.tunnel)
					d.startClient(t, carrierName, 1, addr, cert)
					fwd = d.fwd
				}
				c, err := net.DialTimeout("tcp", fwd, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(15 * time.Second))
				if _, err := c.Write(marker); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(marker))
				if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, marker) {
					t.Fatalf("echo through the tap failed: %v", err)
				}
				seen := tap.seen()
				if len(seen) < 2*len(marker) {
					t.Fatalf("tap saw only %d bytes, it is not in the path", len(seen))
				}
				if bytes.Contains(seen, []byte("PLAINTEXT-MARKER")) {
					t.Fatal("middlebox saw the payload in clear")
				}
			})
		}
	}
}
