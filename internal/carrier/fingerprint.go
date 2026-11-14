package carrier

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sort"
	"sync"

	utls "github.com/refraction-networking/utls"
)

// A TLS fingerprint is how a ClientHello looks on the wire: the cipher suites,
// extensions, curves and their order. Go's crypto/tls has a very recognisable
// one, so by default the client sends the ClientHello of a real browser, built
// by uTLS. The rest of the handshake and the encryption are still Go's.
//
// Every browser profile asks for http/1.1 only: hstun speaks HTTP/1.1 (a
// WebSocket upgrade or plain POSTs), and browsers do the same for WebSocket
// connections. The ALPS extension is dropped with it, as a browser does when it
// does not offer h2.
const (
	FingerprintGo      = "go"          // crypto/tls as it is (recognisable as Go)
	FingerprintChrome  = "chrome"      // Chrome 133, with the X25519MLKEM768 key share (the default)
	FingerprintChromeN = "chrome_nopq" // Chrome 120, without post-quantum key shares: a smaller ClientHello
	FingerprintFirefox = "firefox"     // Firefox 148
	FingerprintSafari  = "safari"      // Safari 26.3
	FingerprintIOS     = "ios"         // Safari on iOS 14
	FingerprintAndroid = "android"     // OkHttp on Android 11
)

// DefaultFingerprint is what a client uses when tls_fingerprint is not set.
const DefaultFingerprint = FingerprintChrome

var fingerprintIDs = map[string]utls.ClientHelloID{
	FingerprintChrome:  utls.HelloChrome_133,
	FingerprintChromeN: utls.HelloChrome_120,
	FingerprintFirefox: utls.HelloFirefox_148,
	FingerprintSafari:  utls.HelloSafari_26_3,
	FingerprintIOS:     utls.HelloIOS_14,
	FingerprintAndroid: utls.HelloAndroid_11_OkHttp,
}

// FingerprintNames lists the accepted tls_fingerprint values.
func FingerprintNames() []string {
	names := []string{FingerprintGo}
	for n := range fingerprintIDs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ValidFingerprint reports whether name is a known tls_fingerprint ("" means the default).
func ValidFingerprint(name string) bool {
	if name == "" || name == FingerprintGo {
		return true
	}
	_, ok := fingerprintIDs[name]
	return ok
}

// userAgents match each profile: a Chrome ClientHello with a Firefox User-Agent is a tell.
var userAgents = map[string]string{
	FingerprintChrome:  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
	FingerprintChromeN: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	FingerprintFirefox: "Mozilla/5.0 (X11; Linux x86_64; rv:148.0) Gecko/20100101 Firefox/148.0",
	FingerprintSafari:  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.3 Safari/605.1.15",
	FingerprintIOS:     "Mozilla/5.0 (iPhone; CPU iPhone OS 14_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.0 Mobile/15E148 Safari/604.1",
	FingerprintAndroid: "okhttp/4.9.3",
}

// UserAgentFor is the User-Agent that fits a fingerprint profile.
func UserAgentFor(fingerprint string) string {
	if ua, ok := userAgents[fingerprint]; ok {
		return ua
	}
	return userAgents[FingerprintChrome]
}

// UA is the User-Agent the requests carry: the configured one, or the one that
// matches the fingerprint.
func (o Options) UA() string {
	if o.UserAgent != "" {
		return o.UserAgent
	}
	f := o.Fingerprint
	if f == FingerprintGo {
		return DefaultUserAgent
	}
	if f == "" {
		f = DefaultFingerprint
	}
	return UserAgentFor(f)
}

// Handshake runs the client TLS handshake on raw with the configured
// fingerprint and returns the encrypted connection. The caller closes raw on error.
func (o Options) Handshake(ctx context.Context, raw net.Conn) (net.Conn, error) {
	name := o.Fingerprint
	if name == "" {
		name = DefaultFingerprint
	}
	if name == FingerprintGo {
		c := tls.Client(raw, o.TLS.Clone())
		if err := c.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		return c, nil
	}
	id, ok := fingerprintIDs[name]
	if !ok {
		return nil, fmt.Errorf("unknown tls_fingerprint %q", name)
	}
	uc, err := newUConn(raw, o.TLS, id)
	if err != nil {
		return nil, err
	}
	if err := uc.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	return uc, nil
}

// sessionCaches keeps one TLS session cache per client configuration, so later
// connections resume with a ticket as a browser does.
var sessionCaches sync.Map // *tls.Config -> utls.ClientSessionCache

func sessionCacheFor(c *tls.Config) utls.ClientSessionCache {
	if v, ok := sessionCaches.Load(c); ok {
		return v.(utls.ClientSessionCache)
	}
	v, _ := sessionCaches.LoadOrStore(c, utls.NewLRUClientSessionCache(64))
	return v.(utls.ClientSessionCache)
}

func newUConn(raw net.Conn, base *tls.Config, id utls.ClientHelloID) (*utls.UConn, error) {
	cfg := &utls.Config{
		ServerName:            base.ServerName,
		RootCAs:               base.RootCAs,
		InsecureSkipVerify:    base.InsecureSkipVerify, // set only together with the spki pin check below
		VerifyPeerCertificate: base.VerifyPeerCertificate,
		MinVersion:            tls.VersionTLS12,
		// A real ClientHello cannot carry a different ALPN than the profile; this
		// is for the library's own bookkeeping and matches what the profile sends.
		NextProtos:         []string{"http/1.1"},
		ClientSessionCache: sessionCacheFor(base),
		OmitEmptyPsk:       true, // no ticket yet: leave the pre_shared_key extension out, as a browser does
	}
	spec, err := utls.UTLSIdToSpec(id)
	if err != nil {
		return nil, err
	}
	exts := spec.Extensions[:0:0]
	for _, e := range spec.Extensions {
		switch x := e.(type) {
		case *utls.ALPNExtension:
			x.AlpnProtocols = []string{"http/1.1"}
		case *utls.ApplicationSettingsExtension, *utls.ApplicationSettingsExtensionNew:
			continue // ALPS is only offered together with h2
		}
		exts = append(exts, e)
	}
	// A browser that has a ticket adds the pre_shared_key extension, always last;
	// the library leaves it out until a ticket is cached.
	hasPSK := false
	for _, e := range exts {
		if _, ok := e.(utls.PreSharedKeyExtension); ok {
			hasPSK = true
		}
	}
	if !hasPSK {
		exts = append(exts, &utls.UtlsPreSharedKeyExtension{})
	}
	spec.Extensions = exts
	uc := utls.UClient(raw, cfg, utls.HelloCustom)
	if err := uc.ApplyPreset(&spec); err != nil {
		return nil, err
	}
	return uc, nil
}
