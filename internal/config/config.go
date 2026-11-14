// Package config loads the TOML files. Unknown keys are an error, so a typo
// in a security setting can never be silently ignored.
package config

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"hstun/internal/decoy"
)

// Default carrier paths. They are public in the source repository, so every
// deployment should set its own.
const (
	DefaultWSPath   = "/hs/ws"
	DefaultHTTPPath = "/hs/h"
)

// Roles. The entry owns the user-facing listeners (forwards, SOCKS5) and
// opens streams; the exit serves them (fixed targets, optional SOCKS exit).
// Either role can be on the listening side (server) or the dialing side
// (client): reverse mode is server=entry + client=exit, direct mode is
// client=entry + server=exit.
const (
	RoleEntry = "entry"
	RoleExit  = "exit"
)

type ServerFile struct {
	Server    ServerSection `toml:"server"`
	Forward   []Forward     `toml:"forward"`
	Socks     SocksSection  `toml:"socks"`
	SocksExit SocksExit     `toml:"socks_exit"`
}

type ServerSection struct {
	Role        string   `toml:"role"` // entry (default) or exit
	Listen      string   `toml:"listen"`
	TLSCert     string   `toml:"tls_cert"`
	TLSKey      string   `toml:"tls_key"`
	Carriers    []string `toml:"carriers"`
	WSPath      string   `toml:"ws_path"`
	HTTPPath    string   `toml:"http_path"`
	DecoyDir    string   `toml:"decoy_dir"` // static site shown to everyone who is not the tunnel; empty = built-in page
	SecretFiles []string `toml:"secret_files"`

	TrustProxy   string `toml:"trust_proxy"`
	CFRangesFile string `toml:"cf_ranges_file"`

	MaxConns             int      `toml:"max_conns"`
	MaxConnsPerIP        int      `toml:"max_conns_per_ip"` // 0 = off; Cloudflare peers are exempt
	AllowFrom            []string `toml:"allow_from"`       // optional source CIDR allow-list
	MaxSessionsPerKey    int      `toml:"max_sessions_per_key"`
	MaxStreamsPerSession int      `toml:"max_streams_per_session"`
	// ForwardMaxConnsPerIP caps concurrent connections per source IP on the
	// entry-role forward and SOCKS ports (0 = off): each one holds a stream.
	ForwardMaxConnsPerIP int    `toml:"forward_max_conns_per_ip"`
	AuthFailPerMinPerIP  int    `toml:"auth_fail_per_min_per_ip"`
	IdleTimeout          string `toml:"idle_timeout"`
	AdminListen          string `toml:"admin_listen"`

	// Per secret key (every session of one key shares the budget); 0 = off.
	MaxMbitPerKey             float64 `toml:"max_mbit_per_key"`                // megabits per second, each direction
	MaxNewStreamsPerSecPerKey int     `toml:"max_new_streams_per_sec_per_key"` // exit role: new streams the key may open per second
	// DrainTimeout is how long a stopping process lets the streams in flight
	// finish (no new sessions or streams are accepted meanwhile); "0s" = stop at once.
	DrainTimeout string `toml:"drain_timeout"`
}

type Forward struct {
	Name   string `toml:"name"`
	Listen string `toml:"listen"` // entry role only
	Target string `toml:"target"` // exit role only
}

type SocksSection struct {
	Enabled bool   `toml:"enabled"`
	Listen  string `toml:"listen"`
	// SOCKS5 here has no authentication, so a non-loopback listen address is
	// refused unless this is set on purpose.
	AllowPublic bool `toml:"allow_public"`
}

type ClientFile struct {
	Client    ClientSection `toml:"client"`
	Forward   []Forward     `toml:"forward"`
	Socks     SocksSection  `toml:"socks"`
	SocksExit SocksExit     `toml:"socks_exit"`
}

