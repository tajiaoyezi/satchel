//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/satchel/satchel/internal/base/db"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// lockEnvFor 是把 fd 当作 path 上那个文件交出去时的环境变量值（与 execEnv 同一格式）。
func lockEnvFor(t *testing.T, fd int, path string) string {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%d:%d", fd, uint64(st.Dev), uint64(st.Ino))
}

func cloexecSet(t *testing.T, fd uintptr) bool {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatalf("F_GETFD：%v", errno)
	}
	return flags&syscall.FD_CLOEXEC != 0
}

func fdOpen(fd int) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	return errno == 0
}

// 审查：同一个数据目录上第二个 serve 拿不到锁（conflict）；exec 交接时锁经环境变量交给新进程，新进程接着用、不再去抢，
// 并把它设回 close-on-exec（不让子进程继承）。
func TestServeLock(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, db.ServeLockFile)
	first, err := acquireServeLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireServeLock(dir); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("第二个应当 conflict：%v", err)
	}
	var handed string
	for _, kv := range execEnv(first) {
		if v, ok := strings.CutPrefix(kv, serveLockEnv+"="); ok {
			handed = v
		}
	}
	if want := lockEnvFor(t, int(first.Fd()), path); handed != want {
		t.Fatalf("exec 的环境变量应当是 %q，得到 %q", want, handed)
	}
	// 模拟 exec 出来的新进程：exec 继承的文件描述符与原来的共享同一个打开的文件（锁跟着它）。测试进程里不能让两个 *os.File
	// 管同一个 fd（会被关两次），所以 dup 一个出来交过去，效果相同。dup 出来的 fd 不带 close-on-exec，正好检验接管时设回。
	dup, err := syscall.Dup(int(first.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(serveLockEnv, lockEnvFor(t, dup, path))
	again, err := acquireServeLock(dir)
	if err != nil || again.Fd() != uintptr(dup) {
		t.Fatalf("新进程应当接着用交过来的那个：%v", err)
	}
	if !cloexecSet(t, again.Fd()) {
		t.Fatal("接管之后应当设回 close-on-exec")
	}
	if os.Getenv(serveLockEnv) != "" {
		t.Fatal("读完就应当删掉环境变量")
	}
	first.Close()
	if _, err := acquireServeLock(dir); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("交过去的那个还拿着锁：%v", err)
	}
	again.Close()
	third, err := acquireServeLock(dir)
	if err != nil {
		t.Fatalf("锁放开之后应当拿得到：%v", err)
	}
	third.Close()
}

// 审查：环境变量是伪造或残留的——指向一个无关的已打开文件时，不接管也不关它（垃圾回收之后它照样能用）；
// 指向一个没拿锁的 serve.lock、而锁在别人手上时，照常 conflict，不能绕过互斥。
func TestServeLockForgedEnv(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, db.ServeLockFile)
	f, err := acquireServeLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	other, err := os.CreateTemp(dir, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// fd 是无关的文件，设备号与 inode 却写成 serve.lock 的：fstat 对不上，不碰它。
	t.Setenv(serveLockEnv, fmt.Sprintf("%d:%s", other.Fd(), strings.SplitN(lockEnvFor(t, 0, path), ":", 2)[1]))
	if _, err := acquireServeLock(dir); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("锁在 f 手上，应当 conflict：%v", err)
	}
	// fd 是本进程自己打开的文件（带 close-on-exec），设备号与 inode 也写成它自己的：照样不碰（交过来的锁一定不带 close-on-exec）。
	t.Setenv(serveLockEnv, lockEnvFor(t, int(other.Fd()), other.Name()))
	if _, err := acquireServeLock(dir); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("锁在 f 手上，应当 conflict：%v", err)
	}
	runtime.GC()
	runtime.GC()
	if _, err := other.WriteString("still mine"); err != nil {
		t.Fatalf("无关的文件不应当被关掉：%v", err)
	}
	// 另开一个没拿锁的 serve.lock 的裸 fd（不交给 *os.File），伪造成交过来的。
	raw, err := syscall.Open(path, syscall.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(serveLockEnv, lockEnvFor(t, raw, path))
	if _, err := acquireServeLock(dir); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("伪造的交接不能绕过互斥：%v", err)
	}
}

// 审查：交过来的确实是自己的锁、但路径上的 serve.lock 已经被删掉重建（inode 变了）：关掉交过来的那个（不留一个拿着旧 inode
// 的 fd 被子进程继承），在新文件上重新加锁。
func TestServeLockReplacedFile(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, db.ServeLockFile)
	first, err := acquireServeLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	dup, err := syscall.Dup(int(first.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	handed := lockEnvFor(t, dup, path)
	first.Close() // 只剩 dup 拿着旧 inode 上的锁
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv(serveLockEnv, handed)
	f, err := acquireServeLock(dir)
	if err != nil {
		t.Fatalf("应当在新文件上加锁：%v", err)
	}
	defer f.Close()
	if fdOpen(dup) {
		t.Fatal("交过来的旧 fd 应当被关掉")
	}
}
