package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/projection/cli"
	"github.com/satchel/satchel/internal/projection/mcp"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 端到端：真的装配、真的监听（随机 TCP 端口 + 数据目录里的 socket）、真的库（双库）。
// 服务本体不查平台，这些用例在 macOS 上也跑；只有 serve 命令真启动的用例要 Linux。

type harness struct {
	t       *testing.T
	dataDir string
	db      *bun.DB
	app     *app
	tcpURL  string
	cancel  context.CancelFunc
	done    chan error
	logs    *syncBuffer
}

// syncBuffer 是主控的日志缓冲：主控在别的 goroutine 里写，测试同时读，所以加一把锁。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// shortTempDir：macOS 上 socket 路径不能超过 104 字节，t.TempDir() 太长。
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func start(t *testing.T, bdb *bun.DB) *harness {
	t.Helper()
	return startWith(t, bdb, db.ServeConfig{})
}

// startWith 同 start，但带着 serve 的配置装配（自救开关、允许跨域的来源）。
func startWith(t *testing.T, bdb *bun.DB, cfg db.ServeConfig) *harness {
	t.Helper()
	dataDir := shortTempDir(t)
	if err := db.EnsureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	// 日志同时写进数据目录的日志文件（像 serve 那样），logs list 才读得到。
	logFile, err := os.OpenFile(filepath.Join(dataDir, db.LogsDir, db.LogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(logs, logFile), nil))
	a, err := newApp(dataDir, bdb, logger, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tcp, unix, err := a.listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, dataDir: dataDir, db: bdb, app: a, tcpURL: listenAddrOf(tcp), cancel: cancel, done: make(chan error, 1), logs: logs}
	go func() { h.done <- a.serve(ctx, tcp, unix) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(15 * time.Second):
			t.Error("serve 没有在限时内停止")
		}
	})
	return h
}

func (h *harness) socket() string { return filepath.Join(h.dataDir, db.SocketFile) }

// unixClient 经 socket 连主控（对端就是本测试进程，uid 相同 → 本机管理员）。
func (h *harness) unixClient() *http.Client {
	sock := h.socket()
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
}

func get(t *testing.T, c *http.Client, url string) (int, map[string]json.RawMessage) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("%s 的 body 不是 JSON 对象：%s", url, body)
	}
	return resp.StatusCode, fields
}

func (h *harness) cli(args ...string) (stdout, stderr string, code int) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	opts := options()
	code = cli.Execute(opts, append(args, "--data-dir", h.dataDir), &out, &errOut)
	return out.String(), errOut.String(), code
}

func (h *harness) auditCount() int {
	h.t.Helper()
	n, err := h.db.NewSelect().TableExpr("audit_logs").Count(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) mcpRun(args ...string) (string, bool) {
	h.t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: "http://satchel" + mcp.Path, HTTPClient: h.unixClient()}, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "satchel_run", Arguments: map[string]any{"args": args}})
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Content[0].(*sdk.TextContent).Text, res.IsError
}

