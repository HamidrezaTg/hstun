package socks5

import (
	"bytes"
	"testing"
)

type rw struct {
	*bytes.Reader
	out bytes.Buffer
}

func (r *rw) Write(p []byte) (int, error) { return r.out.Write(p) }

func req(b ...byte) *rw { return &rw{Reader: bytes.NewReader(b)} }

func TestConnectDomain(t *testing.T) {
	c := req(append([]byte{5, 1, 0, 5, 1, 0, 3, 11}, append([]byte("example.com"), 0x01, 0xBB)...)...)
	got, err := ReadRequest(c)
	if err != nil || got != "example.com:443" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestConnectIPv4AndIPv6(t *testing.T) {
	got, err := ReadRequest(req(5, 1, 0, 5, 1, 0, 1, 1, 2, 3, 4, 0, 80))
	if err != nil || got != "1.2.3.4:80" {
		t.Fatalf("v4 got %q %v", got, err)
	}
	v6 := append([]byte{5, 1, 0, 5, 1, 0, 4}, append(make([]byte, 15), 1, 0x1F, 0x90)...)
	got, err = ReadRequest(req(v6...))
	if err != nil || got != "[::1]:8080" {
		t.Fatalf("v6 got %q %v", got, err)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string][]byte{
		"socks4":        {4, 1, 0, 80},
		"no no-auth":    {5, 1, 2},
		"bind command":  {5, 1, 0, 5, 2, 0, 1, 1, 2, 3, 4, 0, 80},
		"udp associate": {5, 1, 0, 5, 3, 0, 1, 1, 2, 3, 4, 0, 80},
		"unknown atyp":  {5, 1, 0, 5, 1, 0, 9},
		"empty domain":  {5, 1, 0, 5, 1, 0, 3, 0, 0, 80},
		"port zero":     {5, 1, 0, 5, 1, 0, 1, 1, 2, 3, 4, 0, 0},
		"truncated":     {5, 1, 0, 5, 1, 0, 3, 11, 'a'},
		"zero methods":  {5, 0},
	}
	for name, b := range cases {
		if _, err := ReadRequest(req(b...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
