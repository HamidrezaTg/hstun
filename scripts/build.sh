#!/usr/bin/env bash
# Static linux/amd64 build. Run on the laptop; the hosts only run the binary.
# Needs Go (mise: `mise install go@1.26`; 1.25.x has 11 unfixed stdlib CVEs, build with >= 1.26.11).
set -euo pipefail
cd "$(dirname "$0")/.."
# A mise shim may exist without a version set, so test that go really runs.
if G="$(mise where go@1.26 2>/dev/null)" && [ -x "$G/bin/go" ]; then
  export PATH="$G/bin:$PATH"
fi

# Refuse an old toolchain: Go < 1.26.11 has stdlib CVEs (net/http, crypto/tls, crypto/x509)
# that govulncheck reports as reachable from this code.
MIN_GO=1.26.11
have="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
if [ -z "$have" ] || [ "$(printf '%s\n%s\n' "$have" "$MIN_GO" | sort -V | head -1)" != "$MIN_GO" ]; then
  echo "build: need Go >= $MIN_GO, found '${have:-none}' (mise install go@1.26)" >&2
  exit 1
fi
mkdir -p bin
VERSION="${VERSION:-0.2.12}"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o bin/hstun ./cmd/hstun
sha256sum bin/hstun
