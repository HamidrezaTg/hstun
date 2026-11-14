# Plan: match hstun to real HTTPS as far as it goes

Written 2026-11-14 after the third wire comparison (Chrome against hstun 0.5.0 and NipoVPN). It lists what still tells hstun apart from a browser talking to an
ordinary website, in the order we intend to close it, with an exit criterion per phase. The companion plan for the techniques themselves is
[stealth-plan.md](stealth-plan.md); this one is about **parity with one reference: Chrome on Linux desktop** (it matches the VPS operating system and the
User-Agent we send, and it is the browser we can capture on a laptop).

## Where we are (measured 2026-11-14 on live paths)

Closed: the JA4 cipher hash is identical to Chrome's, GREASE, shuffled extension order, GREASE ECH of the same four sizes, the X25519MLKEM768 key share, a real SNI and a
CA certificate on the ws path, nginx answers for strangers, irregular idle records instead of fixed keepalives.

Open:

| Gap | Why it matters |
|---|---|
| The hello copies Chrome 133 while Chrome is now 157: no ALPS, no `0xCA34`, a signature-algorithm list without Chrome's GREASE value and its three new entries | JA4_a reads `1515h1` against Chrome's `1517h2`, JA4_c is constant where Chrome's varies |
| ALPN is `http/1.1` only | a browser fetch offers `h2, http/1.1` |
| The chunk carrier makes 13-31 short TLS connections per second | a browser reuses a few connections |
| The decoy is a tiny page | a real site has assets, `robots.txt`, `favicon.ico` |
| Port and IP of the exit are not a clean 443 website | operational |
| TLS-in-TLS record pattern of carried HTTPS | visible to a size classifier |
| HTTP/2 behaviour of the front is only checked with curl | no reference trace of a browser yet |

## Phase 0: ground truth and a repeatable benchmark (about 1 day, no product code)

- `scripts/capture-chrome-hello.sh`: current Chrome (headless, own TLS listener) writes N ClientHellos to a file.
- A hello-diff tool and a golden test: parse Chrome's and hstun's hellos and print a row-by-row diff (extensions, signature algorithms, groups, ECH sizes, ALPN, JA4 a/b/c,
  size histogram). The test fails when hstun drifts from the stored Chrome sample.
- Measure instead of remembering: (a) the ALPN Chrome offers on a WebSocket connection and on a plain fetch to the same host; (b) the size of Chrome's resumed hello;
  (c) Chrome's HTTP/2 SETTINGS, WINDOW_UPDATE and pseudo-header order; (d) what extension `0xCA34` is (read from the capture and the Chromium sources);
  (e) a reference trace of Chrome browsing a small static site and a single-page app (record sizes, inter-arrival times, connection count) for the shape metrics.
- Turn the capture parser into `scripts/wire-compare.py` so every phase can regenerate the comparison table.

## Phase 1: ClientHello parity (1-2 days; highest value, lowest risk)

- Build the profile from a captured real hello instead of the library's Chrome 133 preset: uTLS `Fingerprinter.RawClientHello` turns the capture into a hello spec; unknown
  extensions become generic ones; GREASE and shuffling stay with the library. Store it as data with a refresh script. `chrome` becomes the latest capture, `chrome133` stays as
  the fallback name, the other profiles stay on the library presets until captured.
- Signature algorithms with the GREASE value and `0904-0906`, the `0xCA34` extension if confirmed, ALPS and ALPN exactly as Chrome does for the connection type found in Phase 0.
- Keep the User-Agent and `Sec-CH-UA` strings tied to the same Chrome major version; a test pins the pair.
- Exit criteria: the hello-diff shows no structural difference against the stored sample; JA4_a equals Chrome's for the same connection type; `hstun-ctl test --matrix` is still
  green on every real path (a bigger hello once broke a path).

## Phase 2: HTTP layer (3-4 days)

- If Chrome's WebSocket connections use `http/1.1` (Phase 0 decides), the ws carrier is already right. The http and chunk carriers are plain fetches, which a browser sends over
  HTTP/2: ALPN `h2, http/1.1`, Chrome's SETTINGS and WINDOW_UPDATE, pseudo-header and header order, HPACK behaviour (T16 of the stealth plan). `golang.org/x/net/http2` is not vendored
  and the standard library's bundled HTTP/2 does not expose SETTINGS; the options are vendoring `x/net/http2` and adjusting its framer, or a maintained Chrome-matching client.
  Large; done only if Phase 0 shows that `http/1.1` is a tell on those paths.
- Headers of tunnel requests: same-origin `Referer`, `Sec-Fetch-Site: same-origin`, cookie continuity from the decoy page (Phase 3), `Accept-Encoding` as Chrome sends it.
- Exit side: the nginx front keeps `http2 on` on the public side; the loopback hop stays plain; `server`, `date` and `etag` stay nginx's own.

## Phase 3: browse before tunnelling (2 days)

Before the first tunnel connection, and now and then afterwards, the client fetches the decoy site like a browser: `GET /`, its CSS, script and favicon with a `Referer`, cookies
kept, same SNI (over h2 once Phase 2 exists). Tunnel connections then look like a visitor's later requests, not the first and only thing from that address. Configurable
(`cover = "off|light"`), off for chunk because of its cost; the URL list comes from the decoy directory, never from random strings.

## Phase 4: traffic shape (3 days)

- Vision-style padding of the first records of each tunnelled stream (T12): hides the inner TLS handshake pattern; measured with the TLS-in-TLS metric.
- Tune `padding = strong` against the Phase 0 browsing reference (size histogram, inter-arrival), reporting a distance score per setting, not by feel.
- Chunk: a larger per-connection budget wherever the path allows it (adaptive tuning exists, T11), fewer and longer connections; report handshakes per second before and after.
- Ceiling: a long-lived bidirectional tunnel never matches browsing statistically. This narrows the gap; REALITY-style direct mode (T17) is a separate experiment.

## Phase 5: server and deployment parity (operational, needs decisions)

- Exit on port 443, with its own domain, on a clean IP that serves a real site: several pages and assets, `robots.txt`, `favicon.ico`, consistent headers; T20 TLS hygiene
  (unknown SNI gets the default decoy, session tickets on, no distinct default server).
- Direct mode (hstun's own TLS server answers): documented as weaker, since JARM and header order differ from nginx; nginx stays the recommended mode. Measure JARM for both.

## Phase 6: prove it

Run the arms B2-B6 of [stealth-plan.md](stealth-plan.md) on the real paths (and, if possible, a phone network and another ISP), regenerate the comparison with
`scripts/wire-compare.py`, publish the next edition, and record what worked as well as what changed. Keep the Chrome sample current (Chrome ships about monthly): the capture
script plus the golden test make drift visible.

## Out of scope (stated, not hidden)

The TCP/IP stack fingerprint (it comes from the VPS kernel), DNS side channels such as the HTTPS-record queries a browser sends, device diversity, a real classifier that we cannot
test, and the statistical difference between any tunnel and browsing.
