package carrier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"hstun/internal/testutil"
)

// hello is the part of a ClientHello a DPI box can read.
type hello struct {
	ciphers []uint16
	exts    []uint16
	alpn    []string
	groups  []uint16
	sni     string
	size    int // bytes of the TLS record
}

func isGrease(v uint16) bool { return v&0x0f0f == 0x0a0a && v>>8 == v&0xff }

// captureHello dials a listener that only records the first TLS record and
// returns what the client's handshake sent.
func captureHello(t *testing.T, o Options) hello {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	rec := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			rec <- nil
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(c, hdr); err != nil {
			rec <- nil
			return
		}
		body := make([]byte, binary.BigEndian.Uint16(hdr[3:5]))
		if _, err := io.ReadFull(c, body); err != nil {
			rec <- nil
			return
		}
		rec <- append(hdr, body...)
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _, _ = o.Handshake(ctx, raw) }() // fails when the listener hangs up: only the hello matters
	b := <-rec
	_ = raw.Close()
	if b == nil {
		t.Fatal("no ClientHello received")
	}
	return parseHello(t, b)
}

func parseHello(t *testing.T, rec []byte) hello {
	t.Helper()
	h := hello{size: len(rec)}
	p := rec[5+4+2+32:] // record header, handshake header, version, random
	p = p[1+int(p[0]):] // session id
	n := int(binary.BigEndian.Uint16(p))
	for i := 0; i < n; i += 2 {
		h.ciphers = append(h.ciphers, binary.BigEndian.Uint16(p[2+i:]))
	}
	p = p[2+n:]
	p = p[1+int(p[0]):] // compression
	p = p[2:]           // extensions length
	for len(p) >= 4 {
		typ, l := binary.BigEndian.Uint16(p), int(binary.BigEndian.Uint16(p[2:]))
		data := p[4 : 4+l]
		h.exts = append(h.exts, typ)
		switch typ {
		case 0: // server_name
			if len(data) > 5 {
				h.sni = string(data[5:])
			}
		case 16: // ALPN
			for q := data[2:]; len(q) > 0; q = q[1+int(q[0]):] {
				h.alpn = append(h.alpn, string(q[1:1+int(q[0])]))
			}
		case 10: // supported_groups
			for i := 2; i+1 < len(data); i += 2 {
				h.groups = append(h.groups, binary.BigEndian.Uint16(data[i:]))
			}
		}
		p = p[4+l:]
	}
	return h
}

// ja4Ciphers is the second part of a JA4 fingerprint: the sorted cipher suites
// without GREASE, hashed. Chrome has produced the same value for years.
func (h hello) ja4Ciphers() string {
	var cs []string
	for _, c := range h.ciphers {
		if !isGrease(c) {
			cs = append(cs, fmt.Sprintf("%04x", c))
		}
	}
	sort.Strings(cs)
	sum := sha256.Sum256([]byte(strings.Join(cs, ",")))
	return hex.EncodeToString(sum[:])[:12]
}

func (h hello) has(ext uint16) bool {
	for _, e := range h.exts {
		if e == ext {
			return true
		}
	}
	return false
}

func (h hello) grease() bool {
	for _, c := range h.ciphers {
		if isGrease(c) {
			return true
		}
	}
	return false
}

func (h hello) group(g uint16) bool {
	for _, x := range h.groups {
		if x == g {
			return true
		}
	}
	return false
}

