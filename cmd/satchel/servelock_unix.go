//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/satchel/satchel/internal/base/db"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// serveLockEnv 是 exec 交接时把锁告诉新进程的环境变量（只在 serve exec 自己时设，新进程读完就删掉），
// 值是「文件描述符:设备号:inode」：新进程据此确认这个 fd 是自己交过来的那一个。
const serveLockEnv = "SATCHEL_SERVE_LOCK_FD"

// acquireServeLock 拿数据目录的独占锁（flock 数据目录里的 serve.lock），serve 整个运行期间拿着：同一个数据目录上只能有一个
// 主控在跑，第二个在碰任何升级或恢复的状态之前就 conflict。自升级 exec 出来的新进程经 serveLockEnv 接着用旧进程的那个
// 文件描述符（锁跟着它，交接期间不松开）。
//
// 交过来的文件描述符先用 fstat 核对：设备号与 inode 与环境变量里记的相同，才是自己交过来的那一个（伪造、残留、早已关闭的都不碰：
// *os.File 被回收时会关掉它的 fd，绝不能包一个属于别人的 fd）。是自己的、而且仍是路径上的 serve.lock，才包成 *os.File 接着用，
// 并再 flock 一次（同一个打开的文件已经拿着锁时是空操作）；是自己的、但路径上的 serve.lock 已经被删掉或换掉，就关掉它（不留
// 一个拿着旧 inode 的 fd 被子进程继承），照常打开并去锁。
func acquireServeLock(dataDir string) (*os.File, error) {
	path := filepath.Join(dataDir, db.ServeLockFile)
	if raw := os.Getenv(serveLockEnv); raw != "" {
		os.Unsetenv(serveLockEnv)
		if fd, dev, ino, ok := parseLockEnv(raw); ok && fd > 2 && !hasCloexec(fd) && fdIs(fd, dev, ino) {
			if pathIs(path, dev, ino) {
				syscall.CloseOnExec(fd)
				return lock(os.NewFile(uintptr(fd), path), dataDir, path)
			}
			syscall.Close(fd)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "打开 "+path+" 失败", err)
	}
	return lock(f, dataDir, path)
}

// lock 对 f 加独占的 flock（不等待）；拿不到时关掉 f。
func lock(f *os.File, dataDir, path string) (*os.File, error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, v1.Newf(v1.CodeConflict, "数据目录 %s 已有一个主控在运行（%s 被锁着）", dataDir, path).
				WithNext("先停掉那个主控，或给这个实例另指定数据目录")
		}
		return nil, v1.Wrap(v1.CodeInternal, "锁住 "+path+" 失败", err)
	}
	return f, nil
}

// parseLockEnv 解析「文件描述符:设备号:inode」。
func parseLockEnv(raw string) (fd int, dev, ino uint64, ok bool) {
	parts := strings.Split(raw, ":")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	fd, err1 := strconv.Atoi(parts[0])
	dev, err2 := strconv.ParseUint(parts[1], 10, 64)
	ino, err3 := strconv.ParseUint(parts[2], 10, 64)
	return fd, dev, ino, err1 == nil && err2 == nil && err3 == nil
}

// hasCloexec 报告 fd 是否带 close-on-exec（打开失败也算带，即不碰）。交过来的锁一定被 execEnv 清掉了这个标志，而本进程里
// Go 打开的 fd（日志文件、netpoll）都带着：故意按别的 fd 的设备号与 inode 构造的环境变量，也碰不到它们。
func hasCloexec(fd int) bool {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	return errno != 0 || flags&syscall.FD_CLOEXEC != 0
}

// fdIs 报告文件描述符 fd 是否打开着、并且是设备号 dev、inode ino 的那个文件。只用 fstat，不接管这个 fd。
func fdIs(fd int, dev, ino uint64) bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return false
	}
	return uint64(st.Dev) == dev && uint64(st.Ino) == ino
}

// pathIs 报告 path 是否是设备号 dev、inode ino 的那个文件。
func pathIs(path string, dev, ino uint64) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Dev) == dev && uint64(st.Ino) == ino
}

// execEnv 是 exec 新二进制时用的环境变量：清掉锁的 close-on-exec，把它的文件描述符写进 serveLockEnv，锁就跟着进程过去。
func execEnv(lock *os.File) []string {
	env := os.Environ()
	if lock == nil {
		return env
	}
	var st syscall.Stat_t
	fd := lock.Fd()
	if err := syscall.Fstat(int(fd), &st); err != nil {
		return env // 交不过去：新进程自己再去锁（旧进程已经没了，锁随之释放）
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFD, 0); errno != 0 {
		return env
	}
	return append(env, fmt.Sprintf("%s=%d:%d:%d", serveLockEnv, fd, uint64(st.Dev), uint64(st.Ino)))
}
