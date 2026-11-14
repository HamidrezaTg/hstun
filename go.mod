module hstun

go 1.25

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
)

// hstun carries two small patches (Stream.Reset and reset-on-shutdown); see third_party/yamux/PATCHES.md.
replace github.com/hashicorp/yamux => ./third_party/yamux
