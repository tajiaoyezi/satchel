//go:build !linux && !darwin

package authn

import "net"

// peerUID 在其它平台上拿不到对端凭据：这些平台只保证客户端子命令（第 09 章），经 socket 进来的也按无身份处理。
func peerUID(*net.UnixConn) (uint32, bool) {
	return 0, false
}
