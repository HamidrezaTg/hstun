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
