# Third-party software

hstun is released under the GNU Affero General Public License, version 3 or later (see `LICENSE`). All the licenses below (MIT, BSD, ISC, MPL-2.0) allow being combined with an AGPL work. The release binaries are built with the
following libraries, vendored in `vendor/` (yamux with two small patches, see below). Each keeps its own license file next to its source.

| Library | Version | License | License file |
|---|---|---|---|
| github.com/BurntSushi/toml | v1.6.0 | MIT | `vendor/github.com/BurntSushi/toml/COPYING` |
| github.com/coder/websocket | v1.8.15 | ISC | `vendor/github.com/coder/websocket/LICENSE.txt` |
| github.com/hashicorp/yamux | v0.1.2, patched | MPL-2.0 | `vendor/github.com/hashicorp/yamux/LICENSE` |
| github.com/refraction-networking/utls | v1.8.3 (snapshot 2026-03-01) | BSD-3-Clause | `vendor/github.com/refraction-networking/utls/LICENSE` |
| github.com/andybalholm/brotli | v1.2.7 | MIT | `vendor/github.com/andybalholm/brotli/LICENSE` |
| github.com/klauspost/compress | v1.20.2 | BSD-3-Clause (some files Apache-2.0 / MIT, see the file) | `vendor/github.com/klauspost/compress/LICENSE` |
| golang.org/x/crypto, golang.org/x/sys | v0.58.0, v0.49.0 | BSD-3-Clause | `vendor/golang.org/x/crypto/LICENSE`, `vendor/golang.org/x/sys/LICENSE` |

MPL-2.0 is file-level: if you change files of `yamux` itself you must publish those changes under the
MPL-2.0. We changed `stream.go` (a `Reset` method and reset-on-shutdown, marked "hstun patch" in the file; described in
`third_party/yamux/PATCHES.md`), and that file stays under the MPL-2.0. It does not affect the license of the rest of hstun.
The source of the patched copy is available in this repository (`third_party/yamux`, also vendored), which satisfies the
source-availability requirement for the binaries.

The Go standard library is BSD-3-Clause (https://go.dev/LICENSE).