// 三个投影同一份输出、身份判定、审计条数、无身份入口。
func TestEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)

		// CLI 经 socket：本机管理员。
		stdout, stderr, code := h.cli("whoami", "--json")
		if code != 0 {
			t.Fatalf("satchel whoami 退出码 %d：%s", code, stderr)
		}
		var viaCLI map[string]json.RawMessage
		_ = json.Unmarshal([]byte(stdout), &viaCLI)
		if string(viaCLI["actor_kind"]) != `"local_admin"` || string(viaCLI["role"]) != `"admin"` {
			t.Fatalf("经 socket 的 CLI 应当是 local_admin：%s", stdout)
		}
		// REST 经 socket 与 TCP。
		status, viaSocket := get(t, h.unixClient(), "http://satchel/api/v1/whoami")
		if status != 200 {
			t.Fatalf("REST 经 socket 应当 200：%d %v", status, viaSocket)
		}
		status, viaTCP := get(t, http.DefaultClient, h.tcpURL+"/api/v1/whoami")
		if status != 401 || string(viaTCP["code"]) != `"unauthenticated"` || len(viaTCP) != 4 {
			t.Fatalf("REST 经 TCP 没有身份应当 401 四字段：%d %v", status, viaTCP)
		}
		// MCP 经 socket。
		text, isErr := h.mcpRun("whoami")
		if isErr {
			t.Fatalf("MCP 经 socket 应当成功：%s", text)
		}
		var viaMCP map[string]json.RawMessage
		_ = json.Unmarshal([]byte(text), &viaMCP)
		for k, v := range viaCLI {
			if string(viaSocket[k]) != string(v) || string(viaMCP[k]) != string(v) {
				t.Fatalf("三份输出在键 %s 上不同：CLI %s REST %s MCP %s", k, v, viaSocket[k], viaMCP[k])
			}
		}
		if len(viaSocket) != len(viaCLI) || len(viaMCP) != len(viaCLI) {
			t.Fatalf("三份输出的键数不同：%d %d %d", len(viaCLI), len(viaSocket), len(viaMCP))
		}
		// 审计：CLI、REST 经 socket、MCP 各一条；TCP 那次 anonymous 不记。
		if n := h.auditCount(); n != 3 {
			t.Fatalf("审计表应当恰好 3 条，得到 %d", n)
		}
		// explain 不记；audit list 记且能看到前面的记录。
		if _, stderr, code := h.cli("explain", "Task", "--json"); code != 0 {
			t.Fatal(stderr)
		}
		stdout, stderr, code = h.cli("audit", "list", "--json", "--limit", "2")
		if code != 0 {
			t.Fatalf("audit list 退出码 %d：%s", code, stderr)
		}
		var page struct {
			Items      []map[string]any `json:"items"`
			Total      int              `json:"total"`
			NextCursor string           `json:"nextCursor"`
		}
		if err := json.Unmarshal([]byte(stdout), &page); err != nil || page.Total != 3 || len(page.Items) != 2 || page.NextCursor == "" {
			t.Fatalf("audit list 应当报 3 条、给 2 条、有下一页：%s %v", stdout, err)
		}
		if page.Items[0]["command"] != "whoami" || page.Items[0]["actor_kind"] != "local_admin" || page.Items[0]["result"] != "ok" {
			t.Fatalf("最新一条应当是 whoami：%v", page.Items[0])
		}
		if n := h.auditCount(); n != 4 {
			t.Fatalf("explain 不记、audit list 记：应当 4 条，得到 %d", n)
		}
		// 无身份入口。
		if status, fields := get(t, http.DefaultClient, h.tcpURL+"/api/v1/healthz"); status != 200 || string(fields["status"]) != `"ok"` {
			t.Fatalf("healthz：%d %v", status, fields)
		}
		if err := os.WriteFile(filepath.Join(h.dataDir, db.PublicDir, "logo.png"), []byte("PNG"), 0o600); err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(h.tcpURL + "/public/logo.png")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "PNG" {
			t.Fatalf("/public/ 文件：%d %s", resp.StatusCode, body)
		}
		if resp, err := http.Get(h.tcpURL + "/public/"); err != nil || resp.StatusCode != 404 {
			t.Fatalf("/public/ 目录应当 404")
		}
		if status, fields := get(t, http.DefaultClient, h.tcpURL+"/nosuch"); status != 404 || string(fields["code"]) != `"not_found"` {
			t.Fatalf("未知路径应当 404 四字段：%d %v", status, fields)
		}
		if n := h.auditCount(); n != 4 {
			t.Fatalf("healthz、public、404 都不记：仍应当 4 条，得到 %d", n)
		}
		if info, err := os.Stat(h.socket()); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("socket 文件应当 0600：%v %v", info, err)
		}
	})
}

