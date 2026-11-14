# Security policy

hstun is pre-release software and has not been independently audited ([docs/security.md](docs/security.md) says what is
and is not covered). Reports of real weaknesses are welcome and taken seriously.

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private reporting instead:

> Repository → **Security** → **Report a vulnerability**
> (https://github.com/HamidrezaTg/hstun/security/advisories/new)

Only the maintainer sees it. Please include:

- what you found and what an attacker gains (read traffic, impersonate a host, crash or exhaust the exit, bypass a limit...);
- the hstun version (`hstun version`) and the mode and carrier in use;
- the smallest steps or config that reproduce it, or a proof of concept;
- whether you have told anyone else, and any date you plan to publish.

Never include a real secret, pair code or private key in a report. If a test needs one, generate a fresh one with `hstun keygen`.

## What to expect

hstun is maintained by one person in their spare time, so these are targets, not guarantees:

- an acknowledgement within about a week;
- a first assessment (accepted, needs more information, or not a vulnerability) within about two weeks;
- a fix or a mitigation for a confirmed issue as soon as it can be made and tested, released with an advisory that credits you
  if you want to be named.

Please give a reasonable time to fix before publishing; if you have not had an answer for a month, say so in the report.

## Supported versions

Only the latest release gets fixes. Update with `hstun-ctl update`; releases ship `SHA256SUMS`.

## In scope

- the handshake, the sealed stream and key derivation (`internal/proto`, `internal/seal`, `internal/auth`);
- anything that lets a peer without the secret get past the decoy, or that makes the server leak whether a path is valid;
- memory or connection exhaustion that the documented limits should prevent;
- the SOCKS exit's private-address filtering and the forward-by-name rule;
- `install.sh` / `hstun-ctl` doing something unsafe as root (file modes, firewall rules, secrets in logs or the process list).

## Out of scope

- traffic analysis and fingerprinting as such: hstun sends a browser-like ClientHello and a decoy site but does not claim to
  be indistinguishable from web browsing (see [docs/security.md](docs/security.md)); a concrete tell you found is welcome as a normal issue;
- attacks that need the secret, root on the host, or a modified binary;
- weaknesses in nginx, cloudflared, Cloudflare, your proxy (Xray and others) or the operating system;
- denial of service by sending more traffic than the host's bandwidth.

When in doubt, report it: it is easier to say "not a vulnerability" than to find out later.
