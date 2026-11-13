package server

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hstun/internal/testutil"
)

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func bump(t *testing.T, paths ...string) { // make the modification time differ even on coarse file systems
	t.Helper()
	at := time.Now().Add(time.Duration(len(paths)) * 3 * time.Second)
	for _, p := range paths {
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCertReloaderPicksUpRenewedCertificate(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	aCert, aKey := testutil.Cert(t)
	bCert, bKey := testutil.Cert(t)
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	copyFile(t, aCert, cert)
	copyFile(t, aKey, key)

	r, err := newCertReloader(cert, key, log, 0)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := r.get(nil)
	if again, _ := r.get(nil); again != first {
		t.Fatal("unchanged files must serve the same certificate object")
	}

	// renewed pair: new certificate is served
	copyFile(t, bCert, cert)
	copyFile(t, bKey, key)
	bump(t, cert, key)
	second, _ := r.get(nil)
	if second == first || string(second.Certificate[0]) == string(first.Certificate[0]) {
		t.Fatal("the renewed certificate was not picked up")
	}
	if r.expiry().IsZero() {
		t.Fatal("expiry not set")
	}

	// a half-written pair (certificate replaced, key still the old one) keeps the working certificate
	copyFile(t, aCert, cert)
	bump(t, cert)
	kept, _ := r.get(nil)
	if kept != second {
		t.Fatal("a certificate that does not match its key replaced the working one")
	}
	// once the key matches too, it is loaded
	copyFile(t, aKey, key)
	bump(t, cert, key)
	third, _ := r.get(nil)
	if string(third.Certificate[0]) != string(first.Certificate[0]) {
		t.Fatal("the completed pair was not loaded")
	}
}

func TestCertReloaderRejectsBadFilesAtStart(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	if _, err := newCertReloader(filepath.Join(dir, "no.pem"), filepath.Join(dir, "no.key"), log, 0); err == nil {
		t.Fatal("missing files must fail at start")
	}
}
