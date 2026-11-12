package decoy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestBuiltInSite(t *testing.T) {
	h := Handler("")
	if r := get(h, "GET", "/"); r.Code != 200 || !strings.Contains(r.Body.String(), "<h1>") {
		t.Fatalf("index: %d %q", r.Code, r.Body.String())
	}
	for _, p := range []string{"/hs/ws", "/anything", "/.git/config", "/a/../../etc/passwd"} {
		r := get(h, "GET", p)
		if r.Code != 404 || r.Body.String() != notFoundPage {
			t.Fatalf("%s: %d %q", p, r.Code, r.Body.String())
		}
	}
	if r := get(h, "POST", "/"); r.Code != 404 {
		t.Fatalf("POST /: %d", r.Code)
	}
	for _, r := range []*httptest.ResponseRecorder{get(h, "GET", "/"), get(h, "GET", "/nope")} {
		if r.Header().Get("Server") != "" {
			t.Fatalf("Server header leaked: %q", r.Header().Get("Server"))
		}
	}
}

func TestStaticDir(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>blog</h1>"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".secret"), []byte("x"), 0o644))
	must(os.Mkdir(filepath.Join(dir, "empty"), 0o755))
	must(os.Mkdir(filepath.Join(dir, "blog"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "blog", "index.html"), []byte("<h1>post</h1>"), 0o644))
	h := Handler(dir)
	if r := get(h, "GET", "/"); r.Code != 200 || !strings.Contains(r.Body.String(), "blog") {
		t.Fatalf("/: %d %q", r.Code, r.Body.String())
	}
	if r := get(h, "GET", "/a.txt"); r.Code != 200 || r.Body.String() != "hello" {
		t.Fatalf("/a.txt: %d %q", r.Code, r.Body.String())
	}
	if r := get(h, "GET", "/blog/"); r.Code != 200 || !strings.Contains(r.Body.String(), "post") {
		t.Fatalf("/blog/: %d %q", r.Code, r.Body.String())
	}
	for _, p := range []string{"/empty/", "/empty", "/missing", "/.secret", "/../etc/passwd"} {
		r := get(h, "GET", p)
		if r.Code != 404 || r.Body.String() != notFoundPage {
			t.Fatalf("%s: %d %q (want the standard 404, no listing)", p, r.Code, r.Body.String())
		}
	}
	if r := get(h, "HEAD", "/a.txt"); r.Code != 200 || r.Body.Len() != 0 {
		t.Fatalf("HEAD: %d len %d", r.Code, r.Body.Len())
	}
}

func TestCheckDir(t *testing.T) {
	if CheckDir(t.TempDir()) != nil {
		t.Fatal("dir should pass")
	}
	f := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(f, nil, 0o644)
	if CheckDir(f) == nil || CheckDir(filepath.Join(f, "nope")) == nil {
		t.Fatal("file and missing path must fail")
	}
}
