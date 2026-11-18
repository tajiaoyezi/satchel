package release

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MinStagingFreeBytes 是下载升级包之前要求暂存目录至少拥有的可用磁盘空间（128 MiB，第 09 章硬条件）。
const MinStagingFreeBytes uint64 = 128 << 20

// InDocker 判断是否在容器环境里运行：root 为 "/" 时额外看 env DOCKER=1，再检查 .dockerenv 与 proc/1/cgroup。
func InDocker(root string) bool {
	if root == "/" && os.Getenv("DOCKER") == "1" {
		return true
	}
	if _, err := os.Stat(filepath.Join(root, ".dockerenv")); err == nil {
		return true
	}
	data, err := os.ReadFile(filepath.Join(root, "proc", "1", "cgroup"))
	if err != nil {
		return false
	}
	s := string(data)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd") || strings.Contains(s, "kubepods")
}

// CheckFreeSpace 验证 dir 所在文件系统的可用空间不低于 minBytes；probe 为空时调用平台实现的 FreeSpaceBytes。
func CheckFreeSpace(dir string, minBytes uint64, probe func(string) (uint64, error)) error {
	if probe == nil {
		probe = FreeSpaceBytes
	}
	avail, err := probe(dir)
	if err != nil {
		return fmt.Errorf("检查目录 %s 可用空间失败：%w", dir, err)
	}
	if avail < minBytes {
		return fmt.Errorf("暂存目录 %s 可用空间不足：%d MiB（至少需要 %d MiB）", dir, avail>>20, minBytes>>20)
	}
	return nil
}

// PreviousPath 是目标路径的上一版二进制路径（加 .bak）。
func PreviousPath(target string) string { return target + ".bak" }

// SavePrevious 把目标路径上的二进制复制成 .bak（落盘）：复制而不是改名，替换之前目标路径上一直有完整可用的二进制。
func SavePrevious(target string) error {
	src, err := os.Open(target)
	if err != nil {
		return err
	}
	defer src.Close()
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Dir(target), PreviousPath(target), data, 0o755)
}

// Install 把 staged（与目标同目录的临时文件）改名到 target：同一文件系统上的改名是原子的；之后把目录落盘。
// replaced 报告改名是否已经发生：为真时即使返回了错误（目录落盘失败），目标路径上也已经是新二进制。
func Install(staged, target string) (replaced bool, err error) {
	if err := os.Chmod(staged, 0o755); err != nil {
		return false, err
	}
	if err := os.Rename(staged, target); err != nil {
		return false, err
	}
	return true, SyncDir(filepath.Dir(target))
}

// RestorePrevious 把 .bak 复制回目标路径（先写同目录的临时文件再改名），.bak 保留。
func RestorePrevious(target string) error {
	data, err := os.ReadFile(PreviousPath(target))
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Dir(target), target, data, 0o755)
}

// WriteFileAtomic 先在同一目录写临时文件并落盘，再改名到 path 并把目录落盘。
func WriteFileAtomic(dir, path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err == nil {
		err = SyncDir(dir)
	}
	return err
}

// SyncDir 对目录执行 fsync，保证目录项变更落盘。
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
