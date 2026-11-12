# hstun

Carries TCP over an ordinary HTTPS connection. Everything rides authenticated,
end-to-end encrypted, multiplexed sessions, so a handful of TLS connections
carry any number of TCP streams. It runs in two modes (see "Modes").

Status: v0.2.4, pre-release, not independently reviewed. Tested with the race detector, a wedge test,
resets, added latency and a TLS-terminating tap; both modes were also run across real
networks and inspected with packet captures (payload never appeared in clear). See "Security".

## What it is, and is not

Plain transport engineering, like wstunnel or BackPack's wssmux: real TLS, a real
HTTP/1.1 WebSocket upgrade or real POST requests, an authenticated handshake.
It does not imitate other protocols or rotate fake URLs, and it does not try to
beat DPI by trickery.

## Carriers (`carrier` in the client config)

| carrier | how | use |
|---|---|---|
| `ws` (default) | one WSS connection per slot, yamux mux inside | fast; passes Cloudflare orange cloud |
| `http` | keep-alive POSTs: `/u` upstream chunks, `/d` long-poll downstream, sequence numbers + ack | fallback where WebSocket is not allowed; slower |

Loopback numbers with 100 ms RTT: ws ~10 MB/s each way, http ~1.4 MB/s each way.
Idle cost per minute: ws ~300 B, http ~6 KB.

## Modes

Two independent choices: who **dials** the carrier (`hstun client`) versus who
**listens** (`hstun server`), and who is the **entry** (owns the user-facing
forward and SOCKS5 ports, opens a stream per connection) versus the **exit**
(serves streams: fixed `target`s from its own config, plus an optional SOCKS exit).

| mode | listens | dials | entry | exit | when |
|---|---|---|---|---|---|
| **direct** | server, role `exit` (the outside host) | client, role `entry` (the restricted side) | client | server | normal use, when the restricted side can reach the outside host |
| **reverse** | server, role `entry` (the restricted side) | client, role `exit` (the outside host) | server | client | when connections the restricted side opens outward are cut |

Both modes use the same carriers, handshake and sealing. `role` defaults to
`entry` on the server and `exit` on the client, which is reverse mode.
Which one works depends on the network in between. Try direct first; if the restricted side's
outbound connections stall or are cut, use reverse.

## Quick start

```bash
scripts/build.sh                                 # static linux/amd64 binary in bin/hstun
hstun keygen -out /etc/hstun/secret              # on the server; copy the same file to the client (0600)
hstun check -config /etc/hstun/server.toml       # validate before running
hstun server -config /etc/hstun/server.toml      # the listening host
hstun client -config /etc/hstun/client.toml      # the dialing host
```

Example configs and systemd units: `deploy/`. The secret is hex in a 0600 file;
hstun refuses to start with looser permissions and never prints it.

## Config summary

Examples: `deploy/server.example.toml` and `deploy/client.example.toml` (reverse),
`deploy/server-exit.example.toml` and `deploy/client-entry.example.toml` (direct).
Rules enforced by `hstun check`: an entry's forwards need `listen` and no `target`,
an exit's need `target` and no `listen`; `[socks]` only on an entry, `[socks_exit]`
only on an exit; SOCKS5 and admin listeners must be loopback (SOCKS only with
`allow_public = true` otherwise).

Server (`deploy/server.example.toml`): `role`, `listen`, `tls_cert`/`tls_key`, `carriers`,
`ws_path`/`http_path` (set your own; anything else is answered by the decoy site), `decoy_dir`, `secret_files` (several
allowed for rotation), `trust_proxy = "cloudflare"` + `cf_ranges_file` (or `"loopback"` behind a local nginx, see below)
(`CF-Connecting-IP` is trusted only from those ranges), connection and auth-failure
limits (`max_conns`, `max_conns_per_ip`, `auth_fail_per_min_per_ip`,
`forward_max_conns_per_ip` for the entry forward/SOCKS ports, optional
`allow_from` source CIDRs), `admin_listen` (`/stats`, loopback), `[[forward]]`, `[socks]`
or `[socks_exit]` depending on the role.

