# hstun

Carries TCP over an ordinary HTTPS connection. Everything rides authenticated,
end-to-end encrypted, multiplexed sessions, so a handful of TLS connections
carry any number of TCP streams. It runs in two modes (see "Modes").

Status: v0.2.12, pre-release, not independently reviewed. Tested with the race detector, a wedge test,
resets, added latency and a TLS-terminating tap; both modes were also run across real networks and
inspected with packet captures (payload never appeared in clear). The direct mode has also run
behind nginx on a real path, with TLS resumption seen on the wire, crash recovery and the black-box
probe (`scripts/sec-probe.py`) passing 16/16. Not yet covered: a real DPI classifier, a host reboot,
a long soak on that layout and a third-party review. See "Security".

## Install

One script installs and manages everything: the binary, the config, a service account, systemd units, the nginx front and its decoy site, the certificate
hook and the firewall rules. Run it as root on each host (`sudo -i` first):

```bash
bash <(curl -Ls https://raw.githubusercontent.com/HamidrezaTg/hstun/main/install.sh)
```

While the repository is **private** the script and its release need a token with read access (a fine-grained token with *Contents: read* is enough):

```bash
export GITHUB_TOKEN=...      # never paste it into a shared place
bash <(curl -Ls -H "Authorization: Bearer $GITHUB_TOKEN" https://raw.githubusercontent.com/HamidrezaTg/hstun/main/install.sh)
```

With no arguments it shows a menu. Everything it asks has a flag, so it also runs unattended (`--yes`). The first run installs the same script as `hstun-ctl`.

### Requirements

| What | Needed | Notes |
|---|---|---|
| OS | Linux with systemd | Debian 11+/Ubuntu 20.04+ tested; apt, dnf, yum and pacman are handled for the packages it pulls. Without systemd run the binary by hand ("Manual setup"). |
| CPU | x86-64 or arm64 | static binary, no libc dependency |
| Privileges | root | |
| Always | `curl`, `tar`, `openssl` (and `jq` for the release download) | installed automatically if missing |
| `nginx` mode | `nginx`, a domain that points at the host, a certificate | your own files (`--cert`, `--key`), or `--acme` (installs `certbot`, needs port 80 free) |
| `direct` mode | nothing extra | makes a self-signed certificate that the entry pins; or `--domain` with `--acme`/`--cert` |
| `cloudflared` mode | `cloudflared` with a tunnel | the installer prints the ingress lines to add; it does not edit cloudflared |
| Firewall | `ufw` (optional) | the installer opens the port and adds a per-source cap of 64 concurrent connections; with another firewall it tells you what to open |
| The inner proxy | something listening on `--forward-target` | hstun carries raw TCP to it, for example a VLESS/Reality inbound on `127.0.0.1` in x-ui/3x-ui or xray; **not** installed by hstun |
| Build from source | Go >= 1.26.11 | only with `HSTUN_BUILD=1`; the installer fetches the official toolchain; Go dependencies are vendored in `vendor/` |
| Private repository | `GITHUB_TOKEN` | not needed once the repository is public |

Ports: the exit needs its public TLS port open (and port 80 for certbot), the entry needs its forward port open if clients connect to it from outside.

### 1. The exit (the host abroad)

```bash
hstun-ctl exit        # or: bash <(curl ...) exit
```

Three ways to be reached (`--mode`):

| mode | what it does | you need |
|---|---|---|
| `nginx` (recommended) | nginx owns the public TLS port, serves a static decoy site and proxies only the two secret paths to hstun on loopback; answers look like any nginx site | `--domain`, a certificate |
| `direct` | hstun itself listens publicly; self-signed certificate pinned by the entry unless you give a real one | nothing |
| `cloudflared` | hstun listens on loopback behind your Cloudflare Tunnel; the real client IP is read from `CF-Connecting-IP` | a tunnel; use the `chunk` carrier on the entry |

Unattended examples:

