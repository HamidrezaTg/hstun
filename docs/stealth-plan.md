# Stealth plan

How hstun gets harder to detect and block, and how every step is measured before it becomes a default.
Written 2026-11-14 after a source study of Xray-core, the 3x-ui panel, BackPack, C-Tunnel, NipoVPN and
GhostWire / GhostTunnel. Status of each item is in the last column of the tables; "done" means merged and tested,
"measure" means built but not yet proven on a real path.

## 1. What we are up against

What the Iran-side classifier is known or suspected to look at, from our own tests (an Iran-side host to Cloudflare-fronted names,
BackPack, C-Tunnel, NipoVPN, hstun over 2026-11-10 to 2026-11-13) and from what the other projects report:

| signal | what we know | where hstun stood |
|---|---|---|
| SNI in clear | name based blocking; some well known names (google, cloudflare) are filtered; ECH did not help on one path | fixed name, our own domain |
| ClientHello fingerprint (JA3/JA4) | every serious client in this space (Xray, BackPack) sends a browser hello; plain Go is recognisable | **was Go's own; now a browser profile (done)** |
| bytes per connection | to some Cloudflare names a flow dies after about 24 KB up / 48 KB down, depending on SNI not IP | `chunk` carrier works around it |
| direction | flows that start in Iran are cut after a few packets on some paths; flows into Iran work | direct mode from an Iran-side host to an nginx exit works; the Cloudflare path does not |
| active probing | a prober that connects without the secret must see an ordinary site | decoy site; needs a byte-identical audit |
| traffic shape | fixed-period empty keepalives, constant record sizes, many short connections at a steady rate | fixed 25 s ping, no padding, chunk ~30 connections/s under load |
| TLS inside TLS | an inner TLS handshake (VLESS+Reality, https sites through the tunnel) leaves a recognisable pattern of record lengths | not addressed |

Everything after the handshake is encrypted by hstun; the plan is about what is left visible: the hello, the
timing and sizes, the connection pattern, and how the server answers strangers.

## 2. What the other projects do

| project | stealth mechanisms that exist | verdict for hstun |
|---|---|---|
| **Xray-core** | uTLS Chrome hello by default and ALPN rewritten to http/1.1 for WebSocket; REALITY (borrowed real site, bad clients forwarded to it); Vision padding of the first inner packets; XHTTP padding, connection rotation with random ranges; TLS fragmentation of the first record; pinned cert hash and verify-by-name; fallbacks to a real site | the richest source. Adopt: hello (done), fragment, padding, rotation, Vision-style padding, pinning. REALITY only as a separate direct-mode arm |
| **3x-ui** | a front end for Xray: it exposes the above (`fp`, `alpn`, `ech`, `fragment`, `mode`, padding) and writes them into subscription links | confirms which knobs operators actually use and which defaults are recommended (section 6) |
| **BackPack** (AGPL-3.0; code reusable under our licence with notices) | uTLS Chrome on WSS; token-derived fake nginx page; Noise "Stealth" with random filler inside records; a connection test that soaks every transport for 60 s and picks the fastest that survived; our measurements: Iran-initiated flows are cut, Iran as passive side works | adopt: soak-based selection, keep Iran passive, token-derived decoy. Skip: SNI/IP spoofing, PCK, QUIC |
| **C-Tunnel** (AGPL-3.0; code reusable under our licence with notices) | not an HTTPS tool: stub headers copied from DTLS/QUIC/RTP/DNS, fake `17 03 03` frames without a handshake, 0-16 bytes padding, silent drop on bad auth | hstun is already ahead on camouflage. Adopt the test design only: direction, size escalation, ok/unstable/blocked, per-direction speed. Its DIRECT failure may be a firewall on the exit host, not Iran |
| **NipoVPN** | decoy HTTP over plain OpenSSL (random method, path, Host, SNI), AES-GCM body, no multiplexing, no fingerprinting; its useful accident is one short connection per request | hstun's `chunk` carrier already took that. Adopt: move carrier metadata out of the plain `X-Hs-*` headers into the sealed body. Skip: random SNI/Host, hex or base64 bodies, polling at 1-50 ms |
| **GhostWire / GhostTunnel** | CDN front with client-side `resolve_ip`, `sni` and `host_header` overrides; Google Apps Script fronting; random padding per frame (GhostTunnel); no TLS fingerprinting; no probe resistance | adopt: uTLS (done), probe audit, padding, lifetime randomisation. Optional emergency carrier: Apps Script relay |

