# hstun: agent notes

Go module `hstun`.

- Toolchain: Go >= 1.26.11 via mise (`mise install go@1.26`; scripts find it with `mise where go@1.26`). Go 1.25.x has 11 stdlib CVEs in net/http, crypto/tls and crypto/x509 that govulncheck reports as reachable; never ship a build from it. Build once and copy the static binary to the hosts.
- Build: `scripts/build.sh`. Test: `scripts/test.sh` (add `-short` to skip the ~100 s of slow tests). Run both before committing.
- No secrets in git: `.gitignore` blocks `secret`, `*.key`, `*.pem` and every `*.toml` except `*.example.toml`. Keys are generated on the host with `hstun keygen` and never printed.
- Two modes: reverse (server=entry, client=exit) and direct (client=entry, server=exit). Check which one a host runs before touching it; see README "Modes".
- Security: everything after the handshake goes through `internal/seal`; never bypass it. New listeners must be loopback by default; new pre-auth input needs caps and a test.
- Keep it plain: real TLS and ordinary HTTP/WebSocket framing only. No protocol imitation, no browser TLS-fingerprint copying, fake URL rotation or DPI tricks. Making the server look like an ordinary website (decoy, ALPN, resumption) is in scope; see README "Looking like ordinary HTTPS".
- Every new read/write/handshake needs a deadline and a length cap; add a wedge-style test for anything that accepts input from the network.
