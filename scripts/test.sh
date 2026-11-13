#!/usr/bin/env bash
# vet + tests with the race detector + a short fuzz run of the wire parsers.
# Pass -short to skip the slow tests (wedge, throughput, 60 s idle).
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
go vet ./...
go test -race -count=1 "$@" ./...
for f in FuzzReadMsg FuzzReadOpen FuzzParseHello; do
  # Go's fuzzer sometimes reports "context deadline exceeded" when its time limit lands while a worker is mid-run
  # on a busy machine (no crashing input is saved). A real finding fails again and leaves a file in testdata/fuzz.
  go test ./internal/proto -run '^$' -fuzz "^${f}\$" -fuzztime 5s || go test ./internal/proto -run '^$' -fuzz "^${f}\$" -fuzztime 5s
done
