package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeyFilePermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret")
	k, _ := Generate()
	if err := WriteKeyFile(p, k); err != nil {
		t.Fatal(err)
	}
	got, err := LoadKeyFile(p)
	if err != nil || string(got) != string(k) {
		t.Fatalf("load: %v", err)
	}
	if err := WriteKeyFile(p, k); err == nil {
		t.Fatal("overwrote an existing key file")
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(p); err == nil {
		t.Fatal("loaded a world-readable key")
	}
}

func TestKeyringDuplicateAndLookup(t *testing.T) {
	k1, _ := Generate()
	kr, err := NewKeyring(k1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kr.Get(KeyID(k1)); !ok {
		t.Fatal("key not found")
	}
	if _, err := NewKeyring(k1, k1); err == nil {
		t.Fatal("duplicate key ids accepted")
	}
	if _, err := NewKeyring(); err == nil {
		t.Fatal("empty keyring accepted")
	}
}

func TestMacKnownBehaviour(t *testing.T) {
	k := make([]byte, 32)
	a := Mac(k, "x", []byte("a"), []byte("b"))
	if !Equal(a, Mac(k, "x", []byte("a"), []byte("b"))) {
		t.Fatal("mac not deterministic")
	}
	if Equal(a, Mac(k, "y", []byte("a"), []byte("b"))) {
		t.Fatal("labels do not separate domains")
	}
	// RFC 4231 test case 2 for HMAC-SHA256 (key "Jefe") via an empty label would
	// not apply here, so only check output length and sensitivity.
	if len(a) != 32 {
		t.Fatalf("len %d", len(a))
	}
}
