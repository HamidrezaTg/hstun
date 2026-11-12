package httpmode

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"hstun/internal/auth"
	"hstun/internal/proto"
)

// One source must not be able to hold every pending-handshake slot.
func TestHelloPendingCapPerIP(t *testing.T) {
	key, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	kr, err := auth.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	ip := "203.0.113.7"
	srv := NewServer(kr, Hooks{
		OnSession: func(_ net.Conn, _ *proto.ServerResult, _ string) {},
		IP:        func(r *http.Request) string { return ip },
		Allow:     func(string) bool { return true },
		Fail:      func(string) {},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	hello := func() int {
		_, body, err := proto.ClientBegin(key, proto.CarrierHTTP, "t")
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Post(ts.URL+pathHello, "application/octet-stream", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < maxPendingPerIP; i++ {
		if c := hello(); c != http.StatusOK {
			t.Fatalf("hello %d: status %d, want 200", i, c)
		}
	}
	if c := hello(); c != http.StatusTooManyRequests {
		t.Fatalf("hello over the per-IP cap: status %d, want 429", c)
	}
	ip = "203.0.113.8" // another source is unaffected
	if c := hello(); c != http.StatusOK {
		t.Fatalf("other source: status %d, want 200", c)
	}
}
