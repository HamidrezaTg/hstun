# Cloudflare, cloudflared and the chunk carrier

## Behind Cloudflare (proxied hostname)

Cloudflare terminates TLS, so it sees the HTTP/WebSocket framing and the
handshake, but not the payload: after the handshake every byte is sealed with
AES-256-GCM under keys only the two hstun ends hold (see [security](security.md)). What the
payload itself needs (VLESS+Reality, etc.) is a separate question; hstun does not depend on it for confidentiality against the CDN.
Use a proxied hostname plus an Origin Rule that rewrites the destination port to
hstun's port if the standard ports are taken. Set SSL mode Full (strict) with a
Cloudflare Origin Certificate for that hostname. Whether a given hostname is
reachable from your network through Cloudflare is something to measure first.


## Behind cloudflared (Cloudflare Tunnel)

For an exit reached through a Cloudflare Tunnel run hstun on loopback and point the tunnel's ingress at it (`https://127.0.0.1:PORT` with `noTLSVerify`
or `http://` with empty `tls_cert`/`tls_key`). `trust_proxy = "cloudflared"` reads the real client IP from `CF-Connecting-IP`, only from a loopback peer
(the config refuses a non-loopback `listen` and `allow_from` in this mode), so `auth_fail_per_min_per_ip` and the per-IP limits work per client instead of
treating everyone as 127.0.0.1. See `deploy/server-cloudflared.example.toml`. Pair it with `carrier = "chunk"` ("Chunk carrier" below) for clients on a path that
cuts long connections to Cloudflare, or with `ws` where long connections survive.


## Chunk carrier: when a path cuts long connections

Some paths let only small connections through. On some networks (measured in November 2026 from a client host in Iran, to any Cloudflare customer hostname) a TLS
connection survives about 20 KB upstream and 48 KB downstream; beyond that the rest is silently swallowed. That is independent of
the IP and the port and is not fixed by ECH. `carrier = "chunk"` is the answer for such a path: it is the http carrier, but every
request uses its own TCP/TLS connection and carries at most `chunk_up_bytes` (default 12288) up and `chunk_down_bytes` (default 32768)
down, so no connection ever reaches the cut. `chunk_parallel` (default 8) uploads and downloads run at once. Server side nothing
changes except that the `http` carrier must be enabled; the client announces its budget in a request header.

What makes it work in practice (all found by measuring through Cloudflare from that client host):

- Requests are short-lived and some are swallowed by the path, so timeouts are short (3 s per request) and an upstream chunk that
  has not answered after 1 s is also sent on a second connection (the server deduplicates by sequence number). The server resends an
  unacked downstream chunk after 1.5 s instead of 3 s.
- A long-poll the client has abandoned can stay alive at the origin behind a CDN and swallow the next data, so the server answers
  polls before the client gives up (hold 3 s while the session is busy, 10 s when idle, client timeout 15 s).
- The carrier send buffer is limited to 192 KB in chunk mode (2 MB elsewhere); a bigger buffer queues seconds of upload in front of the
  downstream's window updates and starves downloads. The mux write timeout is 45 s for http and chunk sessions.
- Set `connect = "ip:443"` to an edge address that works from your side. From the measured client host IPv6 was blackholed and 188.114.98.0 lost about 15%
  of connections; 104.21.43.22 lost none.

Measured from that client host to a Cloudflare-fronted exit abroad (real HTTPS through the exit's SOCKS support): exit IP correct, small page 0.9 s,
5 MB download 8.8 s (about 4.6 Mbit/s), 2 MB upload 8.8 s, three parallel 5 MB downloads about 4.8 Mbit/s aggregate.
Single stream download about 5 Mbit/s, upload 7-10 Mbit/s. More `chunk_parallel` did not help.

Cost: many short TLS connections to the CDN (about 15-30 per second while busy) is a different and louder wire pattern than ws, and it is
slow. It adds no protocol imitation: standard TLS and HTTP, one fixed SNI, no random URLs or headers. Use it only where ws cannot work.
Tests: `internal/e2e` runs it through a proxy that cuts connections after 24 KB up and 48 KB down (the plain http carrier fails there),
through a 5% connection-swallowing, delayed path with four concurrent streams, and `internal/testutil.BudgetProxy` is the harness.

