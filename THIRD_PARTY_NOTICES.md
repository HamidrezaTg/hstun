# Third-party software

hstun is released under the GNU Affero General Public License, version 3 or later (see `LICENSE`). The release binaries are built with the
following libraries, vendored in `vendor/` (yamux with two small patches, see below). Each keeps its own license file next to its source.

| Library | Version | License | License file |
|---|---|---|---|
| github.com/BurntSushi/toml | v1.6.0 | MIT | `vendor/github.com/BurntSushi/toml/COPYING` |
| github.com/coder/websocket | v1.8.15 | ISC | `vendor/github.com/coder/websocket/LICENSE.txt` |
| github.com/hashicorp/yamux | v0.1.2, patched | MPL-2.0 | `vendor/github.com/hashicorp/yamux/LICENSE` |

MPL-2.0 is file-level: if you change files of `yamux` itself you must publish those changes under the
MPL-2.0. We changed `stream.go` (a `Reset` method and reset-on-shutdown, marked "hstun patch" in the file; described in
`third_party/yamux/PATCHES.md`), and that file stays under the MPL-2.0. It does not affect the license of the rest of hstun.
The source of the patched copy is available in this repository (`third_party/yamux`, also vendored), which satisfies the
source-availability requirement for the binaries.

The Go standard library is BSD-3-Clause (https://go.dev/LICENSE).