func opts(t *testing.T, fp, sni string) Options {
	t.Helper()
	tc, err := ClientTLS(sni, "system", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	return Options{TLS: tc, Fingerprint: fp, SNI: sni}
}

const chromeCiphersJA4 = "8daaf6152771" // Chrome's JA4 cipher hash

func TestChromeHelloLooksLikeChrome(t *testing.T) {
	h := captureHello(t, opts(t, "chrome", "www.example.org"))
	if got := h.ja4Ciphers(); got != chromeCiphersJA4 {
		t.Errorf("cipher suites hash %s, Chrome's is %s", got, chromeCiphersJA4)
	}
	if !h.grease() {
		t.Error("no GREASE values: a real browser always sends them")
	}
	if len(h.alpn) != 1 || h.alpn[0] != "http/1.1" {
		t.Errorf("ALPN %v, want only http/1.1 (a browser's WebSocket connection)", h.alpn)
	}
	if h.has(17613) || h.has(17513) {
		t.Error("ALPS extension present although h2 is not offered")
	}
	if !h.group(0x11ec) {
		t.Errorf("supported groups %x lack X25519MLKEM768", h.groups)
	}
	if h.sni != "www.example.org" {
		t.Errorf("SNI %q", h.sni)
	}
	if h.has(41) {
		t.Error("pre_shared_key present on a first connection (no ticket to offer)")
	}
	if h.size < 1000 {
		t.Errorf("ClientHello is only %d bytes: Chrome's is about 1.5 KB with the post-quantum share", h.size)
	}
	for _, e := range []uint16{23, 43, 51, 45, 27, 65037} { // extended_master_secret, supported_versions, key_share, psk_kex_modes, compress_certificate, ECH
		if !h.has(e) {
			t.Errorf("extension %d missing", e)
		}
	}
}

// A browser resumes sessions: the first connection has no pre_shared_key
// extension (there is no ticket), a later one has it.
func TestChromeResumesWithATicket(t *testing.T) {
	certPath, keyPath := testutil.Cert(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	var resumed []bool
	var mu sync.Mutex
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tc := c.(*tls.Conn)
				if tc.Handshake() == nil {
					mu.Lock()
					resumed = append(resumed, tc.ConnectionState().DidResume)
					mu.Unlock()
					_, _ = io.Copy(io.Discard, c)
				}
			}()
		}
	}()
	tc, err := ClientTLS("localhost", "ca_file", certPath, "", false)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{TLS: tc, Fingerprint: "chrome"}
	for i := 0; i < 2; i++ {
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c, err := o.Handshake(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		// TLS 1.3 tickets arrive after the handshake: read until the server has sent them.
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_, _ = c.Read(make([]byte, 1))
		_ = c.Close()
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(resumed) != 2 || resumed[0] || !resumed[1] {
		t.Fatalf("resumption (first, second) = %v, want [false true]", resumed)
	}
}

func TestChromeNoPQHelloHasNoPostQuantumShare(t *testing.T) {
	h := captureHello(t, opts(t, "chrome_nopq", "www.example.org"))
	if h.group(0x11ec) || h.group(0x6399) {
		t.Errorf("post-quantum group offered: %x", h.groups)
	}
	if h.ja4Ciphers() != chromeCiphersJA4 {
		t.Errorf("cipher hash %s", h.ja4Ciphers())
	}
	if h.size > 700 {
		t.Errorf("ClientHello is %d bytes, should be small without the PQ share", h.size)
	}
}

func TestGoHelloIsNotChrome(t *testing.T) {
	h := captureHello(t, opts(t, "go", "www.example.org"))
	if h.ja4Ciphers() == chromeCiphersJA4 {
		t.Error("Go's hello has Chrome's cipher hash; the test would prove nothing")
	}
	if h.grease() {
		t.Error("Go sends no GREASE")
	}
}

func TestDefaultFingerprintIsChrome(t *testing.T) {
	h := captureHello(t, opts(t, "", "www.example.org"))
	if h.ja4Ciphers() != chromeCiphersJA4 || !h.grease() {
		t.Error("an empty tls_fingerprint must mean the Chrome profile")
	}
}

func TestOtherProfilesDifferFromChromeAndOfferHTTP11(t *testing.T) {
	seen := map[string]string{}
	for _, fp := range []string{"firefox", "safari", "ios", "android"} {
		h := captureHello(t, opts(t, fp, "www.example.org"))
		// uTLS's OkHttp profile sends no ALPN at all; the others offer http/1.1 only.
		if fp != "android" && (len(h.alpn) != 1 || h.alpn[0] != "http/1.1") {
			t.Errorf("%s: ALPN %v", fp, h.alpn)
		}
		if h.ja4Ciphers() == chromeCiphersJA4 {
			t.Errorf("%s has Chrome's cipher hash", fp)
		}
		if prev, dup := seen[h.ja4Ciphers()]; dup && fp != "ios" && prev != "safari" {
			t.Errorf("%s and %s have the same cipher hash", fp, prev)
		}
		seen[h.ja4Ciphers()] = fp
	}
}

func TestNoSNIForAnIPAddress(t *testing.T) {
	h := captureHello(t, opts(t, "chrome", "127.0.0.1"))
	if h.sni != "" || h.has(0) && h.sni != "" {
		t.Errorf("SNI %q sent for an IP address; browsers send none", h.sni)
	}
}

func TestUnknownFingerprintRefused(t *testing.T) {
	if ValidFingerprint("netscape") {
		t.Fatal("netscape accepted")
	}
	o := opts(t, "netscape", "x")
	raw, _ := net.Pipe()
	if _, err := o.Handshake(context.Background(), raw); err == nil {
		t.Fatal("handshake with an unknown fingerprint")
	}
}

func TestUserAgentFollowsTheProfile(t *testing.T) {
	cases := map[string]string{"": "Chrome/133", "chrome": "Chrome/133", "chrome_nopq": "Chrome/120", "firefox": "Firefox/148", "safari": "Safari/605", "go": "Chrome/"}
	for fp, want := range cases {
		if ua := (Options{Fingerprint: fp}).UA(); !strings.Contains(ua, want) {
			t.Errorf("%q: UA %q lacks %q", fp, ua, want)
		}
	}
	if ua := (Options{Fingerprint: "firefox", UserAgent: "mine"}).UA(); ua != "mine" {
		t.Errorf("a configured user agent must win, got %q", ua)
	}
}

// Every profile must complete a real handshake with a Go server that offers
// only http/1.1, with verification on.
func TestEveryProfileHandshakesAndVerifies(t *testing.T) {
	certPath, keyPath := testutil.Cert(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 4)
				if _, err := io.ReadFull(c, b); err == nil {
					_, _ = c.Write(b)
				}
			}()
		}
	}()
	spki := func() string {
		pemBytes, _ := os.ReadFile(certPath)
		blk, _ := pem.Decode(pemBytes)
		leaf, _ := x509.ParseCertificate(blk.Bytes)
		sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		return hex.EncodeToString(sum[:])
	}()
	dial := func(o Options) error {
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return err
		}
		defer raw.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := o.Handshake(ctx, raw)
		if err != nil {
			return err
		}
		if _, err := c.Write([]byte("ping")); err != nil {
			return err
		}
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
			return fmt.Errorf("echo: %q %v", b, err)
		}
		return nil
	}
	for _, fp := range FingerprintNames() {
		for _, mode := range []string{"ca_file", "spki"} {
			tc, err := ClientTLS("localhost", mode, certPath, spki, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := dial(Options{TLS: tc, Fingerprint: fp}); err != nil {
				t.Errorf("%s with %s: %v", fp, mode, err)
			}
		}
		// Verification stays on: a wrong pin and a wrong CA must fail.
		bad, _ := ClientTLS("localhost", "spki", "", strings.Repeat("00", 32), false)
		if err := dial(Options{TLS: bad, Fingerprint: fp}); err == nil {
			t.Errorf("%s accepted a wrong spki pin", fp)
		}
		sys, _ := ClientTLS("localhost", "system", "", "", false)
		if err := dial(Options{TLS: sys, Fingerprint: fp}); err == nil {
			t.Errorf("%s accepted a self-signed certificate against the system roots", fp)
		}
	}
}

