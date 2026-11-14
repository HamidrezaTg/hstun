package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServerUnknownKeyRejected(t *testing.T) {
	p := write(t, `[server]
listen="0.0.0.0:1"
tls_cert="a"
tls_key="b"
secret_files=["s"]
tls_verfy="oops"
`)
	if _, err := LoadServer(p); err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Fatalf("typo not rejected: %v", err)
	}
}

func TestServerDefaultsAndValidation(t *testing.T) {
	ok := write(t, `[server]
listen="0.0.0.0:1"
tls_cert="a"
tls_key="b"
secret_files=["s"]
`)
	f, err := LoadServer(ok)
	if err != nil || f.Server.MaxConns != 1024 || f.Server.WSPath != "/hs/ws" {
		t.Fatalf("defaults: %v %+v", err, f)
	}
	bad := map[string]string{
		"no listen":                 "[server]\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\n",
		"same paths":                "[server]\nlisten=\"0.0.0.0:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\nws_path=\"/x\"\nhttp_path=\"/x\"\n",
		"cf no ranges":              "[server]\nlisten=\"0.0.0.0:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\ntrust_proxy=\"cloudflare\"\n",
		"loopback public listen":    "[server]\nlisten=\"0.0.0.0:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\ntrust_proxy=\"loopback\"\n",
		"loopback hostname":         "[server]\nlisten=\"localhost:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\ntrust_proxy=\"loopback\"\n",
		"cloudflared public listen": "[server]\nlisten=\"0.0.0.0:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\ntrust_proxy=\"cloudflared\"\n",
		"cloudflared allow_from":    "[server]\nlisten=\"127.0.0.1:1\"\nsecret_files=[\"s\"]\ntrust_proxy=\"cloudflared\"\nallow_from=[\"10.0.0.0/8\"]\n",
		"loopback allow_from":       "[server]\nlisten=\"127.0.0.1:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\ntrust_proxy=\"loopback\"\nallow_from=[\"10.0.0.0/8\"]\n",
		"bad carrier":               "[server]\nlisten=\"0.0.0.0:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\ncarriers=[\"quic\"]\n",
	}
	for name, s := range bad {
		if _, err := LoadServer(write(t, s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTrustProxyLoopbackAccepted(t *testing.T) {
	for _, l := range []string{"127.0.0.1:24083", "[::1]:24083"} {
		for _, mode := range []string{"loopback", "cloudflared"} {
			cfg := "[server]\nlisten=\"" + l + "\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\ntrust_proxy=\"" + mode + "\"\n"
			if _, err := LoadServer(write(t, cfg)); err != nil {
				t.Errorf("%s %s rejected: %v", mode, l, err)
			}
		}
	}
}

func TestPlainBackendOnlyBehindLoopbackProxy(t *testing.T) {
	ok := "[server]\nlisten=\"127.0.0.1:1\"\nsecret_files=[\"s\"]\ntrust_proxy=\"loopback\"\n"
	if _, err := LoadServer(write(t, ok)); err != nil {
		t.Errorf("plain backend behind a loopback proxy rejected: %v", err)
	}
	for name, cfg := range map[string]string{
		"no cert, no proxy":   "[server]\nlisten=\"127.0.0.1:1\"\nsecret_files=[\"s\"]\n",
		"no cert, cloudflare": "[server]\nlisten=\"127.0.0.1:1\"\nsecret_files=[\"s\"]\ntrust_proxy=\"cloudflare\"\ncf_ranges_file=\"x\"\n",
		"cert without key":    "[server]\nlisten=\"127.0.0.1:1\"\ntls_cert=\"a\"\nsecret_files=[\"s\"]\ntrust_proxy=\"loopback\"\n",
		"key without cert":    "[server]\nlisten=\"127.0.0.1:1\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\ntrust_proxy=\"loopback\"\n",
	} {
		if _, err := LoadServer(write(t, cfg)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestChunkCarrierConfig(t *testing.T) {
	base := "[client]\nserver=\"h.example:443\"\nsecret_file=\"s\"\ncarrier=\"chunk\"\n"
	if _, err := LoadClient(write(t, base+"chunk_up_bytes=12288\nchunk_down_bytes=32768\nchunk_parallel=8\n")); err != nil {
		t.Errorf("valid chunk config rejected: %v", err)
	}
	if _, err := LoadClient(write(t, base)); err != nil {
		t.Errorf("chunk with defaults rejected: %v", err)
	}
	for name, extra := range map[string]string{
		"up too small":      "chunk_up_bytes=100\n",
		"up too big":        "chunk_up_bytes=70000\n",
		"down too small":    "chunk_down_bytes=100\n",
		"down too big":      "chunk_down_bytes=200000\n",
		"parallel too many": "chunk_parallel=99\n",
		"parallel negative": "chunk_parallel=-1\n",
	} {
		if _, err := LoadClient(write(t, base+extra)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadClient(write(t, "[client]\nserver=\"h.example:443\"\nsecret_file=\"s\"\ncarrier=\"quic\"\n")); err == nil {
		t.Error("unknown carrier accepted")
	}
}

func TestClientValidation(t *testing.T) {
	good := write(t, "[client]\nserver=\"h.example:443\"\nsecret_file=\"s\"\n")
	if _, err := LoadClient(good); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"carrier":    "[client]\nserver=\"h:1\"\nsecret_file=\"s\"\ncarrier=\"tcp\"\n",
		"conns":      "[client]\nserver=\"h:1\"\nsecret_file=\"s\"\nconns=0\n",
		"verify":     "[client]\nserver=\"h:1\"\nsecret_file=\"s\"\ntls_verify=\"off\"\n",
		"spki short": "[client]\nserver=\"h:1\"\nsecret_file=\"s\"\ntls_verify=\"spki\"\nspki_sha256=\"ab\"\n",
		"no port":    "[client]\nserver=\"h\"\nsecret_file=\"s\"\n",
	}
	for name, s := range bad {
		if _, err := LoadClient(write(t, s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

const srvBase = "[server]\nlisten=\"0.0.0.0:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\n"
const cliBase = "[client]\nserver=\"h:1\"\nsecret_file=\"s\"\n"

func TestRoles(t *testing.T) {
	ok := map[string]func() error{
		"reverse server (entry)": func() error {
			_, err := LoadServer(write(t, srvBase+"[[forward]]\nname=\"x\"\nlisten=\"127.0.0.1:2\"\n[socks]\nenabled=true\nlisten=\"127.0.0.1:3\"\n"))
			return err
		},
		"reverse client (exit)": func() error {
			_, err := LoadClient(write(t, cliBase+"[[forward]]\nname=\"x\"\ntarget=\"127.0.0.1:2\"\n[socks_exit]\nenabled=true\n"))
			return err
		},
		"direct server (exit)": func() error {
			_, err := LoadServer(write(t, srvBase+"role=\"exit\"\n[[forward]]\nname=\"x\"\ntarget=\"127.0.0.1:2\"\n[socks_exit]\nenabled=true\n"))
			return err
		},
		"direct client (entry)": func() error {
			_, err := LoadClient(write(t, "[client]\nrole=\"entry\"\nserver=\"h:1\"\nsecret_file=\"s\"\n[[forward]]\nname=\"x\"\nlisten=\"127.0.0.1:2\"\n[socks]\nenabled=true\nlisten=\"127.0.0.1:3\"\n"))
			return err
		},
	}
	for name, f := range ok {
		if err := f(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]func() error{
		"unknown role": func() error {
			_, err := LoadClient(write(t, "[client]\nrole=\"both\"\nserver=\"h:1\"\nsecret_file=\"s\"\n"))
			return err
		},
		"entry forward with target": func() error {
			_, err := LoadServer(write(t, srvBase+"[[forward]]\nname=\"x\"\nlisten=\"127.0.0.1:2\"\ntarget=\"127.0.0.1:9\"\n"))
			return err
		},
		"exit forward with listen": func() error {
			_, err := LoadClient(write(t, cliBase+"[[forward]]\nname=\"x\"\nlisten=\"127.0.0.1:2\"\n"))
			return err
		},
		"socks listener on exit": func() error {
			_, err := LoadClient(write(t, cliBase+"[socks]\nenabled=true\nlisten=\"127.0.0.1:3\"\n"))
			return err
		},
		"socks_exit on entry": func() error {
			_, err := LoadServer(write(t, srvBase+"[socks_exit]\nenabled=true\n"))
			return err
		},
	}
	for name, f := range bad {
		if err := f(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPublicListenersRefused(t *testing.T) {
	bad := map[string]func() error{
		"socks on all interfaces": func() error {
			_, err := LoadServer(write(t, srvBase+"[socks]\nenabled=true\nlisten=\"0.0.0.0:3\"\n"))
			return err
		},
		"admin on all interfaces": func() error {
			_, err := LoadServer(write(t, srvBase+"admin_listen=\"0.0.0.0:9\"\n"))
			return err
		},
		"client admin public": func() error {
			_, err := LoadClient(write(t, cliBase+"admin_listen=\"10.0.0.1:9\"\n"))
			return err
		},
		"bad allow_from": func() error {
			_, err := LoadServer(write(t, srvBase+"allow_from=[\"nope\"]\n"))
			return err
		},
	}
	for name, f := range bad {
		if err := f(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadServer(write(t, srvBase+"admin_listen=\"127.0.0.1:9\"\n[socks]\nenabled=true\nlisten=\"[::1]:3\"\n")); err != nil {
		t.Errorf("loopback rejected: %v", err)
	}
	if _, err := LoadServer(write(t, srvBase+"[socks]\nenabled=true\nlisten=\"0.0.0.0:3\"\nallow_public=true\n")); err != nil {
		t.Errorf("explicit allow_public rejected: %v", err)
	}
}

// The shipped examples must stay valid, or the documented setup breaks.
func TestExampleConfigsLoad(t *testing.T) {
	for name, load := range map[string]func(string) error{
		"server.example.toml":              func(p string) error { _, err := LoadServer(p); return err },
		"server-exit.example.toml":         func(p string) error { _, err := LoadServer(p); return err },
		"server-behind-nginx.example.toml": func(p string) error { _, err := LoadServer(p); return err },
		"server-cloudflared.example.toml":  func(p string) error { _, err := LoadServer(p); return err },
		"client.example.toml":              func(p string) error { _, err := LoadClient(p); return err },
		"client-entry.example.toml": func(p string) error {
			// spki_sha256 is blank in the template; fill a placeholder pin.
			b, _ := os.ReadFile(filepath.Join("..", "..", "deploy", "client-entry.example.toml"))
			fixed := strings.Replace(string(b), `spki_sha256 = ""`, `spki_sha256 = "`+strings.Repeat("ab", 32)+`"`, 1)
			_, err := LoadClient(write(t, fixed))
			return err
		},
	} {
		p := filepath.Join("..", "..", "deploy", name)
		if name == "client-entry.example.toml" {
			p = ""
		}
		if err := load(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Values that would not mean "off" but silently break the tunnel must be refused.
func TestLimitsAndDurationsValidated(t *testing.T) {
	const srvBase = "[server]\nlisten=\"0.0.0.0:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\n"
	for name, extra := range map[string]string{
		"max_conns 0":                 "max_conns=0\n",
		"max_streams_per_session 0":   "max_streams_per_session=0\n",
		"max_streams_per_session < 0": "max_streams_per_session=-1\n",
		"idle_timeout 0s":             "idle_timeout=\"0s\"\n",
		"idle_timeout negative":       "idle_timeout=\"-5m\"\n",
		"negative per-ip cap":         "max_conns_per_ip=-1\n",
		"negative forward cap":        "forward_max_conns_per_ip=-1\n",
	} {
		if _, err := LoadServer(write(t, srvBase+extra)); err == nil {
			t.Errorf("server %s: accepted", name)
		}
	}
	for name, extra := range map[string]string{
		"idle_timeout 0s":  "idle_timeout=\"0s\"\n",
		"ping 0s":          "ping=\"0s\"\n",
		"ping too slow":    "ping=\"90s\"\n",
		"negative cap":     "forward_max_conns_per_ip=-3\n",
		"stagger negative": "dial_stagger=\"-1s\"\n",
		"stagger too long": "dial_stagger=\"5m\"\n",
		"stagger garbage":  "dial_stagger=\"soon\"\n",
	} {
		s := "[client]\nserver=\"h.example:443\"\nsecret_file=\"s\"\n" + extra
		if _, err := LoadClient(write(t, s)); err == nil {
			t.Errorf("client %s: accepted", name)
		}
	}
	for _, v := range []string{"0s", "2s", "30s"} {
		if _, err := LoadClient(write(t, "[client]\nserver=\"h.example:443\"\nsecret_file=\"s\"\ndial_stagger=\""+v+"\"\n")); err != nil {
			t.Errorf("dial_stagger %s rejected: %v", v, err)
		}
	}
	// 0 stays valid where it means "off".
	if _, err := LoadServer(write(t, srvBase+"max_conns_per_ip=0\nmax_sessions_per_key=0\nauth_fail_per_min_per_ip=0\nforward_max_conns_per_ip=0\n")); err != nil {
		t.Errorf("0 = off rejected: %v", err)
	}
}

func TestDecoyDirValidated(t *testing.T) {
	good := t.TempDir()
	mk := func(dir string) string {
		return write(t, "[server]\nlisten=\"0.0.0.0:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\ndecoy_dir=\""+dir+"\"\n")
	}
	if _, err := LoadServer(mk(good)); err != nil {
		t.Fatalf("existing dir rejected: %v", err)
	}
	if _, err := LoadServer(mk(filepath.Join(good, "missing"))); err == nil || !strings.Contains(err.Error(), "decoy_dir") {
		t.Fatalf("missing decoy_dir accepted: %v", err)
	}
}

func TestAutoCarrierConfig(t *testing.T) {
	base := "[client]\nserver=\"h.example:443\"\nsecret_file=\"s\"\n"
	for name, body := range map[string]string{
		"auto":                "carrier=\"auto\"\n",
		"auto with order":     "carrier=\"auto\"\nauto_order=[\"ws\",\"chunk\"]\n",
		"auto, reverse order": "carrier=\"auto\"\nauto_order=[\"chunk\",\"http\",\"ws\"]\n",
	} {
		if _, err := LoadClient(write(t, base+body)); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
	for name, body := range map[string]string{
		"order without auto":  "carrier=\"ws\"\nauto_order=[\"ws\",\"http\"]\n",
		"one carrier":         "carrier=\"auto\"\nauto_order=[\"ws\"]\n",
		"unknown carrier":     "carrier=\"auto\"\nauto_order=[\"ws\",\"quic\"]\n",
		"duplicate":           "carrier=\"auto\"\nauto_order=[\"ws\",\"ws\"]\n",
		"unknown carrier set": "carrier=\"fast\"\n",
	} {
		if _, err := LoadClient(write(t, base+body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestKeyLimitsAndDrainConfig(t *testing.T) {
	srv := "[server]\nlisten=\"0.0.0.0:1\"\ntls_cert=\"a\"\ntls_key=\"b\"\nsecret_files=[\"s\"]\n"
	f, err := LoadServer(write(t, srv+"max_mbit_per_key=12.5\nmax_new_streams_per_sec_per_key=40\ndrain_timeout=\"30s\"\n"))
	if err != nil || f.Server.MaxMbitPerKey != 12.5 || f.Server.MaxNewStreamsPerSecPerKey != 40 || f.Server.DrainTimeout != "30s" {
		t.Fatalf("valid limits: %v %+v", err, f)
	}
	if f, err := LoadServer(write(t, srv)); err != nil || f.Server.MaxMbitPerKey != 0 || f.Server.DrainTimeout != "10s" {
		t.Fatalf("defaults: limits off, drain 10s: %v %+v", err, f)
	}
	for name, body := range map[string]string{
		"negative mbit":      "max_mbit_per_key=-1\n",
		"absurd mbit":        "max_mbit_per_key=1e9\n",
		"negative streams":   "max_new_streams_per_sec_per_key=-5\n",
		"drain not duration": "drain_timeout=\"soon\"\n",
		"drain negative":     "drain_timeout=\"-5s\"\n",
		"drain too long":     "drain_timeout=\"1h\"\n",
	} {
		if _, err := LoadServer(write(t, srv+body)); err == nil {
			t.Errorf("server: %s accepted", name)
		}
	}
	cli := "[client]\nserver=\"h.example:443\"\nsecret_file=\"s\"\n"
	if f, err := LoadClient(write(t, cli+"drain_timeout=\"0s\"\n")); err != nil || f.Client.DrainTimeout != "0s" {
		t.Fatalf("client drain 0s: %v", err)
	}
	if _, err := LoadClient(write(t, cli+"drain_timeout=\"forever\"\n")); err == nil {
		t.Error("client: drain_timeout forever accepted")
	}
}
