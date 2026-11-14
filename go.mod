module hstun

go 1.26.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/coder/websocket v1.8.15
	github.com/hashicorp/yamux v0.1.2
	github.com/refraction-networking/utls v1.8.3-0.20260301010127-aa6edf4b11af
)

require (
	github.com/andybalholm/brotli v1.2.7 // indirect
	github.com/klauspost/compress v1.20.2 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
)

// hstun carries two small patches (Stream.Reset and reset-on-shutdown); see third_party/yamux/PATCHES.md.
replace github.com/hashicorp/yamux => ./third_party/yamux