// recordingConn remembers every Write.
type recordingConn struct {
	net.Conn
	writes [][]byte
}

func (r *recordingConn) Write(p []byte) (int, error) {
	r.writes = append(r.writes, append([]byte(nil), p...))
	return len(p), nil
}

// records splits a byte stream into TLS records.
func records(b []byte) (types []byte, payloads [][]byte) {
	for len(b) >= 5 {
		n := int(binary.BigEndian.Uint16(b[3:5]))
		if len(b) < 5+n {
			break
		}
		types = append(types, b[0])
		payloads = append(payloads, b[5:5+n])
		b = b[5+n:]
	}
	return
}

func sampleHello() []byte {
	payload := make([]byte, 600)
	for i := range payload {
		payload[i] = byte(i)
	}
	rec := []byte{0x16, 0x03, 0x01, byte(len(payload) >> 8), byte(len(payload))}
	return append(rec, payload...)
}

func TestFragmentModesKeepThePayloadAndSplitTheHello(t *testing.T) {
	orig := sampleHello()
	for _, mode := range []string{"tcp", "record", "both"} {
		for i := 0; i < 30; i++ { // the split points are random
			rc := &recordingConn{}
			c := newFragmentConn(rc, mode)
			if _, err := c.Write(orig); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Write([]byte("second write goes through untouched")); err != nil {
				t.Fatal(err)
			}
			var all []byte
			for _, w := range rc.writes[:len(rc.writes)-1] {
				all = append(all, w...)
			}
			types, payloads := records(all)
			var joined []byte
			for _, p := range payloads {
				joined = append(joined, p...)
			}
			if !bytes.Equal(joined, orig[5:]) {
				t.Fatalf("%s: payload changed", mode)
			}
			if (mode == "record" || mode == "both") && len(payloads) < 2 {
				t.Fatalf("%s: still one record", mode)
			}
			if mode == "tcp" && (len(payloads) != 1 || len(rc.writes)-1 < 2) {
				t.Fatalf("tcp: %d records in %d writes, want 1 record in several writes", len(payloads), len(rc.writes)-1)
			}
			if mode == "both" && len(rc.writes)-1 < 2 {
				t.Fatalf("both: one write")
			}
			for _, ty := range types {
				if ty != 0x16 {
					t.Fatalf("%s: record type %x", mode, ty)
				}
			}
			if string(rc.writes[len(rc.writes)-1]) != "second write goes through untouched" {
				t.Fatalf("%s: a later write was changed", mode)
			}
		}
	}
	rc := &recordingConn{}
	if newFragmentConn(rc, "off") != net.Conn(rc) || newFragmentConn(rc, "") != net.Conn(rc) {
		t.Fatal("off must not wrap")
	}
}