## 3. The plan

Each item has an id used by the benchmark. "Effort" is a rough estimate for one person.

### P0: done in this change

| id | item | status |
|---|---|---|
| T1 | browser ClientHello through uTLS (`tls_fingerprint`; chrome, chrome_nopq, firefox, safari, ios, android, go), http/1.1-only ALPN, no ALPS, session resumption, User-Agent that matches | done, hello checked by test (JA4 cipher hash of Chrome) and against four public sites (chrome, chrome_nopq, firefox pass; safari failed once); **default checked on two Iran-side paths (benchmark B1: no breakage); the survival benefit is not shown yet** |
| T0 | written guardrails removed (AGENTS.md, docs, templates) | done |

### P1: cheap, high value (about a day each)

| id | item | from | effort | expected effect | cost | status |
|---|---|---|---|---|---|---|
| T2 | split the first TLS record (ClientHello) into several TCP segments or TLS records (`tls_fragment`: off, tcp, record, both) | Xray `fragment` | 0.5 d | beats SNI filters that do not reassemble | a few ms on connect | **built, off by default. B2 on one Iran-side path: `tcp` passes, `record` and `both` break the tunnel, so only `tcp` is a candidate** |
| T3 | jittered keepalive interval (+-40 %) and random-size filler with each ping | C-Tunnel, BackPack lessons | 0.5 d | removes the fixed-period heartbeat | none | **built, on; measure (B4)** |
| T4 | carrier metadata out of `X-Hs-Sid/Seq/Ack/Tag/Mx` into cookies named from the secret path (old and new ends interoperate) | NipoVPN | 1 d | a CDN or middlebox no longer sees a tool signature | none | **built, on** |
| T5 | browser-like header set on the HTTP carriers (Accept, Accept-Language, Origin, Sec-Fetch-*, Sec-CH-UA per profile; no ordering: Go sorts headers) | Xray `browser.go` | 0.5 d | only visible after TLS termination (CDN path) | none | **built, on** |
| T6 | padding: random filler on small records, the first 16 records shaped to 900-1400 B in `strong` (`padding`), negotiated with the peer | Xray XHTTP, GhostTunnel, BackPack Stealth | 1-2 d | flattens size patterns | light: a few KB per minute; strong: more | **built, `light` on; measure (B3)** |
| T7 | connection lifetime randomisation (`session_age`: replace sessions after a random 10-40 min, old one drains) | Xray xmux, GhostWire | 1 d | removes constant-lifetime flows | a reconnect | **built, on for conns >= 2; measure (B4)** |
| T8 | probe-resistance audit: a stranger gets bytes and timing identical to the decoy on every path (wrong path, wrong method, bad hello, half-open) | GhostWire review, Xray fallback | 1 d | closes active-probing tells | none | **done for bodies and headers (the http carrier errors were plain text and are now HTML pages, tested); timing of rejected handshakes not compared yet** |
| T9 | `hstun-ctl test --matrix` and `hstun probe`: carrier x fingerprint (and fragment) with size escalation, idle gap, soak, per-direction speed, stranger check, JSON | BackPack, C-Tunnel | 2 d | the measuring tool for everything else | none | **built (direct mode; the port and direction axes are not in it)** |
| T10 | `carrier = "auto"` decides by soak (a session must stay up 20 s), not only by dial success | BackPack | 1 d | a carrier that connects and then stalls counts as failed | probe traffic | **built; tested with a path that cuts websockets** |
| T11 | adaptive chunk budget: a stuck session shrinks the next one by a third, a session that ran 3 minutes grows it by a tenth | NipoVPN review | 1 d | survives changing per-connection cuts | none | **built, between sessions (an already numbered chunk cannot be cut smaller, so shrinking inside a session would never unstick it)** |

