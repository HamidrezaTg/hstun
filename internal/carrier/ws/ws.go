// Package ws is the WebSocket carrier: a TLS connection, an HTTP/1.1 upgrade,
// then binary messages carrying the byte stream. It is an ordinary
// WebSocket, which is what lets it pass a CDN such as Cloudflare.
package ws

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"hstun/internal/carrier"
	"hstun/internal/decoy"
	"hstun/internal/netx"
)

const (
	// MaxMessage is the largest message we write; the read limit is higher.
	MaxMessage = 32 * 1024
	ReadLimit  = 64 * 1024
)

// chunkConn splits large writes into messages of at most MaxMessage so the
// peer's read limit is never hit and no single write monopolises the pipe.
type chunkConn struct{ net.Conn }

func (c chunkConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > MaxMessage {
			n = MaxMessage
		}
		w, err := c.Conn.Write(p[:n])
		total += w
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

// wrap turns an accepted or dialed websocket into a net.Conn. It uses its own
// background context: the lifetime is the connection, not the dial call.
func wrap(c *websocket.Conn) net.Conn {
	c.SetReadLimit(ReadLimit)
	return chunkConn{websocket.NetConn(context.Background(), c, websocket.MessageBinary)}
}

// Dial opens the websocket to the server and returns the raw byte-stream conn
// (the hstun handshake is run by the caller).
func Dial(ctx context.Context, o carrier.Options) (net.Conn, error) {
	var d net.Dialer
	d.Timeout = 10 * time.Second
	d.KeepAlive = netx.KeepAlive
	tr := &http.Transport{
		// net/http drops ALPN from the ClientHello for websocket upgrades. A
		// browser always sends it, so the TLS handshake is done here instead.
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if o.Connect != "" {
				addr = o.Connect
			}
			raw, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			netx.Tune(raw)
			hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			conn, err := o.Handshake(hctx, raw)
			if err != nil {
				_ = raw.Close()
				return nil, err
			}
			return conn, nil
		},
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableCompression:  true,
	}
	hc := &http.Client{Transport: carrier.WithBrowserHeaders(tr, o)}
	c, _, err := websocket.Dial(ctx, "wss://"+o.Server+o.WSPath, &websocket.DialOptions{
		HTTPClient:      hc,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		tr.CloseIdleConnections()
		return nil, err
	}
	return wrap(c), nil
}

// Handler upgrades requests on the websocket path and hands each connection
// to onConn, which owns it from then on (handshake, mux, close).
func Handler(onConn func(c net.Conn, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Anything that is not a websocket upgrade looks like any other missing page.
		if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			decoy.NotFound(w, r)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionDisabled,
			// The tunnel is authenticated by the handshake, not by where a page came from, and
			// behind a reverse proxy the Host seen here may differ from the client's Origin
			// (nginx drops the port), which the default check would turn into a 403.
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			return // Accept already wrote the HTTP error
		}
		onConn(wrap(c), r)
	})
}
