#!/usr/bin/env bash
# Build the release assets: static linux/amd64 and linux/arm64 tarballs plus SHA256SUMS, in dist/.
# Each tarball holds: hstun (the binary), share/ (installer, deploy examples, decoy example) and VERSION.
# Needs Go >= 1.26.11 (mise: `mise install go@1.26`). Dependencies are vendored, so no network is needed.
# Publish with:  gh release create "v$(cat dist/VERSION)" dist/hstun_*.tar.gz dist/SHA256SUMS --repo OWNER/hstun
set -euo pipefail
cd "$(dirname "$0")/.."
if G="$(mise where go@1.26 2>/dev/null)" && [ -x "$G/bin/go" ]; then export PATH="$G/bin:$PATH"; fi
MIN_GO=1.26.11
have="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
if [ -z "$have" ] || [ "$(printf '%s\n%s\n' "$have" "$MIN_GO" | sort -V | head -1)" != "$MIN_GO" ]; then
  echo "package: need Go >= $MIN_GO, found '${have:-none}'" >&2; exit 1
fi
VERSION="${VERSION:-$(sed -n 's/^var version = "\(.*\)"$/\1/p' cmd/hstun/main.go)}"
[ -n "$VERSION" ] || { echo "package: cannot read the version from cmd/hstun/main.go" >&2; exit 1; }
rm -rf dist; mkdir -p dist
for arch in amd64 arm64; do
  stage="dist/stage-$arch"; mkdir -p "$stage/share"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -mod=vendor \
    -ldflags "-s -w -X main.version=${VERSION}" -o "$stage/hstun" ./cmd/hstun
  cp install.sh "$stage/share/install.sh"
  cp -r deploy "$stage/share/deploy"
  cp -r deploy/decoy-example "$stage/share/decoy-example"
  # licenses travel with the binary (AGPL-3.0 for hstun, notices and license texts of the vendored libraries)
  mkdir -p "$stage/share/licenses"
  cp LICENSE THIRD_PARTY_NOTICES.md "$stage/share/licenses/"
  cp vendor/github.com/BurntSushi/toml/COPYING "$stage/share/licenses/BurntSushi-toml.COPYING"
  cp vendor/github.com/coder/websocket/LICENSE.txt "$stage/share/licenses/coder-websocket.LICENSE"
  cp vendor/github.com/hashicorp/yamux/LICENSE "$stage/share/licenses/hashicorp-yamux.LICENSE"
  cp vendor/github.com/refraction-networking/utls/LICENSE "$stage/share/licenses/refraction-networking-utls.LICENSE"
  cp vendor/github.com/andybalholm/brotli/LICENSE "$stage/share/licenses/andybalholm-brotli.LICENSE"
  cp vendor/github.com/klauspost/compress/LICENSE "$stage/share/licenses/klauspost-compress.LICENSE"
  cp vendor/golang.org/x/crypto/LICENSE "$stage/share/licenses/golang-x-crypto.LICENSE"
  cp vendor/golang.org/x/sys/LICENSE "$stage/share/licenses/golang-x-sys.LICENSE"
  echo "$VERSION" > "$stage/VERSION"
  tar -C "$stage" --owner=0 --group=0 -czf "dist/hstun_${VERSION}_linux_${arch}.tar.gz" hstun share VERSION
  rm -rf "$stage"
done
echo "$VERSION" > dist/VERSION
( cd dist && sha256sum hstun_*.tar.gz > SHA256SUMS && cat SHA256SUMS )