### P2: medium (several days each)

| id | item | from | expected effect | cost |
|---|---|---|---|---|
| T12 | Vision-style padding of the first 8 packets of every tunnelled stream | Xray Vision | hides the TLS-in-TLS record pattern of an inner VLESS/TLS | a few KB per stream |
| T13 | pinned certificate plus verify-by-name, SNI and Host decoupled, clean-IP list | Xray, GhostWire | prerequisite for fronting through a CDN path | none |
| T14 | ECH on the client (needs a DoH lookup of the ECH config) | Xray | hides the SNI, if the path does not block ECH | one DoH query |
| T15 | forward-secret handshake (X25519 plus a pinned Ed25519 server key) instead of a static secret only | GhostWire review | a leaked secret no longer decrypts past traffic | one more round of crypto |
| T16 | real HTTP/2 option for the http carrier with a browser's SETTINGS | Xray (which does not do it) | an h2 fingerprint that matches | large; only if the benchmark says h1 is a tell |

Two more from the panel study:

| id | item | from | expected effect | cost |
|---|---|---|---|---|
| T20 | server TLS hygiene in the nginx front and the direct listener: unknown SNI rejected, session tickets on, one default server that behaves like the decoy | 3x-ui, Xray | strangers and scanners that do not know the name get nothing distinctive | none |
| T21 | optional pools for the client: several SNI names and several resolved IPs of the front, picked per connection from the operator's own list (not random strings) | 3x-ui hosts | spreads blocking by name or address | config only |

### P3: experiments, separate arms, not defaults

| id | item | note |
|---|---|---|
| T17 | REALITY in direct mode (importable server library, about 60 lines of client on uTLS) | replaces nginx and CDN; TCP only; the library needs Go 1.27 |
| T18 | Google Apps Script relay as an emergency carrier | slow, quota limited, policy risk; the operator already has a Drive based path |
| T19 | fake ClientHello with a low TTL (SNI desync) | needs raw sockets and root; fragile |

Not planned: QUIC / KCP / UDP carriers (every test from Iran failed), IP or SNI spoofing, packet-level carriers (PCK,
ICMP), random SNI or Host per request (it fingerprints), hex or base64 body armour (33-167 % overhead), tools that update
themselves as root.

## 4. How it is benchmarked

A technique becomes a default only when it helps and does not cost too much. Three kinds of measurement, all scripted so
the numbers can be repeated.

### A. Wire (offline, repeatable on one machine)

Capture with `tcpdump` on the client host while a fixed workload runs (a page-like mix of small requests, a 20 MB
download, a 10-minute idle period with a few requests). Compare against a **reference capture of a real browser** doing
the same against the same kind of site.

| metric | how | goal |
|---|---|---|
| ClientHello | JA3, JA4 (cipher hash, extension set, ALPN, groups), record size | equal to the reference browser profile |
| first 10 packets | sizes and directions | inside the reference spread |
| record-size distribution | histogram, KS distance to the reference | smaller than the baseline arm |
| idle pattern | inter-arrival times of keepalives | no fixed period |
| connection pattern | connections per minute, lifetime | like the reference |
| active probe | wrong path / method / hello / secret: compare bytes and timing with the decoy | identical |
| TLS in TLS | run a VLESS+TLS inner stream; test whether the first inner records have the handshake length pattern | pattern gone with T12 |

### B. Path (survival, from the real network)

Run `hstun-ctl test --matrix` from the restricted host to each exit, three times on different days, and record
ok / unstable / blocked per row:

