package e2e

import (
	"context"
	"testing"
	"time"

	"hstun/internal/client"
	"hstun/internal/config"
	"hstun/internal/testutil"
)

func probeOpts(carrier string) client.ProbeOptions {
	return client.ProbeOptions{
		Carrier: carrier, Idle: time.Second, Soak: 3 * time.Second,
		BulkDown: 1 << 20, BulkUp: 1 << 20,
	}
}

func runProbe(t *testing.T, cfg *config.ClientFile, po client.ProbeOptions) client.ProbeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return client.Probe(ctx, cfg, quiet(), po)
}

// A healthy path measures "ok" on every carrier, with the whole escalation ladder,
// the idle gap, the soak and both speeds filled in.
func TestProbeOnAHealthyPath(t *testing.T) {
	d := startDirectServer(t, []string{"ws", "http"}, false)
	for _, carrier := range []string{"ws", "http", "chunk"} {
		cfg := d.clientCfg(carrier, 1, d.tunnel, d.certPath)
		r := runProbe(t, cfg, probeOpts(carrier))
		if r.Verdict != "ok" || !r.ExitProbe {
			t.Fatalf("%s: verdict %s, %+v", carrier, r.Verdict, r)
		}
		if len(r.Steps) != len(client.DefaultSizes) || r.IdleOK == nil || !*r.IdleOK {
			t.Errorf("%s: ladder %d steps, idle %v", carrier, len(r.Steps), r.IdleOK)
		}
		if r.SoakSent < 2 || r.SoakLost != 0 {
			t.Errorf("%s: soak sent %d lost %d", carrier, r.SoakSent, r.SoakLost)
		}
		if r.DownMbit <= 0 || r.UpMbit <= 0 {
			t.Errorf("%s: speeds %.1f/%.1f", carrier, r.DownMbit, r.UpMbit)
		}
	}
}

// Both fingerprints and fragment modes can be probed by name.
func TestProbeWithFingerprintAndFragment(t *testing.T) {
	d := startDirectServer(t, []string{"ws"}, false)
	for _, fp := range []string{"go", "chrome", "firefox"} {
		for _, frag := range []string{"off", "both"} {
			po := client.ProbeOptions{Carrier: "ws", Fingerprint: fp, Fragment: frag, Sizes: []int{1 << 10, 8 << 10}}
			r := runProbe(t, d.clientCfg("ws", 1, d.tunnel, d.certPath), po)
			if r.Verdict != "ok" || r.Fingerprint != fp || r.Fragment != frag {
				t.Errorf("%s/%s: %+v", fp, frag, r)
			}
		}
	}
}

// A path that cuts every connection after a few KB is "unstable", and the step where
// it breaks is visible; an exit with probes off still gets the handshake check.
func TestProbeSeesACutPath(t *testing.T) {
	d := startDirectServer(t, []string{"ws", "http"}, false)
	proxy, _ := testutil.BudgetProxy(t, d.tunnel, 24<<10, 48<<10)
	cfg := d.clientCfg("ws", 1, proxy, d.certPath)
	r := runProbe(t, cfg, client.ProbeOptions{Carrier: "ws", Sizes: []int{1 << 10, 4 << 10, 64 << 10, 256 << 10}})
	if r.Verdict != "unstable" || !r.Connected {
		t.Fatalf("a path cutting at 24 KB gave %s: %+v", r.Verdict, r)
	}
	last := r.Steps[len(r.Steps)-1]
	if last.OK || last.Bytes != 64<<10 {
		t.Fatalf("expected the 64 KB step to be the one that fails, got %+v", r.Steps)
	}
}

func TestProbeAgainstAnExitWithoutProbeStreams(t *testing.T) {
	d := startDirectServerMut(t, []string{"ws"}, false, 256, func(c *config.ServerFile) { c.Server.DisableProbe = true })
	r := runProbe(t, d.clientCfg("ws", 1, d.tunnel, d.certPath), probeOpts("ws"))
	if r.ExitProbe || !r.Connected || !r.ForwardOK || r.Verdict != "ok" {
		t.Fatalf("exit with probes off: %+v", r)
	}
}

func TestProbeBlockedWhenNothingConnects(t *testing.T) {
	d := startDirectServer(t, []string{"http"}, false) // no ws on the server
	r := runProbe(t, d.clientCfg("ws", 1, d.tunnel, d.certPath), client.ProbeOptions{Carrier: "ws"})
	if r.Verdict != "blocked" || r.Connected || r.Error == "" {
		t.Fatalf("%+v", r)
	}
}
