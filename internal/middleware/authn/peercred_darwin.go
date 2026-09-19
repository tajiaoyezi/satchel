package authn

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID 用 LOCAL_PEERCRED 取 unix socket 对端进程的 uid。macOS 只在开发与测试时跑主控服务本体（serve 命令本身拒绝非 Linux）。
func peerUID(c *net.UnixConn) (uint32, bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, false
	}
	var (
		cred *unix.Xucred
		gerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil || gerr != nil || cred == nil {
		return 0, false
	}
	return cred.Uid, true
}
