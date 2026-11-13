# Third-party software

hstun is released under the MIT License (see `LICENSE`). The release binaries are built with the
following libraries, vendored unmodified in `vendor/`. Each keeps its own license file next to its source.

| Library | Version | License | License file |
|---|---|---|---|
| github.com/BurntSushi/toml | v1.6.0 | MIT | `vendor/github.com/BurntSushi/toml/COPYING` |
| github.com/coder/websocket | v1.8.15 | ISC | `vendor/github.com/coder/websocket/LICENSE.txt` |
| github.com/hashicorp/yamux | v0.1.2 | MPL-2.0 | `vendor/github.com/hashicorp/yamux/LICENSE` |

MPL-2.0 is file-level: if you change files of `yamux` itself you must publish those changes under the
MPL-2.0. It does not affect the license of the rest of hstun. The source of the vendored copy is
available in this repository, which satisfies the source-availability requirement for the binaries.

The Go standard library is BSD-3-Clause (https://go.dev/LICENSE).