```bash
# nginx front with an existing certificate; tunnelled TCP goes to a local VLESS inbound on 19990
hstun-ctl exit --yes --mode nginx --domain tunnel.example.org --port 443 \
  --cert /etc/letsencrypt/live/tunnel.example.org/fullchain.pem --key /etc/letsencrypt/live/tunnel.example.org/privkey.pem \
  --forward-target 127.0.0.1:19990

# the same, getting the certificate with certbot
hstun-ctl exit --yes --mode nginx --domain tunnel.example.org --acme --email you@example.org --forward-target 127.0.0.1:19990

# direct, self-signed, only one entry allowed in
hstun-ctl exit --yes --mode direct --port 2083 --allow-from 203.0.113.7/32 --forward-target 127.0.0.1:19990

# behind a Cloudflare Tunnel
hstun-ctl exit --yes --mode cloudflared --domain tunnel.example.org --backend-port 18444 --forward-target 127.0.0.1:19990
```

It prints a **join string** (everything the entry needs except the secret) and the command for the other host. `hstun-ctl show-join` prints it again.

### 2. The entry (the host in Iran)

```bash
hstun-ctl entry --join 'hstun-join:...' --secret-from root@EXIT-HOST
```

The secret is fetched over ssh straight into a 0600 file and never printed. Alternatives: `--secret-file FILE` (copy it yourself), or paste it when asked.
Useful flags: `--carrier ws|http|chunk` (`chunk` for a path that cuts long connections, such as Cloudflare from Iran), `--connect IP:443`
(dial a clean address instead of resolving the name), `--forward-listen 0.0.0.0:20151` (the client-facing port; the installer opens it in ufw),
`--socks 127.0.0.1:1080`, `--conns N`, `--tls-verify spki --spki HEX`. Then point your proxy clients at `ENTRY-HOST:20151`.

### Managing it

| command | |
|---|---|
| `hstun-ctl status` | units, sessions, streams, errors |
| `hstun-ctl logs [-f]` | journal of the instance |
| `hstun-ctl restart` | restart the instance's units |
| `hstun-ctl update [--all]` | download the latest release, check that it accepts every instance's config, replace the binary (previous one kept as `hstun.prev`), restart (`--all`: every instance) |
| `hstun-ctl adopt [--dry-run] [--unit U]` | bring a hand-installed setup under `hstun-ctl` (see below) |
| `hstun-ctl forget` | stop managing an adopted instance; it keeps running untouched |
| `hstun-ctl show-join` / `show-secret` | pairing information (exit) |
| `hstun-ctl uninstall [--purge]` | remove the units and firewall rules; `--purge` also deletes config, secret, decoy and, when no instance is left, the binary |

Several instances can live on one host (`--name NAME` on every command): each gets `/etc/hstun-NAME`, its own units and ports.
Installed files: `/usr/local/bin/{hstun,hstun-ctl}`, `/usr/share/hstun/` (examples, decoy), `/etc/hstun[-NAME]/` (config, secret, certificate, `install.conf`),
`/var/www/hstun[-NAME]-decoy`, units `hstun[-NAME]-{server,client,nginx,nginx-reload}`, user `hstunsvc`.

### Adopting a hand-installed setup

If you set hstun up by hand (your own systemd units and config under `/etc/hstun[-NAME]/`), `adopt` lets `hstun-ctl` manage it without
reinstalling anything:

```bash
bash <(curl ...) adopt --dry-run     # shows what it found, writes nothing
bash <(curl ...) adopt               # registers it
```

It scans `/etc/systemd/system/hstun*.service` for `ExecStart=... hstun[-x] server|client -config FILE`, checks that the binary accepts the
config, reads role, ports, paths and carrier from the config (and the nginx unit, its reload units and the domain for an nginx-fronted
exit), and writes one file, `/etc/hstun[-NAME]/install.conf`. Your units, configs, binaries and firewall rules are not touched. The config
directory must be `/etc/hstun` (default instance) or `/etc/hstun-NAME` with `server.toml` or `client.toml` inside; anything else is
skipped with a message. Unit and binary names may be custom (`hstun-chunk.service`, `/usr/local/bin/hstun-chunk`): they are recorded.
Afterwards `status`, `logs`, `restart`, `show-join`, `update` and `uninstall` work as for installed instances.
`update` first runs `hstun check` with the new binary on every config and stops before replacing anything if one is rejected; it replaces
the shared binary and any custom binary of an adopted instance and keeps the old ones as `*.prev`.
`install.sh exit|entry` refuses to run over an adopted or otherwise unmanaged `/etc/hstun[-NAME]` (it would rewrite your files): adopt it
or pick another `--name`. `uninstall` of an adopted instance removes its units (and with `--purge` its config directory and binary), but
leaves ufw rules and certificates, which `hstun-ctl` did not create.

