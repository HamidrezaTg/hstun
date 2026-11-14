# Local changes to hashicorp/yamux v0.1.2

This directory is hashicorp/yamux v0.1.2 (MPL-2.0, see LICENSE) with two changes to `stream.go`,
both marked "hstun patch". `go.mod` at the repository root replaces the module with this directory.

1. `Stream.Reset()` sends an RST so the peer sees `ErrConnectionReset` instead of a clean EOF.
   Upstream only sends RST internally after the close timeout.
2. `forceClose()` (session shutdown) marks a stream whose peer had not finished sending as reset,
   so a reader of a stream cut by a dying session gets `ErrConnectionReset`, not a clean EOF.

To update yamux: copy the new release over this directory and re-apply the two hunks.

`session_test.go` has one adjusted assertion (`TestOpenStreamTimeout` accepts a reset stream as well as a closed one).
The tests run on their own: `cd third_party/yamux && go test ./...`.
The TLS connection type of the tests builds its certificate in memory (`testcert_test.go`) instead of loading `testdata/*.pem`, so no key file is committed.
