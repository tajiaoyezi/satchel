//go:build unix

package release

import "syscall"

// FreeSpaceBytes 返回 dir 所在文件系统非特权进程可用的字节数。
func FreeSpaceBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	if st.Bavail == 0 || int64(st.Bsize) <= 0 {
		return 0, nil
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