### Offline, from source, and releases

- Offline: copy `hstun_VERSION_linux_ARCH.tar.gz` and run with `HSTUN_TARBALL=/path/to/it`.
- From source: `HSTUN_BUILD=1 hstun-ctl update` (or on the first run). It downloads the source, fetches Go if needed and builds with the vendored dependencies.
- Pin a version: `--version 0.2.12` (or `HSTUN_VERSION`). Another repository (a fork): `--repo OWNER/hstun` (or `HSTUN_REPO`).
- Maintainers: `scripts/package.sh` builds `dist/hstun_VERSION_linux_{amd64,arm64}.tar.gz` and `SHA256SUMS`;
  publish with `gh release create vVERSION dist/hstun_*.tar.gz dist/SHA256SUMS --repo OWNER/hstun`. The installer verifies the checksum.

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
| `chunk` | the http carrier with one TCP/TLS connection per request, each under a byte budget | paths that cut long connections (Iran to Cloudflare); about 5 Mbit/s, noisier (see "Chunk carrier") |

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

## Manual setup (without the installer)

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
allowed for rotation), `trust_proxy = "cloudflare"` + `cf_ranges_file` (or `"loopback"` behind a local nginx, or `"cloudflared"` behind a local cloudflared, see below)
(`CF-Connecting-IP` is trusted only from those ranges), connection and auth-failure
limits (`max_conns`, `max_conns_per_ip`, `auth_fail_per_min_per_ip`,
`forward_max_conns_per_ip` for the entry forward/SOCKS ports, optional
`allow_from` source CIDRs), `admin_listen` (`/stats`, loopback), `[[forward]]`, `[socks]`
or `[socks_exit]` depending on the role.

Client (`deploy/client.example.toml`): `role`, `server`, optional `connect` (dial an IP
while keeping the hostname for SNI/Host), `carrier` (`ws`, `http` or `chunk`), `conns` (2 is enough; more connections did not measure faster), `dial_stagger`
(spreads the first dials, default `2s`), `user_agent`, `tls_verify`
(`system`, `ca_file` or `spki`; never off), `tls_mlkem` (default false; it worked to one host and broke the upgrade to another, so test each path before enabling), `[[forward]]`
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
scripts/test-install.sh    # installer in a temp dir: exit + entry from the release tarball, echo through the tunnel, --purge (needs scripts/package.sh first)
scripts/test-adopt.sh      # adopt/update/forget/purge on hand-made layouts in a temp dir (needs scripts/package.sh first)
scripts/build.sh           # static build and sha256
```

## Host tests

`scripts/e2e-matrix.sh` (run on the listening host) measures exit IP, a small HTTPS request,
5 MB down and 2 MB up through the SOCKS port; `scripts/wedge-host.py` holds
half-dead connections against a live server while timing a legitimate request;
`scripts/sec-probe.py` sends hostile input an unauthenticated client could send.

## Behind cloudflared

For an exit reached through a Cloudflare Tunnel run hstun on loopback and point the tunnel's ingress at it (`https://127.0.0.1:PORT` with `noTLSVerify`
or `http://` with empty `tls_cert`/`tls_key`). `trust_proxy = "cloudflared"` reads the real client IP from `CF-Connecting-IP`, only from a loopback peer
(the config refuses a non-loopback `listen` and `allow_from` in this mode), so `auth_fail_per_min_per_ip` and the per-IP limits work per client instead of
treating everyone as 127.0.0.1. See `deploy/server-cloudflared.example.toml`. Pair it with `carrier = "chunk"` (README "Chunk carrier") for clients on a path that
cuts long connections to Cloudflare, or with `ws` where long connections survive.

