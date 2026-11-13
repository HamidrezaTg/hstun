package server

import (
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certCheckEvery is the minimum time between looks at the certificate files.
var certCheckEvery = 30 * time.Second

// certReloader serves the TLS certificate from files and picks up a renewed
// one without a restart: when the files' modification times change it loads the
// new pair, and keeps serving the previous one if the new pair cannot be loaded
// (for example while certbot has written the certificate but not yet the key).
type certReloader struct {
	certFile, keyFile string
	log               *slog.Logger
	every             time.Duration

	mu              sync.Mutex
	cert            *tls.Certificate
	notAfter        time.Time
	certMod, keyMod time.Time
	badCert, badKey time.Time // the modification times of the pair that failed to load, so a broken pair is logged once
	checked         time.Time
}

func newCertReloader(certFile, keyFile string, log *slog.Logger, every time.Duration) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, log: log, every: every}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) load() error {
	cs, err := os.Stat(r.certFile)
	if err != nil {
		return err
	}
	ks, err := os.Stat(r.keyFile)
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	r.cert, r.notAfter, r.certMod, r.keyMod = &cert, leaf.NotAfter, cs.ModTime(), ks.ModTime()
	return nil
}

// get is tls.Config.GetCertificate.
func (r *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) >= r.every {
		r.checked = time.Now()
		cs, e1 := os.Stat(r.certFile)
		ks, e2 := os.Stat(r.keyFile)
		if e1 == nil && e2 == nil {
			cm, km := cs.ModTime(), ks.ModTime()
			if (!cm.Equal(r.certMod) || !km.Equal(r.keyMod)) && !(cm.Equal(r.badCert) && km.Equal(r.badKey)) {
				if err := r.load(); err != nil {
					r.badCert, r.badKey = cm, km
					r.log.Warn("the certificate files changed but cannot be loaded; still serving the previous certificate", "err", err)
				} else {
					r.log.Info("certificate reloaded", "not_after", r.notAfter.Format(time.RFC3339))
				}
			}
		}
	}
	return r.cert, nil
}

// expiry is the NotAfter of the certificate currently served.
func (r *certReloader) expiry() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.notAfter
}
