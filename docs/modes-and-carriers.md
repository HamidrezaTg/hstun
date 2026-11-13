# Modes and carriers

Two separate choices: how the **exit** is reached (mode), and how the **entry** talks to it (carrier).

## Which should I pick?

| situation | exit mode | carrier |
|---|---|---|
| You have a domain and can run nginx | `nginx` (recommended: looks like an ordinary website) | `ws` |
| You have only an IP address, or want the fewest parts | `direct` (self-signed certificate that the entry pins; with a domain you can use `--acme` or your own certificate) | `ws` |
| You already run a Cloudflare Tunnel and want no open port | `cloudflared` | `chunk` if the path from the entry cuts long connections to Cloudflare, otherwise `ws` |
| WebSocket is blocked on the path | any | `http` |

`ws` is the default and is chosen for you: the carrier is part of the pair code. Change it with `--carrier` on the entry.

**Not sure which carrier your path allows?** Install the entry with `--carrier auto`: it starts on `ws`, moves to `http` and then `chunk` after three failed attempts in a row, and tries the better ones again every 10 minutes (moving back as soon as one works). `hstun-ctl status` and the admin `/stats` show the carrier in use. Optional `auto_order = ["ws", "chunk"]` in `client.toml` narrows or reorders the list. The exit needs the `http` carrier enabled (it is by default).

## IPv4 and IPv6

On a host with working IPv6 the installer makes the exit listen on both families (`[::]`, dual-stack; nginx gets a second `listen [::]:PORT`) and puts the per-source connection cap on IPv6 too (per /64, because one host can use a whole /64). `--ipv6` forces it, `--no-ipv6` keeps IPv4 only. If the domain has an AAAA record the exit must listen on IPv6, otherwise clients that prefer IPv6 cannot connect: `hstun-ctl doctor` checks exactly that. Addresses in `--server`, `--connect` and `--public-addr` may be IPv6 literals in brackets (`[2001:db8::1]:443`).

## Exit modes (`--mode`)

| mode | what it does | you need |
|---|---|---|
| `nginx` | nginx owns the public TLS port, serves a static decoy site and proxies only the two secret paths to hstun on loopback; answers look like any nginx site | `--domain`, a certificate (`--cert/--key` or `--acme`) |
| `direct` | hstun itself listens on the public port | nothing: a self-signed certificate is made and pinned by the entry; or `--domain` with `--acme`/`--cert`; optional `--allow-from` |
| `cloudflared` | hstun listens on loopback, your Cloudflare Tunnel forwards to it | `cloudflared` with a tunnel; the installer prints the ingress lines |

An IP address with a self-signed certificate is the easiest setup to recognise. With a domain, `nginx` mode looks like an ordinary website.

## Carriers (`carrier` in the client config)

| carrier | how | use |
|---|---|---|
| `ws` (default) | one WSS connection per slot, yamux mux inside | fast; works through Cloudflare's proxy where long connections are allowed |
| `http` | keep-alive POSTs: `/u` upstream chunks, `/d` long-poll downstream, sequence numbers + ack | fallback where WebSocket is not allowed; slower |
| `chunk` | the http carrier with one TCP/TLS connection per request, each under a byte budget | paths that cut long connections (some networks cut connections to Cloudflare early); about 5 Mbit/s, noisier (see [advanced-cloudflare.md](advanced-cloudflare.md)) |

Loopback numbers with 100 ms RTT: ws ~10 MB/s each way, http ~1.4 MB/s each way.
Idle cost per minute: ws ~300 B, http ~6 KB.


## Two directions

The installer builds the normal arrangement: the entry **dials** the exit. hstun can also run the other way round (the exit dials the entry), for networks where connections opened from the restricted side are cut. That is manual setup only, see [config reference](config-reference.md): `role` and who listens are independent, with `deploy/server.example.toml` + `deploy/client.example.toml` for the reverse arrangement.

## nginx in front of hstun (details)

For a server that answers like an ordinary nginx site (nginx's TLS handshake, `Server: nginx`, nginx's own 404 and decoy
pages), put nginx in front: `deploy/nginx-front.example.conf` and `deploy/server-behind-nginx.example.toml`. nginx owns the
public port and the CA certificate, serves the decoy, and proxies only the two secret paths (WebSocket upgrades and POSTs) to
hstun on `127.0.0.1`. hstun runs with `trust_proxy = "loopback"` and, in that mode, may leave `tls_cert`/`tls_key` empty to serve plain HTTP on loopback (a second TLS layer measurably slowed downloads on a small VPS; the relay buffers in the nginx example matter too): it believes `X-Real-IP` only from a loopback peer (the config
refuses a non-loopback `listen` or an `allow_from` in this mode), so the auth-failure limiter still works per real client.
Without hstun's own per-IP cap, add a kernel connection cap (see the comment in the nginx example). Units: `deploy/hstun-nginx.service`, and `hstun-nginx-reload.{path,service,timer}` to reload nginx when the renewed certificate appears (edit the `PathChanged=` line).