// 残留的 socket 文件被接管；SIGTERM（这里用 ctx 取消模拟）后进行中的请求完成、socket 消失。
func TestStaleSocketAndGracefulStop(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	dataDir := shortTempDir(t)
	if err := db.EnsureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dataDir, db.SocketFile)
	if err := os.WriteFile(stale, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(dataDir, bdb, slog.New(slog.NewTextHandler(io.Discard, nil)), db.ServeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tcp, unix, err := a.listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("残留文件应当被接管：%v", err)
	}
	if info, err := os.Stat(stale); err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("应当变成 0600 的 socket：%v %v", info, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.serve(ctx, tcp, unix) }()

	// 一个慢请求正在处理时停止：用 MCP 之外最简单的慢路径——占住一个连接读 healthz 前先不读完 body 不现实，
	// 这里用并发的 healthz 请求代替：取消后仍能拿到已发出请求的完整响应。
	url := listenAddrOf(tcp) + "/api/v1/healthz"
	var wg sync.WaitGroup
	results := make(chan int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(url)
			if err != nil {
				results <- -1
				return
			}
			resp.Body.Close()
			results <- resp.StatusCode
		}()
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()
	close(results)
	for code := range results {
		if code != 200 {
			t.Errorf("停止期间已发出的请求应当拿到 200，得到 %d", code)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve 应当干净退出：%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve 没有在限时内停止")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("停止后 socket 文件应当被删掉")
	}
	if _, err := http.Get(url); err == nil {
		t.Fatal("停止后 TCP 不该再接受连接")
	}
}

// serve 命令：非 Linux 拒绝；Linux 上真起来（CI）。
func TestServeCommand(t *testing.T) {
	dataDir := shortTempDir(t)
	if runtime.GOOS != "linux" {
		var out, errOut bytes.Buffer
		code := cli.Execute(options(), []string{"serve", "--json", "--data-dir", dataDir}, &out, &errOut)
		if code != v1.ExitFailure {
			t.Fatalf("非 Linux 应当退出码 1，得到 %d：%s", code, errOut.String())
		}
		var e v1.Error
		if err := json.Unmarshal(errOut.Bytes(), &e); err != nil || e.Code != v1.CodeUnsupportedPlatform || !strings.Contains(e.Reason, "客户端") {
			t.Fatalf("应当是 unsupported_platform 并说明只保证客户端：%s", errOut.String())
		}
		t.Skip("serve 命令真启动的用例只在 Linux 上跑（CI）")
	}
	// 配置校验不认端口 0：先找一个空闲端口再交给 serve。
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenAddr := probe.Addr().String()
	probe.Close()
	t.Setenv(db.EnvListen, listenAddr)
	t.Setenv(db.EnvConfigPath, "")
	t.Setenv(db.EnvLogLevel, "")
	for _, name := range []string{"SATCHEL_DATABASE_DRIVER", "SATCHEL_DATABASE_HOST", "SATCHEL_DATABASE_PORT", "SATCHEL_DATABASE_NAME", "SATCHEL_DATABASE_USER", "SATCHEL_DATABASE_PASSWORD"} {
		t.Setenv(name, "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var out, errOut bytes.Buffer
	go func() {
		done <- cli.ExecuteContext(ctx, options(), []string{"serve", "--json", "--data-dir", dataDir}, &out, &errOut)
	}()
	sock := filepath.Join(dataDir, db.SocketFile)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve 没有在限时内建出 socket：%s", errOut.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, sub := range db.DataSubDirs {
		if _, err := os.Stat(filepath.Join(dataDir, sub)); err != nil {
			t.Errorf("serve 应当建出子目录 %s：%v", sub, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, db.SQLiteFile)); err != nil {
		t.Errorf("serve 应当建库并迁移：%v", err)
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	if status, fields := get(t, c, "http://satchel/api/v1/healthz"); status != 200 || string(fields["status"]) != `"ok"` {
		t.Fatalf("healthz 经 socket：%d %v", status, fields)
	}
	if status, fields := get(t, c, "http://satchel/api/v1/whoami"); status != 200 || string(fields["actor_kind"]) != `"local_admin"` {
		t.Fatalf("whoami 经 socket：%d %v", status, fields)
	}
	if status, fields := get(t, http.DefaultClient, "http://"+listenAddr+"/api/v1/healthz"); status != 200 || string(fields["status"]) != `"ok"` {
		t.Fatalf("healthz 经配置的 TCP 地址：%d %v", status, fields)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve 停止后退出码应当 0，得到 %d：%s", code, errOut.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve 没有在限时内退出")
	}
	if !strings.Contains(out.String(), `"stopped":true`) {
		t.Fatalf("serve 停止后应当输出 stopped：%s", out.String())
	}
}
