//go:build linux

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	archive "github.com/satchel/satchel/internal/base/backup"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/selfupdate"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 自升级的端到端（master-self-update「升级成功」）：真的 exec，只在 Linux 上跑（本机 macOS 跳过，靠 CI）。
// 构建 0.0.1 与 0.0.2 两个二进制，都用 -ldflags -X 换上测试公钥与 httptest 的发布地址；起 0.0.1 的 serve，
// 经 CLI update check、update apply 跟到结束，然后看版本、PID、.bak、升级前备份与升级标记。
func TestUpgradeEndToEnd(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"/api/repos/o/satchel/releases/latest": []byte(`{"tag_name":"v0.0.2","html_url":"https://example/r"}`)}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	defer gh.Close()

	build := func(version, out string) {
		t.Helper()
		ldflags := strings.Join([]string{
			"-X github.com/satchel/satchel/internal/base/buildinfo.Version=" + version,
			"-X github.com/satchel/satchel/pkg/release.publicKeysCSV=" + base64.StdEncoding.EncodeToString(pub),
			"-X github.com/satchel/satchel/internal/base/selfupdate.GitHubRepo=o/satchel",
			"-X github.com/satchel/satchel/internal/base/selfupdate.GitHubAPI=" + gh.URL + "/api",
			"-X github.com/satchel/satchel/internal/base/selfupdate.GitHubWeb=" + gh.URL + "/web",
			"-X github.com/satchel/satchel/internal/base/selfupdate.GHProxy=",
		}, " ")
		cmd := exec.Command("go", "build", "-o", out, "-ldflags", ldflags, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("构建 %s 失败：%v\n%s", version, err, b)
		}
	}
	binDir := t.TempDir()
	target := filepath.Join(binDir, "satchel")
	build("0.0.1", target)
	next := filepath.Join(t.TempDir(), "satchel-next")
	build("0.0.2", next)
	nextBytes, err := os.ReadFile(next)
	if err != nil {
		t.Fatal(err)
	}
	asset := "/web/o/satchel/releases/download/v0.0.2/" + selfupdate.BinaryName(runtime.GOARCH)
	files[asset], files[asset+".sig"] = nextBytes, ed25519.Sign(priv, nextBytes)

	dataDir := shortTempDir(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	ln.Close()
	serve := exec.Command(target, "serve", "--data-dir", dataDir)
	serve.Env = append(os.Environ(), db.EnvListen+"="+listen)
	logs := &syncBuffer{}
	serve.Stdout, serve.Stderr = logs, logs
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	pid := serve.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- serve.Wait() }()
	defer func() {
		serve.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			serve.Process.Kill()
		}
	}()
	waitSocket(t, filepath.Join(dataDir, db.SocketFile), logs)
	lockFD := serveLockFD(t, pid, dataDir)

	cli := func(args ...string) (string, error) {
		cmd := exec.Command(target, append(args, "--data-dir", dataDir)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := cli("update", "check", "--json"); err != nil || !strings.Contains(out, `"latest_version": "0.0.2"`) && !strings.Contains(out, `"latest_version":"0.0.2"`) {
		t.Fatalf("update check：%v %s\n%s", err, out, logs.String())
	}
	out, err := cli("update", "apply", "0.0.2", "--confirm", "0.0.2", "--json")
	if err != nil || !strings.Contains(out, `"done"`) {
		t.Fatalf("update apply 应当跟到 done：%v %s\n主控日志：%s", err, out, logs.String())
	}

	select {
	case err := <-exited:
		t.Fatalf("主控进程不应当退出（exec 原地换进程）：%v\n%s", err, logs.String())
	default:
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("PID %d 应当还在：%v", pid, err)
	}
	if exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); exe != target {
		t.Fatalf("PID %d 跑的应当是目标路径上的新二进制：%s", pid, exe)
	}
	// 锁交给了新进程：还是同一个 fd，新进程已把它设回 close-on-exec，别人拿不到。
	if got := serveLockFD(t, pid, dataDir); got != lockFD {
		t.Fatalf("serve.lock 应当还是 fd %s，得到 %s", lockFD, got)
	}
	if info, _ := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/%s", pid, lockFD)); !cloexec(string(info)) {
		t.Fatalf("新进程应当把锁设回 close-on-exec：%s", info)
	}
	if _, err := acquireServeLock(dataDir); v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("升级之后锁仍应当被主控拿着：%v", err)
	}
	if out, err := cli("version", "--json"); err != nil || !strings.Contains(out, "0.0.2") {
		t.Fatalf("目标路径上应当是 0.0.2：%v %s", err, out)
	}
	if out, _ := cli("update", "check", "--json"); !strings.Contains(out, `"current_version": "0.0.2"`) && !strings.Contains(out, `"current_version":"0.0.2"`) {
		t.Fatalf("跑着的主控应当是 0.0.2：%s", out)
	}
	if _, err := os.Stat(selfupdate.PreviousPath(target)); err != nil {
		t.Fatal("应当留着上一版二进制")
	}
	list, _ := archive.List(dataDir)
	if len(list) != 1 || !strings.HasPrefix(list[0].Name, archive.PrefixBeforeUpgrade) {
		t.Fatalf("应当有一份升级前备份：%v", list)
	}
	if m, _ := selfupdate.ReadMarker(dataDir); m != nil {
		t.Fatalf("升级标记应当删掉：%+v", m)
	}
}

func waitSocket(t *testing.T, sock string, logs *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", sock); err == nil {
			conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("主控没有起来：%s", logs.String())
}

// serveLockFD 在 /proc/<pid>/fd 里找指向数据目录 serve.lock 的那个文件描述符。
func serveLockFD(t *testing.T, pid int, dataDir string) string {
	t.Helper()
	want := filepath.Join(dataDir, db.ServeLockFile)
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if target, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, e.Name())); target == want {
			return e.Name()
		}
	}
	t.Fatalf("PID %d 没有打开 %s", pid, want)
	return ""
}

// cloexec 报告 fdinfo 里的 flags 是否带 O_CLOEXEC（八进制 02000000）。
func cloexec(fdinfo string) bool {
	for _, line := range strings.Split(fdinfo, "\n") {
		if v, ok := strings.CutPrefix(line, "flags:"); ok {
			flags, err := strconv.ParseUint(strings.TrimSpace(v), 8, 64)
			return err == nil && flags&0o2000000 != 0
		}
	}
	return false
}
