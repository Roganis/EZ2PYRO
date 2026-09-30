//go:build windows

package receiver

import (
	"net"
	"syscall"
	"unsafe"
)

func setRcvBuf(c *net.UDPConn, size int) int {
	_ = c.SetReadBuffer(size)
	got := 0
	if rc, err := c.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) {
			var v int32
			l := int32(unsafe.Sizeof(v))
			if syscall.Getsockopt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, (*byte)(unsafe.Pointer(&v)), &l) == nil {
				got = int(v)
			}
		})
	}
	return got
}
