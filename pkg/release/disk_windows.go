//go:build windows

package release

// FreeSpaceBytes 在 Windows 客户端构建上返回充足可用空间；服务端/节点自升级仅在 Linux 上运行。
func FreeSpaceBytes(string) (uint64, error) {
	return MinStagingFreeBytes * 2, nil
}