type ClientSection struct {
	Role    string `toml:"role"` // exit (default) or entry
	Server  string `toml:"server"`
	Connect string `toml:"connect"`
	SNI     string `toml:"sni"`
	Carrier string `toml:"carrier"` // ws, http, chunk (the http carrier with one connection per request) or auto (fail over ws, http, chunk)
	// AutoOrder is the order tried by carrier = "auto" (default ws, http, chunk).
	AutoOrder []string `toml:"auto_order"`
	Conns     int      `toml:"conns"`
	// chunk carrier only; 0 = default (12288 up, 32768 down, 8 parallel).
	ChunkUpBytes   int `toml:"chunk_up_bytes"`   // largest upstream body per connection, 1024..65536
	ChunkDownBytes int `toml:"chunk_down_bytes"` // largest downstream payload per connection, 4096..131072
	ChunkParallel  int `toml:"chunk_parallel"`   // uploads and downloads in flight each, 1..32
	// DialStagger spreads the first dials of a client's sessions: session 0
	// connects at once, every other session waits a random time between a
	// quarter of this value and the value itself. "0s" dials them all together.
	DialStagger string `toml:"dial_stagger"`
	SecretFile  string `toml:"secret_file"`
	Name        string `toml:"name"`
	WSPath      string `toml:"ws_path"`
	HTTPPath    string `toml:"http_path"`
	UserAgent   string `toml:"user_agent"` // empty = a common browser string

	TLSVerify  string `toml:"tls_verify"`
	CAFile     string `toml:"ca_file"`
	SPKISHA256 string `toml:"spki_sha256"`
	TLSMLKEM   bool   `toml:"tls_mlkem"`

	Ping        string `toml:"ping"`
	IdleTimeout string `toml:"idle_timeout"`

	// Entry role only.
	MaxStreamsPerSession int    `toml:"max_streams_per_session"`
	ForwardMaxConnsPerIP int    `toml:"forward_max_conns_per_ip"` // 0 = off
	AdminListen          string `toml:"admin_listen"`

	// DrainTimeout is how long a stopping process lets the streams in flight
	// finish (no new connections or streams meanwhile); "0s" = stop at once.
	DrainTimeout string `toml:"drain_timeout"`
}

type SocksExit struct {
	Enabled     bool  `toml:"enabled"`
	DenyPrivate bool  `toml:"deny_private"`
	AllowPorts  []int `toml:"allow_ports"`
}

