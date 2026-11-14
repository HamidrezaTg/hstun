package e2e

import (
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hstun/internal/config"
)

// To anyone who is not the tunnel, the server is a small static website: the
// same 404 on every unknown path, wrong method and rejected tunnel request, the
// site from decoy_dir on "/", and no Go or hstun identifiers anywhere.
func TestServerLooksLikeAWebsite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>my notes</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := startServerOpt(t, []string{"ws", "http"}, func(c *config.ServerFile) { c.Server.DecoyDir = dir })

	hc := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true},
	}
	do := func(method, path, body string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(method, "https://"+e.tunnel+path, strings.NewReader(body))
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	if code, body, _ := do("GET", "/", ""); code != 200 || !strings.Contains(body, "my notes") {
		t.Fatalf("/ : %d %q", code, body)
	}
	_, ref, _ := do("GET", "/definitely-not-here", "")
	if !strings.Contains(ref, "Not Found") {
		t.Fatalf("reference 404 page unexpected: %q", ref)
	}
	probes := []struct{ method, path, body string }{
		{"GET", "/hs/ws", ""},   // websocket path without an upgrade
		{"POST", "/hs/ws", "x"}, // wrong method on it
		{"GET", "/hs/h/o", ""},  // http carrier endpoints, wrong method
		{"GET", "/hs/h/a", ""},
		{"GET", "/hs/h/u", ""},
		{"GET", "/hs/h/d", ""},
		{"POST", "/hs/h/o", strings.Repeat("A", 70)}, // garbage hello
		{"POST", "/", "x"},
		{"GET", "/.git/config", ""},
	}
	for _, p := range probes {
		code, body, h := do(p.method, p.path, p.body)
		if code != 404 || body != ref {
			t.Errorf("%s %s: %d %q, want the same 404 page as an unknown path", p.method, p.path, code, body)
		}
		if h.Get("Server") != "" {
			t.Errorf("%s %s: Server header %q", p.method, p.path, h.Get("Server"))
		}
		low := strings.ToLower(body)
		if strings.Contains(low, "hstun") || strings.Contains(low, "go-http") || strings.Contains(low, "websocket") {
			t.Errorf("%s %s: body names the stack: %q", p.method, p.path, body)
		}
	}
}

// Behind a local proxy the server may speak plain HTTP on loopback (no TLS
// certificate). It must serve the decoy and refuse the tunnel paths the same way.
func TestPlainBackendBehindLoopbackProxy(t *testing.T) {
	e := startServerOpt(t, []string{"ws"}, func(c *config.ServerFile) {
		c.Server.TLSCert, c.Server.TLSKey = "", ""
		c.Server.TrustProxy = "loopback"
	})
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := hc.Get("http://" + e.tunnel + "/")
	if err != nil {
		t.Fatalf("plain GET: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "Welcome") {
		t.Fatalf("plain backend / : %d %q", resp.StatusCode, b)
	}
	resp, err = hc.Get("http://" + e.tunnel + "/hs/ws")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("ws path without upgrade on the plain backend: %d, want 404", resp.StatusCode)
	}
	if _, err := hc.Get("https://" + e.tunnel + "/"); err == nil {
		t.Fatal("TLS handshake unexpectedly succeeded against a plain backend")
	}
}

// Someone who knows the secret http path (a CDN's log shows it) but has no session
// still gets nothing that names the tool: every error answer is an HTML error page,
// carries no X-Hs-* header and no cookie, and no plain-text one-word body.
func TestTunnelPathErrorsLookLikeAWebsite(t *testing.T) {
	e := startServerOpt(t, []string{"ws", "http"}, nil)
	hc := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true},
	}
	bogus := strings.Repeat("ab", 16) // a well-formed session or handshake id nobody issued
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	probes := []struct {
		name, path string
		h          http.Header
		body       string
	}{
		{"unknown session, old style, up", "/hs/h/u", hdr("X-Hs-Sid", bogus, "X-Hs-Seq", "1", "X-Hs-Tag", bogus), "x"},
		{"unknown session, old style, down", "/hs/h/d", hdr("X-Hs-Sid", bogus, "X-Hs-Ack", "0", "X-Hs-Tag", bogus), ""},
		{"unknown session, old style, close", "/hs/h/c", hdr("X-Hs-Sid", bogus, "X-Hs-Tag", bogus), ""},
		{"unknown handshake id", "/hs/h/a", hdr("X-Hs-Cid", bogus), strings.Repeat("p", 32)},
		{"hello of the wrong size", "/hs/h/o", nil, strings.Repeat("h", 10)},
		{"hello with a cookie marker", "/hs/h/o", hdr("Cookie", "s1234abcd="+bogus), strings.Repeat("h", 70)},
		{"oversize body", "/hs/h/u", hdr("X-Hs-Sid", bogus, "X-Hs-Seq", "1"), strings.Repeat("z", 80<<10)},
		{"cookie style, unknown names", "/hs/h/u", hdr("Cookie", "abc="+bogus+"; def="+bogus), "x"},
	}
	for _, p := range probes {
		req, _ := http.NewRequest("POST", "https://"+e.tunnel+p.path, strings.NewReader(p.body))
		for k, v := range p.h {
			req.Header[k] = v
		}
		resp, err := hc.Do(req)
		if err != nil {
			if strings.Contains(p.name, "oversize") {
				continue // the server may hang up on a huge body, as nginx can too
			}
			t.Errorf("%s: %v", p.name, err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("%s: status %d for a request without a session", p.name, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type %q, want an HTML error page (plain text would be a signature)", p.name, ct)
		}
		for k := range resp.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-hs-") || strings.EqualFold(k, "Set-Cookie") || strings.EqualFold(k, "Server") {
				t.Errorf("%s: response header %s", p.name, k)
			}
		}
		low := strings.ToLower(string(b))
		for _, w := range []string{"hstun", "go-http", "websocket", "tag", "gone", "expired", "slow down"} {
			if strings.TrimSpace(low) == w { // a bare plain-text word; an HTML page that says "Gone" is fine
				t.Errorf("%s: body %q gives away the tool", p.name, b)
			}
		}
		if !strings.Contains(low, "<html") {
			t.Errorf("%s: body is not a web page: %q", p.name, b)
		}
	}
}
