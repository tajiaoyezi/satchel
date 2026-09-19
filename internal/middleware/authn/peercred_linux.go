package authn

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID 用 SO_PEERCRED 取 unix socket 对端进程的 uid。
func peerUID(c *net.UnixConn) (uint32, bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, false
	}
	var (
		cred *unix.Ucred
		gerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || gerr != nil || cred == nil {
		return 0, false
	}
	return cred.Uid, true
}
