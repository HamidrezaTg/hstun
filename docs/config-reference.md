# Configuration reference

The installer writes these files for you (`/etc/hstun[-NAME]/server.toml` and `client.toml`). Edit them and run `systemctl restart hstun[-NAME]-server` (or `-client`), or use `hstun-ctl edit`.

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

Per-key limits and stopping (server; `0` = off):

| key | default | meaning |
|---|---|---|
| `max_mbit_per_key` | off | megabits per second, **each direction**, shared by every session of one secret key. A key that moves more than this is slowed down by ordinary TCP back-pressure; nothing is dropped |
| `max_new_streams_per_sec_per_key` | off | exit role: how many new streams the key may open per second (a burst of twice that, at least 10, is allowed). Over the rate the exit resets the stream, the entry fails that one connection; `/stats` counts `streams_throttled` |
| `drain_timeout` | `10s` | how long a stopping process lets the streams in flight finish. See "Stopping" in [operations](operations.md). `0s` stops at once; also a key of `[client]` |

Traffic shaping (see [security](security.md) and [stealth plan](stealth-plan.md)); the first three apply to the client, `padding` to both ends:

| key | default | meaning |
|---|---|---|
| `padding` | `light` | `off`, `light` (small records get 0-400 random bytes) or `strong` (the first 16 records look like 900-1400 byte ones, small ones get more). Used only when the peer announced that it reads padding, so a 0.4 or older peer sees no change; keepalives also carry a random-size filler |
| `tls_fragment` | `off` | `tcp` (the first TLS record leaves as small TCP segments a few ms apart), `record` (re-framed as several TLS records), `both`. Defeats filters that read only the first packet; adds a few ms per connection. **Measured 2026-11-14 from an Iran-side host to an nginx exit: `tcp` worked, `record` and `both` broke the tunnel (the session came up, data did not flow), probably something on the path reacting to a hello split across TLS records. Use `tcp`, and check any mode with `hstun-ctl test --matrix` first** |
| `session_age` | `auto` | replace a carrier session on a timer: `auto` is a random 10-40 minutes, `off`, or a duration (varied by +-25 %, at least 1m). The old session takes no new streams, finishes its own (up to 2 minutes), then closes. Needs `conns` of 2 or more; ignored by the chunk carrier |
| `disable_probe` | `false` | exit role: refuse the measuring streams `hstun probe` opens |

The mux keepalive interval is varied by +-40 % (never above 55 s), and the http carrier sends its session id and sequence numbers in cookies named from the secret `http_path`, not in `X-Hs-*` headers (a client or server that predates this still works with a new one).

These are per *key*, so with several entries that each have their own secret (several `secret_files`), one noisy entry cannot starve the others.

Client (`deploy/client.example.toml`): `role`, `server`, optional `connect` (dial an IP
while keeping the hostname for SNI/Host), `carrier` (`ws`, `http`, `chunk` or `auto`, which fails over ws → http → chunk; `auto_order` sets the list), `conns` (2 is enough; more connections did not measure faster), `dial_stagger`
(spreads the first dials, default `2s`), `user_agent`, `tls_verify`
(`system`, `ca_file` or `spki`; never off), `tls_fingerprint` (the ClientHello: `chrome` by default, `chrome_nopq`, `firefox`, `safari`, `ios`, `android` or `go`; see [security](security.md)), `tls_mlkem` (only for `tls_fingerprint = "go"`; default false; it worked to one host and broke the upgrade to another, so test each path before enabling; the `chrome` profile carries the post-quantum share like real Chrome, use `chrome_nopq` if a path cuts the large hello), `drain_timeout`, `[[forward]]`
(`name` -> local `target`), `[socks_exit]` (`deny_private` refuses loopback,
private, CGNAT, link-local and the host's own addresses).

Forwards are matched by **name**: the server only names a forward, the client maps
it to a fixed local target from its own config. A compromised server cannot make
the client dial arbitrary addresses unless `socks_exit` is enabled.


## Example files (`deploy/`)

| file | for |
|---|---|
| `server-exit.example.toml` + `client-entry.example.toml` | the normal arrangement: exit listens, entry dials |
| `server-behind-nginx.example.toml`, `nginx-front.example.conf`, `hstun-nginx*.{service,path,timer}` | exit behind nginx |
| `server-cloudflared.example.toml` | exit behind a Cloudflare Tunnel |
| `client-chunk.example.toml` | entry with the chunk carrier |
| `server.example.toml` + `client.example.toml` | reverse arrangement (the entry listens, the exit dials) |
| `decoy-example/` | static decoy site |
