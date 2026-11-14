# Security

## Looking like ordinary HTTPS

Goal: to a passive observer and to an active prober, the server is a small HTTPS website, and the
client is a TLS client that behaves like one. The carriers are ordinary HTTP/1.1 over TLS (WebSocket or
POST requests). The client's ClientHello is a browser's (uTLS). More techniques are planned and
benchmarked in [stealth-plan.md](stealth-plan.md).

What is done (each has a test):

- **Browser ClientHello (uTLS).** `tls_fingerprint` picks the profile: `chrome` (the default: Chrome 133 with the
  X25519MLKEM768 key share), `chrome_nopq` (Chrome 120, a smaller hello), `firefox`, `safari`, `ios`, `android`, or `go`.
  Every browser profile offers `http/1.1` as its only ALPN and leaves out ALPS, as a browser's WebSocket connection does,
  because hstun speaks HTTP/1.1; it resumes sessions with a ticket. The User-Agent follows the profile. A test
  captures the hello and checks GREASE, ALPN, extensions, the key shares and Chrome's JA4 cipher hash
  (`8daaf6152771`). Verification (CA file or SPKI pin) works the same with every profile. Checked against four real public sites on 2026-11-14:
  `chrome`, `chrome_nopq` and `firefox` completed a request on all of them; `safari` failed once with a signature error
  (a uTLS quirk), and `ios` and `android` were not tried, so prefer chrome or firefox. The profiles are those
  of the pinned uTLS snapshot, so they age: a browser profile that is years old is itself a tell, so update the
  library with the releases.

- **Decoy site.** Every request that is not a valid tunnel request gets what a small web server gives:
  `decoy_dir` (a static site, no directory listings, dot files hidden) or a built-in one-page site, and
  one standard HTML 404 for everything else. That includes the websocket path without an upgrade, wrong
  methods on the http carrier endpoints, malformed session ids and rejected handshakes, so the status
  code and body never confirm that a path is the tunnel. No `Server` header, no Go or hstun text.
  (`TestServerLooksLikeAWebsite`)
- **ALPN `http/1.1` in the ClientHello**, as a browser sends for a websocket. net/http silently drops
  it for upgrade requests, so the websocket carrier does its own TLS dial. (`TestDialLooksLikeOrdinaryHTTPSClient`)
- **TLS session resumption** across the client's connections, like any HTTPS client (same test).
- **A browser-style `User-Agent`** instead of `Go-http-client/1.1` (`user_agent` to change it; the default carries the current Chrome major and is bumped at each release).
- **TLS 1.3 everywhere it is available**, certificate hidden inside the handshake, then only TLS
  application-data records on the wire (checked with packet captures, see the audit report).
- **Warning at startup** while `ws_path`/`http_path` are still the public defaults.

- **Record sizes and timing.** `padding` adds random filler to small records (negotiated, so older peers are unaffected; the
  filler is authenticated and written in the same carrier message as a real record, so on the wire it only changes sizes);
  keepalives come at a varied interval with a random-size filler, not a fixed-size beat every 25 s.
- **First-packet filters.** `tls_fragment` sends the ClientHello as several TCP segments or TLS records (off by default).
- **Connection lifetimes.** `session_age` replaces carrier sessions on a random timer, gracefully, so no connection lives for days.
- **Metadata of the http carrier** travels in cookies named from the secret path, not in `X-Hs-*` headers, and its error
  answers are HTML pages in the decoy's style, not plain-text words. A test checks both.
- **Browser-like headers** on the http and WebSocket requests follow the chosen profile (User-Agent, Accept-Language,
  Sec-CH-UA and Sec-Fetch-* for Chrome profiles). Go's HTTP client sends headers in alphabetical order, which a browser does not;
  that is only visible after TLS ends (a CDN).
- **Failover by soak.** `carrier = "auto"` counts a carrier as working only after its session stayed up for 20 seconds, so a path
  that lets a few packets through and then drops the flow is left, not kept.

