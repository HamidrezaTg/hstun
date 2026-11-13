# Contributing

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


## Installer and releases

`install.sh` is installed on hosts as `hstun-ctl`. Keep `INSTALLER_VERSION` equal to `var version` in `cmd/hstun/main.go`.
After changing it run `shellcheck -S warning install.sh scripts/*.sh`, then `scripts/package.sh && scripts/test-install.sh && scripts/test-adopt.sh && scripts/test-ctl.sh`.
Release: bump the version, `scripts/package.sh`, then `gh release create vX.Y.Z dist/hstun_*.tar.gz dist/SHA256SUMS --repo OWNER/hstun`; the installer downloads and verifies it.
Go dependencies are vendored (`go mod vendor` after changing `go.mod`); update `THIRD_PARTY_NOTICES.md` and the licenses copied in `scripts/package.sh` when they change.

## Guardrails

Plain transport engineering: real TLS and ordinary HTTP/WebSocket framing. No protocol imitation, no browser TLS-fingerprint copying, no fake URL rotation or DPI tricks.
Making the server look like an ordinary website (decoy, ALPN, resumption) is in scope. Every new read, write or handshake needs a deadline and a length cap; new listeners are loopback by default.