- rows: carrier (ws, http, chunk) x fingerprint (go, chrome, chrome_nopq, firefox) x port x who dials;
- each row: handshake, size escalation (4 KB to 1 MB up and down), 60 s idle then a request, 5-minute soak at 1 echo per
  second, bulk 20 MB each way, reconnect after a forced drop, speed **per direction** (downloads decide the ranking);
- a plain-`curl` prober and a wrong-secret client against the same port, to check what a stranger sees.

### C. Cost

CPU and memory of both ends, bytes overhead (wire bytes / payload bytes), added latency of connect and of a 1 KB
request, throughput of a 100 MB transfer. Measured with the same tool so every arm is comparable.

### Decision rule

1. Survival first: a change that lowers the pass rate on a real path is rejected or made opt-in, whatever the wire score.
2. Then wire: it must move at least one wire metric toward the reference without moving another away.
3. Then cost: overhead above 10 % bytes or 20 % throughput needs an explicit reason and stays off by default.
4. Defaults are what wins on the paths the operator actually uses; the rest stays an option.

### First arms (what to run before anything else)

| arm | question |
|---|---|
| B1 | `go` vs `chrome` vs `chrome_nopq` vs `firefox` on each real path: does the browser hello pass everywhere the Go hello did, and where does the large post-quantum hello fail? (decides the default) |
| | **B1 result (2026-11-14, one Iran-side host, 8 rounds):** all four profiles came up and fetched 252 KB through a direct nginx exit and through Cloudflare with no failures that depended on the profile and the same speed; the post-quantum hello did not break either path. The default stays `chrome`, `chrome_nopq` the fallback. These paths did not block the Go hello either, so the survival benefit is not shown yet: repeat on other ISPs and mobile networks. |
| B2 | the first-record fragmentation (T2) on top of the winner |
| B3 | padding off / light / strong (T6) on the wire metrics and on cost |
| B4 | keepalive jitter and lifetime rotation (T3, T7) on the idle pattern |
| B5 | Vision-style padding (T12) on the TLS-in-TLS metric |
| B6 | REALITY direct (T17) as a separate arm against nginx plus ws |

## 5. Risks

- The browser profiles come from the pinned uTLS snapshot (Chrome 133, Firefox 148, Safari 26.3). A profile that is many
  versions old becomes a tell of its own; update the library with each release and re-run the wire tests.
- uTLS copies the ClientHello only. HTTP/2 settings, header order after TLS, and timing are separate things (T5, T16).
- The Chrome profile carries the X25519MLKEM768 key share, which makes the hello about 1.5 KB. One earlier path broke
  on a large hello; `chrome_nopq` is the fallback and B1 decides the default.
- Newer uTLS snapshots have a fresher Chrome profile (Chrome 155 in the 2026-11 snapshot) but need Go 1.27, which the
  build toolchain does not have yet; switch when it does and re-run B1.
- Padding and shaping cost bandwidth; they must stay measurable and switchable.

## 6. Notes from the 3x-ui panel study

3x-ui is a configuration generator over Xray-core, so the wire-level stealth is Xray's. What the panel adds is useful as a
list of what operators switch on and what the defaults are:

- client links carry `fp` (default chrome), `alpn` (`h2,http/1.1`, and `http/1.1` only for WebSocket), `ech`, pinned
  certificate hash and verify-by-name, plus mode, padding and fragment settings. Chrome is the only fresh uTLS preset; the
  others (edge, ios, android, 360, qq) are stale there too, which agrees with our caution about `safari`, `ios`, `android`.
- the panel strips a constant 30 ms interval between XHTTP posts because it was fingerprinted: **constant sizes and
  intervals are tells**, which is the reasoning behind T3, T6 and T7.
- server hygiene it recommends: reject unknown SNI, keep session tickets on (off means "not a browser"), pin the certificate
  instead of skipping verification (T20).
- panel hardening (random paths, a bare 404 for unknown routes, rate-limited login) matters for the panel host, not for
  hstun's data path.
- SNI and IP pools per subscription fetch (T21) are client configuration only.
- gaps it shares with Xray: no HTTP/2 SETTINGS impersonation and no timing shaping.
