//go:build !linux

package netx

import (
	"net"
	"time"
)

func setUserTimeout(tc *net.TCPConn, d time.Duration) {}
