// Package carrier holds what is shared by the WebSocket and HTTP carriers:
// the client TLS configuration and the dial options.
package carrier

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Options describe how a client reaches the server.
type Options struct {
	Server   string // host:port used in URLs and the Host header
	Connect  string // optional ip:port to dial instead of Server
	SNI      string // TLS server name; defaults to the host of Server
	WSPath   string
	HTTPPath string
	TLS      *tls.Config
	// Fingerprint is the ClientHello profile (see fingerprint.go); empty means DefaultFingerprint.
	Fingerprint string
	// Fragment splits the ClientHello (see fragment.go); empty means off.
	Fragment string
	// NoCookies makes the http carrier send its metadata in the old X-Hs-* headers
	// (a hello without the cookie marker). For tests of old-server behaviour.
	NoCookies bool
	// Tuning adapts the chunk sizes between sessions (chunk mode); nil keeps them fixed.
	Tuning *ChunkTuning
	// UserAgent is sent on every request; empty means the one that matches Fingerprint (see UA).
	UserAgent string

	// Chunk selects the short-connection mode of the http carrier: every
	// request uses its own TCP/TLS connection and carries at most ChunkUp
	// bytes upstream and ChunkDown bytes downstream, with ChunkPar uploads and
	// downloads in flight. It exists for paths that cut any connection after a
	// few tens of kilobytes (Iran to Cloudflare customer hostnames, measured
	// 2026-11-13). Zero values mean the package defaults.
	Chunk     bool
	ChunkUp   int
	ChunkDown int
	ChunkPar  int
}

// ClientTLS builds the client TLS config. Verification is never disabled:
// "system" uses the OS roots, "ca_file" a private CA, "spki" pins the leaf
// public key (the pin check is then the verification).
func ClientTLS(serverName, verify, caFile, spkiHex string, mlkem bool) (*tls.Config, error) {
	cfg := &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		// Resume sessions like any HTTPS client does: later connections carry a
		// session ticket instead of a full handshake. A resumed session was
		// already verified (or pinned) on its first full handshake.
		ClientSessionCache: tls.NewLRUClientSessionCache(64),
	}
	if !mlkem {
		// Drop the X25519MLKEM768 hybrid: its large ClientHello broke the
		// websocket upgrade on one real path.
		cfg.CurvePreferences = []tls.CurveID{tls.X25519, tls.CurveP256}
	}
	switch verify {
	case "system", "":
	case "ca_file":
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificates found", caFile)
		}
		cfg.RootCAs = pool
	case "spki":
		pin, err := hex.DecodeString(strings.ToLower(spkiHex))
		if err != nil || len(pin) != sha256.Size {
			return nil, errors.New("spki_sha256 must be 64 hex characters")
		}
		cfg.InsecureSkipVerify = true // replaced by the pin check below
		cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no server certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			if !equal(sum[:], pin) {
				return errors.New("server key does not match spki_sha256 pin")
			}
			return nil
		}
	default:
		return nil, fmt.Errorf("unknown tls_verify %q", verify)
	}
	return cfg, nil
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// DefaultUserAgent is what requests carry when `user_agent` is not set. It is
// only a header: the TLS handshake is still Go's own (see README, "Looking like
// ordinary HTTPS"). Chrome has frozen the rest of the string, so only the major
// version changes; bump it at every release to the current stable Chrome major
// (`google-chrome-stable --version`), because a months-old version is a tell.
// Last bumped 2026-11-12 (Chrome 157).
const DefaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36"

type uaTransport struct {
	rt http.RoundTripper
	o  Options
}

func (t uaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", t.o.UA())
	t.o.browserHeaders(r)
	return t.rt.RoundTrip(r)
}

// WithBrowserHeaders gives every request of rt the User-Agent and the other
// headers a browser of the configured fingerprint profile sends with a script's
// requests, so a CDN or proxy that can read them sees an ordinary client. No
// Accept-Encoding is added: the carriers carry binary bodies that must not be
// re-encoded on the way.
func WithBrowserHeaders(rt http.RoundTripper, o Options) http.RoundTripper {
	return uaTransport{rt: rt, o: o}
}

func (o Options) browserHeaders(r *http.Request) {
	fp := o.Fingerprint
	if fp == "" {
		fp = DefaultFingerprint
	}
	h := r.Header
	set := func(k, v string) {
		if h.Get(k) == "" {
			h.Set(k, v)
		}
	}
	if fp == FingerprintGo {
		return // the old behaviour: only the User-Agent
	}
	ws := strings.EqualFold(h.Get("Upgrade"), "websocket")
	set("Accept", "*/*")
	if fp == FingerprintFirefox {
		set("Accept-Language", "en-US,en;q=0.5")
	} else {
		set("Accept-Language", "en-US,en;q=0.9")
	}
	if fp == FingerprintAndroid {
		return
	}
	set("Origin", "https://"+r.URL.Host)
	if ws {
		set("Sec-Fetch-Dest", "websocket")
		set("Sec-Fetch-Mode", "websocket")
	} else {
		set("Sec-Fetch-Dest", "empty")
		set("Sec-Fetch-Mode", "cors")
	}
	set("Sec-Fetch-Site", "same-origin")
	if fp == FingerprintChrome || fp == FingerprintChromeN {
		v := "133"
		if fp == FingerprintChromeN {
			v = "120"
		}
		set("Sec-Ch-Ua", `"Not(A:Brand";v="99", "Google Chrome";v="`+v+`", "Chromium";v="`+v+`"`)
		set("Sec-Ch-Ua-Mobile", "?0")
		set("Sec-Ch-Ua-Platform", `"Linux"`)
	}
}