What you should do when deploying: use a real domain with a CA-issued certificate (Let's Encrypt)
instead of a self-signed one, put real content in `decoy_dir`, use your own `ws_path`/`http_path`,
and use a normal HTTPS port (443 or a port that site would plausibly use). A self-signed
certificate is visible to anyone who connects and does not hold the pin.

What is **not** done, on purpose or for lack of a way to do it cleanly:

- With `tls_fingerprint = "go"` the ClientHello is Go's own, so a JA3/JA4 fingerprint identifies "a Go program".
  The default is a browser profile instead (see below). Only the ClientHello is a browser's: after it the
  handshake and the sealed stream are hstun's, and the server side is Go's `crypto/tls` (or nginx).
- Shaping is light. Padding covers small records and the start of a session (`strong`), not bulk transfers; the inner
  traffic's own shape (an inner TLS handshake in the first records, many parallel streams) still shows through. A sophisticated
  classifier could still separate this from web browsing by volume and timing. What would help further is listed in
  [stealth-plan.md](stealth-plan.md) (Vision-style padding of the first packets of each stream, ECH, pinning).
- The SNI is sent in clear (TLS 1.3 without ECH); choose a hostname you control.


## Security model

Threat model: an attacker who can reach the listener from the internet, or who
sits on the path including a TLS-terminating CDN, without the shared secret.

- **Handshake:** the secret never crosses the wire. HMAC challenge/response with
  fresh 32-byte nonces from both sides, labelled per direction: mutual, replay-
  proof without clocks, reflection-proof (tests for replay, reflection, bad hello).
- **Sealed stream (protocol v2):** after the handshake all traffic is wrapped in
  AES-256-GCM frames. Keys are HKDF-derived from the handshake session key, one
  per direction; the nonce is a strict per-direction counter. A middlebox cannot
  read, modify, replay, reorder or reflect frames; truncation only kills the
  session. `TestMiddleboxCannotReadPayload` runs all four mode/carrier pairs
  through a TLS-terminating tap and checks the payload never appears in it.
- **TLS:** verification is never off (`system`, `ca_file`, or an SPKI pin);
  TLS 1.2+. Add `allow_from` (and ufw) so only the expected peers can connect.
- **Pre-auth resource limits:** per-IP connection cap (Cloudflare peers exempt,
  `CF-Connecting-IP` trusted only from the Cloudflare ranges), global cap that
  closes instead of queueing, 10 s handshake deadline, 256-byte handshake
  messages, 8 KB headers, request-body caps, per-IP failed-handshake limit
  (IPv6 counted per /64, an attacker cannot reset his own counter).
- **Exit:** forwards are matched by name to fixed local targets; the SOCKS exit is
  off by default, refuses loopback/private/CGNAT/link-local/own addresses
  (including IPv4 hidden in `::ffff:`, NAT64 and 6to4), resolves once and dials
  the IP, and can be limited to `allow_ports`. Streams per session are capped on
  both ends.
- **Local services:** SOCKS5 (no auth) and `/stats` only bind loopback unless
  explicitly overridden. Key files must be 0600; keys are never printed or logged.
- **Not a goal:** hiding that tunnelling happens, or defeating traffic analysis.
  One shared key per peer pair: use a different key (several `secret_files`)
  per exit host so one compromised host does not share a key with the others.
- **Service account:** create it with an explicit, unused uid, for example
  `groupadd -r -g 61999 hstunsvc && useradd -r -u 61999 -g 61999 -M -s /usr/sbin/nologin hstunsvc`
  (and set `User=`/`Group=` in the unit). A plain `useradd -r` hands out uid 999, which
  Docker images (Postgres, among others) also use inside containers, so the tunnel would
  share an identity with them on the host. Check `getent passwd <uid>` and running
  container uids first.
- The systemd units drop all capabilities and sandbox the service; a listener
  on a port below 1024 needs `AmbientCapabilities=CAP_NET_BIND_SERVICE`.


## Robustness properties (each has a test)

- Every read, write and handshake has a deadline; no goroutine waits without a timer.
- Every length is capped before allocation (`proto`, `seal`, request bodies, headers).
- TCP keepalive and `TCP_USER_TIMEOUT` on every socket; the mux declares a silent
  session dead after 80 s; pings every 25 s.
- Wedge test: 500 half-dead connections held; a legitimate client keeps p99 < 1 s
  and goroutines return to baseline.
- Idle tunnels stay idle.
- Per-key limits on bandwidth and on the rate of new streams (`max_mbit_per_key`, `max_new_streams_per_sec_per_key`)
  bound what one key can take from an exit, and a stopping process drains instead of cutting every stream.


## Known limits

- A cut transfer is passed on as a cut: if one side resets (RST), vanishes or the tunnel session dies, the other
  side gets a connection reset and not a clean end. This needs a two-hunk patch to yamux that adds `Stream.Reset`
  (see `third_party/yamux/PATCHES.md`). One case is still not signalled: an idle timeout ends the stream with a normal close.
- Cloudflare peers are exempt from `max_conns_per_ip`; the global `max_conns` still applies.
- See "Looking like ordinary HTTPS" for what traffic analysis can still tell.

