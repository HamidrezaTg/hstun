//go:build linux

package netx

import (
	"net"
	"syscall"
	"time"
)

// setUserTimeout sets TCP_USER_TIMEOUT: unacknowledged data older than d
// closes the connection instead of retransmitting for minutes.
func setUserTimeout(tc *net.TCPConn, d time.Duration) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, 0x12 /* TCP_USER_TIMEOUT */, int(d/time.Millisecond))
	})
}