Client (`deploy/client.example.toml`): `role`, `server`, optional `connect` (dial an IP
while keeping the hostname for SNI/Host), `carrier`, `conns` (2 is enough; more connections did not measure faster), `dial_stagger`
(spreads the first dials, default `2s`), `user_agent`, `tls_verify`
(`system`, `ca_file` or `spki`; never off), `tls_mlkem` (keep false), `[[forward]]`
(`name` -> local `target`), `[socks_exit]` (`deny_private` refuses loopback,
private, CGNAT, link-local and the host's own addresses).

Forwards are matched by **name**: the server only names a forward, the client maps
it to a fixed local target from its own config. A compromised server cannot make
the client dial arbitrary addresses unless `socks_exit` is enabled.

## Behind Cloudflare

Cloudflare terminates TLS, so it sees the HTTP/WebSocket framing and the
handshake, but not the payload: after the handshake every byte is sealed with
AES-256-GCM under keys only the two hstun ends hold (see "Security"). What the
payload itself needs (VLESS+Reality, etc.) is a separate question; hstun no
longer relies on it for confidentiality against the CDN.
Use a proxied hostname plus an Origin Rule that rewrites the destination port to
hstun's port if the standard ports are taken. Set SSL mode Full (strict) with a
Cloudflare Origin Certificate for that hostname. Whether a given hostname is
reachable from your network through Cloudflare is something to measure first.

## Looking like ordinary HTTPS

Goal: to a passive observer and to an active prober, the server is a small HTTPS website, and the
client is a TLS client that behaves like one. The line stays at plain TLS plus ordinary HTTP/1.1 and
WebSocket: no imitation of another protocol, no browser TLS-fingerprint spoofing, no fake URL rotation.

What is done (each has a test):

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

What you should do when deploying: use a real domain with a CA-issued certificate (Let's Encrypt)
instead of a self-signed one, put real content in `decoy_dir`, use your own `ws_path`/`http_path`,
and use a normal HTTPS port (443 or a port that site would plausibly use). A self-signed
certificate is visible to anyone who connects and does not hold the pin.

What is **not** done, on purpose or for lack of a way to do it cleanly:

- The ClientHello is Go's own, so a JA3/JA4 fingerprint identifies "a Go program", not a browser.
  Matching a browser needs a TLS library that copies one (uTLS), which is imitation and a large
  third-party dependency; it is outside this project's line.
- No padding or timing shaping. The mux keepalive is a small record every `ping` interval, and bulk
  transfers produce records of the sizes the carriers write. A sophisticated traffic classifier could
  still separate this from web browsing by volume and timing.
- The SNI is sent in clear (TLS 1.3 without ECH); choose a hostname you control.

## Security

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

## Known limits

- A reset or timeout on one side of a stream reaches the other side as a clean EOF
  (yamux has no public reset), so a truncated transfer is not signalled by hstun; rely on
  the carried protocol (TLS, VLESS with its own framing) to detect that.
- Cloudflare peers are exempt from `max_conns_per_ip`; the global `max_conns` still applies.
- See "Looking like ordinary HTTPS" for what traffic analysis can still tell.

## Layout

```
cmd/hstun            CLI: server | client | keygen | check | version
internal/proto       handshake + stream open header (fuzzed)
internal/auth        key files, HMAC, HKDF
internal/seal        AES-GCM sealed stream (tamper/replay/reflection tests, fuzzed)
internal/netx        keepalive, idle conns, pipe, limit listener
internal/carrier     TLS client config; ws/ and httpmode/ carriers
internal/mux         yamux settings
internal/server      listening side: carriers, guard (per-IP, allow-list), limiter, /stats
internal/client      dialing side: slots, reconnect, optional /stats
internal/entry       forward + SOCKS listeners, hub that opens streams
internal/exit        serves streams: fixed targets, SOCKS exit ACL, stream cap
internal/socks5      CONNECT-only SOCKS5
internal/e2e         loopback integration, wedge, perf and idle tests
```

## Testing

Needs Go >= 1.26.11 (`mise install go@1.26`): older Go has stdlib CVEs that `govulncheck`
reports as reachable, and the scripts refuse to run with it.

```bash
scripts/test.sh            # vet + race tests + short fuzz (about 2 minutes)
scripts/test.sh -short     # skips the slow tests (wedge, throughput, 60 s idle)
scripts/build.sh           # static build and sha256
```

## Host tests

`scripts/e2e-matrix.sh` (run on the listening host) measures exit IP, a small HTTPS request,
5 MB down and 2 MB up through the SOCKS port; `scripts/wedge-host.py` holds
half-dead connections against a live server while timing a legitimate request;
`scripts/sec-probe.py` sends hostile input an unauthenticated client could send.

## Behind nginx

For a server that answers like an ordinary nginx site (nginx's TLS handshake, `Server: nginx`, nginx's own 404 and decoy
pages), put nginx in front: `deploy/nginx-front.example.conf` and `deploy/server-behind-nginx.example.toml`. nginx owns the
public port and the CA certificate, serves the decoy, and proxies only the two secret paths (WebSocket upgrades and POSTs) to
hstun on `127.0.0.1`. hstun runs with `trust_proxy = "loopback"`: it believes `X-Real-IP` only from a loopback peer (the config
refuses a non-loopback `listen` or an `allow_from` in this mode), so the auth-failure limiter still works per real client.
Without hstun's own per-IP cap, add a kernel connection cap (see the comment in the nginx example).