func decode(path string, v any) error {
	md, err := toml.DecodeFile(path, v)
	if err != nil {
		return err
	}
	if u := md.Undecoded(); len(u) > 0 {
		keys := make([]string, len(u))
		for i, k := range u {
			keys[i] = k.String()
		}
		return fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	return nil
}

func LoadServer(path string) (*ServerFile, error) {
	f := &ServerFile{}
	f.Server.Role = RoleEntry
	f.SocksExit.DenyPrivate = true
	f.Server.Carriers = []string{"ws", "http"}
	f.Server.WSPath = DefaultWSPath
	f.Server.HTTPPath = DefaultHTTPPath
	f.Server.MaxConns = 1024
	f.Server.MaxConnsPerIP = 64
	f.Server.MaxSessionsPerKey = 16
	f.Server.MaxStreamsPerSession = 256
	f.Server.ForwardMaxConnsPerIP = 256
	f.Server.AuthFailPerMinPerIP = 10
	f.Server.IdleTimeout = "10m"
	f.Server.DrainTimeout = "10s"
	if err := decode(path, f); err != nil {
		return nil, err
	}
	return f, f.validate()
}

func (f *ServerFile) validate() error {
	s := &f.Server
	if s.Listen == "" {
		return errors.New("server: listen is required")
	}
	if (s.TLSCert == "") != (s.TLSKey == "") {
		return errors.New("server: tls_cert and tls_key must be set together")
	}
	if s.TLSCert == "" && s.TrustProxy != "loopback" && s.TrustProxy != "cloudflared" {
		return errors.New("server: tls_cert and tls_key are required; they may be left empty only with trust_proxy=loopback or cloudflared, which serve plain HTTP on a loopback address for a local proxy")
	}
	if len(s.SecretFiles) == 0 {
		return errors.New("server: secret_files is required")
	}
	if len(s.Carriers) == 0 {
		return errors.New("server: carriers must not be empty")
	}
	for _, c := range s.Carriers {
		if c != "ws" && c != "http" {
			return fmt.Errorf("server: unknown carrier %q", c)
		}
	}
	if !strings.HasPrefix(s.WSPath, "/") || !strings.HasPrefix(s.HTTPPath, "/") || s.WSPath == s.HTTPPath {
		return errors.New("server: ws_path and http_path must start with / and differ")
	}
	if s.DecoyDir != "" {
		if err := decoy.CheckDir(s.DecoyDir); err != nil {
			return fmt.Errorf("server: decoy_dir: %w", err)
		}
	}
	switch s.TrustProxy {
	case "", "cloudflare":
	case "loopback", "cloudflared":
		// Behind a local web server (nginx: X-Real-IP) or a local cloudflared
		// (CF-Connecting-IP) every peer is 127.0.0.1 and a header carries the real
		// address, so the header must only ever be believed from a loopback peer.
		if !loopbackListen(s.Listen) {
			return fmt.Errorf("server: trust_proxy=%s needs listen on a loopback address such as 127.0.0.1:24083", s.TrustProxy)
		}
		if len(s.AllowFrom) > 0 {
			return fmt.Errorf("server: allow_from cannot be combined with trust_proxy=%s (every peer is the proxy); filter in the proxy or the firewall", s.TrustProxy)
		}
	default:
		return fmt.Errorf("server: trust_proxy %q unsupported", s.TrustProxy)
	}
	if s.TrustProxy == "cloudflare" && s.CFRangesFile == "" {
		return errors.New("server: trust_proxy=cloudflare needs cf_ranges_file")
	}
	if d, err := time.ParseDuration(s.IdleTimeout); err != nil || d < time.Second {
		return fmt.Errorf("server: idle_timeout must be a duration of at least 1s, got %q", s.IdleTimeout)
	}
	// A zero here would not mean "off": a zero-size connection limit refuses every
	// connection and a zero stream cap leaves no session able to open a stream.
	if s.MaxConns < 1 {
		return errors.New("server: max_conns must be >= 1")
	}
	if s.MaxStreamsPerSession < 1 {
		return errors.New("server: max_streams_per_session must be >= 1")
	}
	if s.MaxConnsPerIP < 0 || s.MaxSessionsPerKey < 0 || s.AuthFailPerMinPerIP < 0 || s.ForwardMaxConnsPerIP < 0 {
		return errors.New("server: max_conns_per_ip, max_sessions_per_key, auth_fail_per_min_per_ip and forward_max_conns_per_ip must be >= 0 (0 = off)")
	}
	if s.MaxMbitPerKey < 0 || s.MaxMbitPerKey > 100000 || s.MaxNewStreamsPerSecPerKey < 0 {
		return errors.New("server: max_mbit_per_key (0..100000) and max_new_streams_per_sec_per_key must be >= 0 (0 = off)")
	}
	if err := checkDrain("server", s.DrainTimeout); err != nil {
		return err
	}
	for _, c := range s.AllowFrom {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return fmt.Errorf("server: allow_from %q: %w", c, err)
		}
	}
	if s.AdminListen != "" && !isLoopback(s.AdminListen) {
		return errors.New("server: admin_listen must be a loopback address (the stats page has no authentication)")
	}
	if err := validateRole(s.Role, f.Forward, f.Socks, f.SocksExit); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

// validateRole checks the forwards and SOCKS sections against the role, so a
// setting that would be silently ignored is an error instead.
func validateRole(role string, fws []Forward, socks SocksSection, se SocksExit) error {
	seen := map[string]bool{}
	for _, fw := range fws {
		if fw.Name == "" {
			return errors.New("forward: name is required")
		}
		if seen[fw.Name] {
			return fmt.Errorf("forward: duplicate name %q", fw.Name)
		}
		seen[fw.Name] = true
		switch role {
		case RoleEntry:
			if fw.Listen == "" || fw.Target != "" {
				return fmt.Errorf("forward %s: the entry role needs listen and no target", fw.Name)
			}
			if _, _, err := net.SplitHostPort(fw.Listen); err != nil {
				return fmt.Errorf("forward %s: %w", fw.Name, err)
			}
		case RoleExit:
			if fw.Target == "" || fw.Listen != "" {
				return fmt.Errorf("forward %s: the exit role needs target and no listen", fw.Name)
			}
			if _, _, err := net.SplitHostPort(fw.Target); err != nil {
				return fmt.Errorf("forward %s: %w", fw.Name, err)
			}
		default:
			return fmt.Errorf("role must be %q or %q, got %q", RoleEntry, RoleExit, role)
		}
	}
	if role != RoleEntry && role != RoleExit {
		return fmt.Errorf("role must be %q or %q, got %q", RoleEntry, RoleExit, role)
	}
	if socks.Enabled && role != RoleEntry {
		return errors.New("socks listener only applies to the entry role")
	}
	if socks.Enabled && socks.Listen == "" {
		return errors.New("socks: listen is required when enabled")
	}
	if socks.Enabled && !socks.AllowPublic && !isLoopback(socks.Listen) {
		return errors.New("socks: listen must be a loopback address (no authentication); set allow_public = true to override")
	}
	if se.Enabled && role != RoleExit {
		return errors.New("socks_exit only applies to the exit role")
	}
	return nil
}

func LoadClient(path string) (*ClientFile, error) {
	f := &ClientFile{}
	f.Client.Role = RoleExit
	f.Client.MaxStreamsPerSession = 256
	f.Client.ForwardMaxConnsPerIP = 256
	f.Client.Carrier = "ws"
	f.Client.Conns = 4
	f.Client.DialStagger = "2s"
	f.Client.Name = "client"
	f.Client.WSPath = DefaultWSPath
	f.Client.HTTPPath = DefaultHTTPPath
	f.Client.TLSVerify = "system"
	f.Client.Ping = "25s"
	f.Client.IdleTimeout = "10m"
	f.Client.DrainTimeout = "10s"
	f.SocksExit.DenyPrivate = true
	if err := decode(path, f); err != nil {
		return nil, err
	}
	return f, f.validate()
}

func (f *ClientFile) validate() error {
	c := &f.Client
	if c.Server == "" || c.SecretFile == "" {
		return errors.New("client: server and secret_file are required")
	}
	if _, _, err := net.SplitHostPort(c.Server); err != nil {
		return fmt.Errorf("client: server: %w", err)
	}
	if c.Connect != "" {
		if _, _, err := net.SplitHostPort(c.Connect); err != nil {
			return fmt.Errorf("client: connect: %w", err)
		}
	}
	if c.Carrier != "ws" && c.Carrier != "http" && c.Carrier != "chunk" && c.Carrier != "auto" {
		return fmt.Errorf("client: carrier must be ws, http, chunk or auto, got %q", c.Carrier)
	}
	if len(c.AutoOrder) > 0 && c.Carrier != "auto" {
		return errors.New("client: auto_order only applies with carrier = \"auto\"")
	}
	seen := map[string]bool{}
	for _, a := range c.AutoOrder {
		if a != "ws" && a != "http" && a != "chunk" {
			return fmt.Errorf("client: auto_order has unknown carrier %q", a)
		}
		if seen[a] {
			return fmt.Errorf("client: auto_order lists %q twice", a)
		}
		seen[a] = true
	}
	if c.Carrier == "auto" && len(c.AutoOrder) == 1 {
		return errors.New("client: auto_order needs at least two carriers (use a single carrier instead)")
	}
	if c.ChunkUpBytes != 0 && (c.ChunkUpBytes < 1024 || c.ChunkUpBytes > 64<<10) {
		return errors.New("client: chunk_up_bytes must be 0 (default) or 1024..65536")
	}
	if c.ChunkDownBytes != 0 && (c.ChunkDownBytes < 4096 || c.ChunkDownBytes > 128<<10) {
		return errors.New("client: chunk_down_bytes must be 0 (default) or 4096..131072")
	}
	if c.ChunkParallel != 0 && (c.ChunkParallel < 1 || c.ChunkParallel > 32) {
		return errors.New("client: chunk_parallel must be 0 (default) or 1..32")
	}
	if c.Conns < 1 || c.Conns > 64 {
		return errors.New("client: conns must be 1..64")
	}
	if c.DialStagger != "" {
		if d, err := time.ParseDuration(c.DialStagger); err != nil || d < 0 || d > 30*time.Second {
			return fmt.Errorf("client: dial_stagger must be a duration between 0s and 30s, got %q", c.DialStagger)
		}
	}
	switch c.TLSVerify {
	case "system":
	case "ca_file":
		if c.CAFile == "" {
			return errors.New("client: tls_verify=ca_file needs ca_file")
		}
	case "spki":
		if len(c.SPKISHA256) != 64 {
			return errors.New("client: tls_verify=spki needs spki_sha256 (64 hex chars)")
		}
	default:
		return fmt.Errorf("client: unknown tls_verify %q", c.TLSVerify)
	}
	ping, err := time.ParseDuration(c.Ping)
	if err != nil {
		return fmt.Errorf("client: bad ping %q: %w", c.Ping, err)
	}
	// The mux declares a session dead after 80 s without any received byte, so
	// pings must be on and comfortably faster than that.
	if ping < time.Second || ping > 60*time.Second {
		return fmt.Errorf("client: ping must be between 1s and 60s, got %q", c.Ping)
	}
	if d, err := time.ParseDuration(c.IdleTimeout); err != nil || d < time.Second {
		return fmt.Errorf("client: idle_timeout must be a duration of at least 1s, got %q", c.IdleTimeout)
	}
	if c.ForwardMaxConnsPerIP < 0 {
		return errors.New("client: forward_max_conns_per_ip must be >= 0 (0 = off)")
	}
	if err := checkDrain("client", c.DrainTimeout); err != nil {
		return err
	}
	if c.MaxStreamsPerSession < 1 {
		return errors.New("client: max_streams_per_session must be >= 1")
	}
	if c.AdminListen != "" && !isLoopback(c.AdminListen) {
		return errors.New("client: admin_listen must be a loopback address (the stats page has no authentication)")
	}
	if err := validateRole(c.Role, f.Forward, f.Socks, f.SocksExit); err != nil {
		return fmt.Errorf("client: %w", err)
	}
	return nil
}

// checkDrain validates drain_timeout: empty or a duration from 0s to 5m.
func checkDrain(who, v string) error {
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 || d > 5*time.Minute {
		return fmt.Errorf("%s: drain_timeout must be a duration from 0s to 5m, got %q", who, v)
	}
	return nil
}

// isLoopback reports whether a host:port names a loopback IP literal.
func isLoopback(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loopbackListen reports whether addr is host:port with a literal loopback IP.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
