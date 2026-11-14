package httpmode

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hstun/internal/auth"
	"hstun/internal/carrier"
	"hstun/internal/proto"
	"hstun/internal/testutil"
)

// metaRig runs a real http carrier server (echoing every session) behind a
// recorder that notes the header names of every request and response.
type metaRig struct {
	mu      sync.Mutex
	reqHdr  map[string]bool
	respHdr map[string]bool
	opts    carrier.Options
	key     []byte
	path    string
	// stripCookies drops the Cookie header before the server sees it, like a server
	// that predates the cookie style and ignores cookies.
	stripCookies bool
}

func newMetaRig(t *testing.T) *metaRig {
	t.Helper()
	key, _ := auth.Generate()
	kr, _ := auth.NewKeyring(key)
	r := &metaRig{reqHdr: map[string]bool{}, respHdr: map[string]bool{}, key: key, path: "/q7x/secret-path"}
	srv := NewServer(kr, Hooks{
		OnSession: func(c net.Conn, _ *proto.ServerResult, _ string) { defer c.Close(); _, _ = io.Copy(c, c) },
		IP:        func(*http.Request) string { return "127.0.0.1" },
		Allow:     func(string) bool { return true },
		Fail:      func(string) {},
		Path:      r.path,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	inner := http.StripPrefix(r.path, srv.Handler())
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		for k := range req.Header {
			r.reqHdr[k] = true
		}
		r.mu.Unlock()
		if r.stripCookies {
			req.Header.Del("Cookie")
		}
		inner.ServeHTTP(&recWriter{ResponseWriter: w, rig: r}, req)
	})
	certPath, keyPath := testutil.Cert(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	tc, err := carrier.ClientTLS("localhost", "ca_file", certPath, "", false)
	if err != nil {
		t.Fatal(err)
	}
	r.opts = carrier.Options{Server: strings.TrimPrefix(ts.URL, "https://"), Connect: "", SNI: "localhost", HTTPPath: r.path, TLS: tc, Fingerprint: "go"}
	return r
}

type recWriter struct {
	http.ResponseWriter
	rig  *metaRig
	done bool
}

func (w *recWriter) note() {
	if w.done {
		return
	}
	w.done = true
	w.rig.mu.Lock()
	for k := range w.Header() {
		w.rig.respHdr[k] = true
	}
	w.rig.mu.Unlock()
}
func (w *recWriter) WriteHeader(c int)           { w.note(); w.ResponseWriter.WriteHeader(c) }
func (w *recWriter) Write(b []byte) (int, error) { w.note(); return w.ResponseWriter.Write(b) }
func (w *recWriter) Flush() {
	w.note()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *metaRig) echo(t *testing.T, o carrier.Options, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _, err := Dial(ctx, o, r.key, "meta")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	want := []byte(strings.Repeat("hstun-meta-", n/11+1))[:n]
	go func() { _, _ = c.Write(want) }()
	got := make([]byte, n)
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("echo mismatch")
	}
}

func hasXHs(m map[string]bool) bool {
	for k := range m {
		if strings.HasPrefix(strings.ToLower(k), "x-hs-") {
			return true
		}
	}
	return false
}

// The default style leaves no X-Hs-* header on any request or response, and the
// cookie names are not the same on two different secret paths.
func TestMetadataTravelsInCookies(t *testing.T) {
	r := newMetaRig(t)
	r.echo(t, r.opts, 300<<10)
	r.mu.Lock()
	defer r.mu.Unlock()
	if hasXHs(r.reqHdr) || hasXHs(r.respHdr) {
		t.Fatalf("X-Hs-* headers still on the wire: request %v response %v", r.reqHdr, r.respHdr)
	}
	if !r.reqHdr["Cookie"] || !r.respHdr["Set-Cookie"] {
		t.Fatalf("no cookies used: request %v response %v", r.reqHdr, r.respHdr)
	}
	if namesFor("/a") == namesFor("/b") {
		t.Fatal("cookie names do not depend on the secret path")
	}
}

// A client that sends no cookie marker (an old client) still works against the new
// server, in the old style.
func TestOldStyleClientStillWorks(t *testing.T) {
	r := newMetaRig(t)
	o := r.opts
	o.NoCookies = true
	r.echo(t, o, 200<<10)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !hasXHs(r.reqHdr) {
		t.Fatalf("the old style was not used: %v", r.reqHdr)
	}
}

// A new client against a server that ignores cookies (an old one) gets a legacy
// answer to its hello and carries on with the old headers.
func TestNewClientFallsBackToHeadersAgainstAnOldServer(t *testing.T) {
	r := newMetaRig(t)
	r.stripCookies = true
	r.echo(t, r.opts, 150<<10)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !hasXHs(r.reqHdr) || !r.respHdr["X-Hs-Cid"] {
		t.Fatalf("no fallback to headers: request %v response %v", r.reqHdr, r.respHdr)
	}
}

// With a browser profile the requests carry the headers that profile's browser sends.
func TestBrowserHeadersFollowTheProfile(t *testing.T) {
	for _, fp := range []string{"chrome", "firefox", "go"} {
		r := newMetaRig(t)
		o := r.opts
		o.Fingerprint = fp
		r.echo(t, o, 20<<10)
		r.mu.Lock()
		got := map[string]bool{} // a copy: the handler goroutines keep writing to the original
		for k := range r.reqHdr {
			got[k] = true
		}
		r.mu.Unlock()
		switch fp {
		case "chrome":
			for _, k := range []string{"Sec-Ch-Ua", "Sec-Ch-Ua-Platform", "Accept-Language", "Origin", "Sec-Fetch-Mode", "Accept"} {
				if !got[k] {
					t.Errorf("chrome: header %s missing (got %v)", k, got)
				}
			}
		case "firefox":
			if !got["Accept-Language"] || got["Sec-Ch-Ua"] {
				t.Errorf("firefox: wrong header set %v", got)
			}
		case "go":
			if got["Sec-Ch-Ua"] || got["Accept-Language"] {
				t.Errorf("go profile must send only the User-Agent: %v", got)
			}
		}
	}
}
