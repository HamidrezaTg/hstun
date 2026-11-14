# hstun

[English](README.md) · [فارسی](README.fa.md)

Carries TCP traffic through an ordinary HTTPS connection. Everything rides authenticated, end-to-end encrypted,
multiplexed sessions, so a few TLS connections carry any number of TCP streams. It uses
real TLS with a browser-like ClientHello (uTLS), a real WebSocket upgrade or real POST requests, an authenticated handshake and a decoy website. It is built to be hard for DPI to
recognise and block, and the [stealth plan](docs/stealth-plan.md) lists what is done and what is measured next.

```
 your users ──▶  ENTRY host           ══ HTTPS (WebSocket or HTTP) ══▶   EXIT host          ──▶  your proxy
                 (restricted side)       one pair of hosts, one secret      (outside)               (e.g. a VLESS inbound)
                 listens on :20151                                          listens on :443         on 127.0.0.1:19990
```

- **Exit**: the host outside the restricted network. It receives the tunnel and passes the traffic to your proxy.
- **Entry**: the host inside. Your users (or your proxy clients) connect to it.

Version 0.5.0. Pre-release, not independently audited. Details: [docs/security.md](docs/security.md).

## Quick start

Run as root (`sudo -i` first) on each host. One command, a menu, the same on both hosts:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/HamidrezaTg/hstun/main/install.sh)
```

1. **On the outside host** choose *Install an exit*. It asks which mode (nginx with a domain is recommended), where your proxy listens, and installs everything. At the end it prints **one command** for the other host.
2. **On the restricted host** run that command (it looks like `hstun-ctl entry --join 'hstun-pair:...'`; where `hstun-ctl` is not installed yet, the exit also prints the same command with the one-line installer in front). The code contains the secret, so send it over a private channel. Choose who connects to the entry (anyone, or only this host).
3. **Point your proxy clients at** `ENTRY-HOST:20151`. Check with `hstun-ctl test`.

Before step 1 you need a proxy listening on the exit, for example a VLESS inbound on `127.0.0.1:19990` in x-ui or xray: hstun carries the raw TCP stream to it and does not install it.
WebSocket (`ws`) is the default carrier and is chosen for you; see below if you need another.

## Which setup should I choose?

| situation | exit mode | carrier |
|---|---|---|
| You have a domain and can run nginx | `nginx` (recommended: looks like an ordinary website) | `ws` |
| Only an IP address, fewest parts | `direct` (self-signed certificate that the entry pins) | `ws` |
| You already use a Cloudflare Tunnel | `cloudflared` | `chunk` if long connections to Cloudflare are cut on your path, else `ws` |
| WebSocket is blocked on the path | any | `http` |

More: [docs/modes-and-carriers.md](docs/modes-and-carriers.md). Each carrier works on its own: change it on the entry with `--carrier ws|http|chunk`, or use `--carrier auto` to let the entry fail over between them by itself (in the menu: *Settings*; or `hstun-ctl edit --carrier http`).

## Daily use

| command | |
|---|---|
| `hstun-ctl` | menu: shows what is installed on this host and only the actions that make sense |
| `hstun-ctl status` / `test` / `doctor` | state; end-to-end check; checks of the host (firewall, certificate, DNS, clock...) |
| `hstun-ctl test --matrix` | (entry) measures every carrier x TLS fingerprint on the real path: echo sizes, idle gap, soak, speeds, and names the best row |
| `hstun-ctl logs [-f]` / `restart` | journal; restart |
| `hstun-ctl edit` | change settings; undone automatically if the tunnel does not come back |
| `hstun-ctl update` / `rollback` | latest release (rolls back by itself if it fails) / previous binary |
| `hstun-ctl show-join` | the pair code again (`--no-secret` for the version without the secret) |
| `hstun-ctl backup` / `restore FILE [--test]` | one private archive of every instance / check and restore it |
| `hstun-ctl rotate-secret` / `pin VERSION` | new secret without downtime / keep every host on one release |
| `hstun-ctl uninstall [--purge]` | remove an instance |

Everything has flags too, so it runs unattended (`--yes`): [docs/install.md](docs/install.md). Re-running the installer on an installed host asks
whether to keep, change or reinstall: the certificate, secret and paths survive a change, so paired entries keep working. Services you set up by hand
can be taken over with `hstun-ctl adopt` ([docs/operations.md](docs/operations.md)).

Against traffic analysis it also pads small records, varies the keepalive timing, replaces carrier sessions on a random timer, can split the first TLS record (`tls_fragment`), and keeps its HTTP metadata out of tool-specific headers; what each does and costs is in [docs/security.md](docs/security.md).

A restart is graceful: user ports close first and connections in flight get `drain_timeout` (10 s by default) to finish. A transfer that is cut (a reset on one side, or the tunnel dying) reaches the other side as a reset, not as a complete one. On the exit, `--max-mbit` and `--max-new-streams` cap what one secret key can use. See [docs/operations.md](docs/operations.md).

## Requirements

Linux with systemd (Debian/Ubuntu tested; dnf, yum and pacman are handled), x86-64 or arm64, root. `curl`, `tar`, `openssl` and `jq` are installed if missing.
`nginx` mode needs a domain that points at the host and a certificate (your files, or `--acme` with certbot and port 80 free). The installer opens its port in `ufw` and adds
a per-source connection cap; with another firewall (or a cloud firewall) you open the port yourself. Full table: [docs/install.md](docs/install.md).

## Something is wrong

`hstun-ctl doctor` first, then [docs/troubleshooting.md](docs/troubleshooting.md): no tunnel session, tunnel up but no traffic, SNI warning, port in use, certbot, failed update.

## Security in brief

- After a mutual HMAC handshake every byte is sealed with AES-256-GCM under keys only the two hstun ends hold; a CDN or proxy in between sees HTTP framing, not your data.
- The secret is a 0600 file, never printed, and the pair code is the only place it travels: treat the pair code like a password.
- The server answers anything that is not the two secret paths with an ordinary website (decoy). Connections, handshakes and auth failures are rate-limited per address.
- Found a vulnerability? Report it privately, see [SECURITY.md](SECURITY.md).
- The client sends the ClientHello of a real browser (`tls_fingerprint`, Chrome by default; `go` turns it off), so a JA3/JA4 fingerprint does not say "a Go program". hstun still does not hide that you use HTTPS to a server; read [docs/security.md](docs/security.md) for what an observer can still tell.

## More documentation

[docs/install.md](docs/install.md) (every flag, files created) · [docs/operations.md](docs/operations.md) (commands, adopt, offline, releases) ·
[docs/modes-and-carriers.md](docs/modes-and-carriers.md) · [docs/advanced-cloudflare.md](docs/advanced-cloudflare.md) (Cloudflare, cloudflared, chunk) ·
[docs/config-reference.md](docs/config-reference.md) (manual setup, config keys) · [docs/security.md](docs/security.md) · [docs/stealth-plan.md](docs/stealth-plan.md) (what is done against DPI and how it is benchmarked) · [docs/troubleshooting.md](docs/troubleshooting.md) ·
[CONTRIBUTING.md](CONTRIBUTING.md)

## License

Copyright (C) 2026 Hamidreza. GNU Affero General Public License, version 3 or later (`LICENSE`). If you run a modified
hstun as a network service, section 13 requires you to offer its source to the people who use that service. The vendored libraries keep their own licenses, listed in `THIRD_PARTY_NOTICES.md` and shipped in `/usr/share/hstun/licenses/` by the installer.