// Fragmented hellos from every profile must still be accepted by a real server,
// and the SNI must not be readable from the first segment alone.
func TestFragmentedHandshakeStillCompletes(t *testing.T) {
	certPath, keyPath := testutil.Cert(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	tc, err := ClientTLS("localhost", "ca_file", certPath, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, fp := range []string{"go", "chrome", "chrome_nopq", "firefox"} {
		for _, mode := range []string{"tcp", "record", "both"} {
			for i := 0; i < 5; i++ {
				raw, err := net.Dial("tcp", ln.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				c, err := (Options{TLS: tc, Fingerprint: fp, Fragment: mode}).Handshake(ctx, raw)
				cancel()
				if err != nil {
					t.Fatalf("%s + %s fragment: %v", fp, mode, err)
				}
				c.Close()
			}
		}
	}
}

func TestChunkTuningShrinksAndGrows(t *testing.T) {
	tu := NewChunkTuning(12<<10, 32<<10)
	up, down := tu.Sizes()
	if up != 12<<10 || down != 32<<10 {
		t.Fatalf("starts at %d/%d", up, down)
	}
	for i := 0; i < 30; i++ {
		tu.Shrink()
	}
	up, down = tu.Sizes()
	if up != 2<<10 || down != 4<<10 {
		t.Fatalf("floor is %d/%d, want 2048/4096", up, down)
	}
	for i := 0; i < 200; i++ {
		tu.Grow()
	}
	up, down = tu.Sizes()
	if up != 12<<10 || down != 32<<10 {
		t.Fatalf("growth must stop at the configured sizes, got %d/%d", up, down)
	}
}
