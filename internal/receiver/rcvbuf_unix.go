//go:build !windows

package receiver

import (
	"net"
	"runtime"
	"syscall"
)

// setRcvBuf asks for a large receive buffer. On Linux the request is capped
// by net.core.rmem_max unless the process may use SO_RCVBUFFORCE.
func setRcvBuf(c *net.UDPConn, size int) int {
	_ = c.SetReadBuffer(size)
	got := 0
	if rc, err := c.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) {
			if runtime.GOOS == "linux" {
				cur, _ := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
				if cur < size {
					_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 33 /* SO_RCVBUFFORCE */, size)
				}
			}
			got, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
		})
	}
	return got
}