## Chunk carrier: when a path cuts long connections

Some paths let only small connections through. From Iran to any Cloudflare customer hostname (measured 2026-11-13 from a client host in Iran) a TLS
connection survives about 20 KB upstream and 48 KB downstream; beyond that the rest is silently swallowed. That is independent of
the IP and the port and is not fixed by ECH. `carrier = "chunk"` is the answer for such a path: it is the http carrier, but every
request uses its own TCP/TLS connection and carries at most `chunk_up_bytes` (default 12288) up and `chunk_down_bytes` (default 32768)
down, so no connection ever reaches the cut. `chunk_parallel` (default 8) uploads and downloads run at once. Server side nothing
changes except that the `http` carrier must be enabled; the client announces its budget in a request header.

What makes it work in practice (all found by measuring through Cloudflare from a client host in Iran):

- Requests are short-lived and some are swallowed by the path, so timeouts are short (3 s per request) and an upstream chunk that
  has not answered after 1 s is also sent on a second connection (the server deduplicates by sequence number). The server resends an
  unacked downstream chunk after 1.5 s instead of 3 s.
- A long-poll the client has abandoned can stay alive at the origin behind a CDN and swallow the next data, so the server answers
  polls before the client gives up (hold 3 s while the session is busy, 10 s when idle, client timeout 15 s).
- The carrier send buffer is limited to 192 KB in chunk mode (2 MB elsewhere); a bigger buffer queues seconds of upload in front of the
  downstream's window updates and starves downloads. The mux write timeout is 45 s for http and chunk sessions.
- Set `connect = "ip:443"` to an edge address that works from your side. From the test client host IPv6 was blackholed and 188.114.98.0 lost about 15%
  of connections; 104.21.43.22 lost none.

Measured from that client host to a Cloudflare-fronted exit abroad (real HTTPS through the exit's SOCKS support): exit IP correct, small page 0.9 s,
5 MB download 8.8 s (about 4.6 Mbit/s), 2 MB upload 8.8 s, three parallel 5 MB downloads about 4.8 Mbit/s aggregate.
Single stream download about 5 Mbit/s, upload 7-10 Mbit/s. More `chunk_parallel` did not help.

Cost: many short TLS connections to the CDN (about 15-30 per second while busy) is a different and louder wire pattern than ws, and it is
slow. It adds no protocol imitation: standard TLS and HTTP, one fixed SNI, no random URLs or headers. Use it only where ws cannot work.
Tests: `internal/e2e` runs it through a proxy that cuts connections after 24 KB up and 48 KB down (the plain http carrier fails there),
through a 5% connection-swallowing, delayed path with four concurrent streams, and `internal/testutil.BudgetProxy` is the harness.

## Behind nginx

For a server that answers like an ordinary nginx site (nginx's TLS handshake, `Server: nginx`, nginx's own 404 and decoy
pages), put nginx in front: `deploy/nginx-front.example.conf` and `deploy/server-behind-nginx.example.toml`. nginx owns the
public port and the CA certificate, serves the decoy, and proxies only the two secret paths (WebSocket upgrades and POSTs) to
hstun on `127.0.0.1`. hstun runs with `trust_proxy = "loopback"` and, in that mode, may leave `tls_cert`/`tls_key` empty to serve plain HTTP on loopback (a second TLS layer measurably slowed downloads on a small VPS; the relay buffers in the nginx example matter too): it believes `X-Real-IP` only from a loopback peer (the config
refuses a non-loopback `listen` or an `allow_from` in this mode), so the auth-failure limiter still works per real client.
Without hstun's own per-IP cap, add a kernel connection cap (see the comment in the nginx example). Units: `deploy/hstun-nginx.service`, and `hstun-nginx-reload.{path,service,timer}` to reload nginx when the renewed certificate appears (edit the `PathChanged=` line).

## License

MIT, see `LICENSE`. The vendored libraries keep their own licenses, listed in `THIRD_PARTY_NOTICES.md` and shipped in
`/usr/share/hstun/licenses/` by the installer.
