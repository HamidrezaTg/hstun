# Install reference

The quick start is in the [README](../README.md). This page lists everything the installer asks for and does.

## Requirements

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


## The exit (the outside host)

```bash
bash <(curl -Ls https://raw.githubusercontent.com/HamidrezaTg/hstun/main/install.sh) exit
```

The first run installs `hstun-ctl`; the commands below use it. On a host where it is not installed yet, run the one-line installer above with the same arguments.

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

It prints one ready-to-run command for the other host. The `hstun-pair:...` code in it holds everything the entry needs, including the secret, so send it only over a private channel. `hstun-ctl show-join` prints it again; `show-join --no-secret` prints the `hstun-join:...` form without the secret.


## The entry (the restricted host)

Paste the command the exit printed:

```bash
hstun-ctl entry --join 'hstun-pair:...'
```

That is all: the secret travels inside the pair code and is written to a 0600 file. If you were given a secret-less `hstun-join:...` string instead, the installer asks you to paste the secret (input hidden); `--secret-file FILE` and `--secret-from USER@HOST` (ssh) also work.
Useful flags: `--carrier ws|http|chunk` (`chunk` for a path that cuts long connections, for example when connections to Cloudflare are cut early), `--connect IP:443`
(dial a clean address instead of resolving the name), `--forward-listen 0.0.0.0:20151` (the client-facing port; the installer opens it in ufw),
`--socks 127.0.0.1:1080`, `--conns N`, `--tls-verify spki --spki HEX`. Then point your proxy clients at `ENTRY-HOST:20151`.


## All flags

| flag | applies to | meaning |
|---|---|---|
| `--yes` | all | never ask; use flags, saved settings and defaults |
| `--name NAME` | all | instance name (several instances per host): files in `/etc/hstun-NAME`, units `hstun-NAME-*` |
| `--mode nginx\|direct\|cloudflared` | exit | how the exit is reached, see [modes and carriers](modes-and-carriers.md) |
| `--forward-target HOST:PORT` | exit | where tunnelled traffic goes on the exit (your proxy) |
| `--domain D`, `--port N`, `--backend-port N` | exit | public name, public TLS port, loopback port behind nginx or cloudflared |
| `--cert FILE --key FILE`, `--acme [--email E]` | exit | your own certificate files, or a Let's Encrypt certificate through certbot |
| `--allow-from CIDR[,CIDR]` | exit (direct) | only these source addresses may connect |
| `--decoy-dir DIR`, `--public-addr ADDR` | exit | your own decoy site; the address printed in the pair code |
| `--ws-path P`, `--http-path P` | exit | secret paths (random by default) |
| `--join STRING` | entry | the pair code (or secret-less join string) printed by the exit |
| `--server HOST:PORT`, `--carrier ws\|http\|chunk\|auto`, `--tls-verify system\|spki`, `--spki HEX` | entry | only needed without a pair code |
| `--connect IP:PORT`, `--sni NAME` | entry | dial this address instead of resolving; send this TLS server name |
| `--forward-listen ADDR:PORT`, `--forward-name NAME` | entry | where your proxy clients connect; the forward's name (default `x1`) |
| `--conns N`, `--chunk-parallel N`, `--socks ADDR:PORT` | entry | parallel connections, chunk requests in flight, optional SOCKS5 listener |
| `--secret-file FILE`, `--secret-from USER@HOST[:PORT]` | entry | secret from a file, or fetched over ssh (rarely needed; the pair code carries it) |
| `--reconfigure`, `--reinstall`, `--new-cert`, `--customize` | exit/entry | change an installed instance; start from scratch; new self-signed certificate; ask the advanced questions |
| `--ipv6`, `--no-ipv6` | exit | listen on IPv6 too (dual-stack) or IPv4 only; default: detected |
| `--no-firewall` | all | do not touch ufw |
| `--version X.Y.Z`, `--repo OWNER/REPO`, `--force` | install/update | pin a release, use a fork, update even when current |

Environment: `GITHUB_TOKEN` (private fork), `HSTUN_TARBALL=FILE` (offline), `HSTUN_BUILD=1` (build from source), `HSTUN_VERSION`, `HSTUN_REPO`.

## Files the installer creates

`/usr/local/bin/{hstun,hstun-ctl}`, `/usr/share/hstun/` (examples, decoy, licenses), `/etc/hstun[-NAME]/` (config, secret, certificate, `install.conf`),
`/var/www/hstun[-NAME]-decoy`, units `hstun[-NAME]-{server,client,nginx,nginx-reload}`, service account `hstunsvc`.
