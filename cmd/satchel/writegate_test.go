package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/command"
)

// master-db-migration「拷贝期间阻塞写入」的拦截部分（写锁另在 service/database 里测）：「写入暂停」开着时，
// 写命令 unavailable 且不写审计；job get / job list 放行、审计只进日志；登录 unavailable；令牌不更新最后使用时间；关掉后照旧。
func TestWriteGate(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	h := start(t, bdb)
	base := h.tcpURL
	admin := setupAdmin(t, base)
	token := issueToken(t, admin, base, "readonly")
	h.app.writeGate.Suspend("正在把数据库迁移到 PostgreSQL，这期间主控只能查长任务", "")

	before := h.auditCount()
	if _, stderr, code := h.cli("settings", "set", "--set", "branding_site_title=x", "--resource-version", "1"); code == 0 || !strings.Contains(stderr, "unavailable") {
		t.Fatalf("迁移期间写命令应当 unavailable：%d %s", code, stderr)
	}
	if _, stderr, code := h.cli("job", "list"); code != 0 {
		t.Fatalf("job list 应当放行：%s", stderr)
	}
	if h.auditCount() != before || !strings.Contains(h.logs.String(), "审计只记日志") {
		t.Fatalf("迁移期间不应当写审计、job list 应当进日志：%d → %d", before, h.auditCount())
	}
	if r := send(t, nil, "POST", base+"/api/v1/session", nil, `{"username":"admin","password":"secret12"}`); r.status != 503 || r.code() != "unavailable" {
		t.Fatalf("迁移期间登录应当 unavailable：%d %s", r.status, r.body)
	}
	if r := send(t, nil, "GET", base+"/api/v1/job", map[string]string{"Authorization": "Bearer " + token}, ""); r.status != 200 {
		t.Fatalf("令牌查 job 应当放行：%d %s", r.status, r.body)
	}
	var tk model.ApiToken
	if err := h.db.NewSelect().Model(&tk).Limit(1).Scan(context.Background()); err != nil || tk.LastUsedAt != nil {
		t.Fatalf("迁移期间不应当更新令牌的最后使用时间：%+v %v", tk.LastUsedAt, err)
	}
	h.app.writeGate.Resume()
	if _, stderr, code := h.cli("settings", "set", "--set", "branding_site_title=x", "--resource-version", "1"); code != 0 {
		t.Fatalf("关掉后应当照旧：%s", stderr)
	}
}

// 审查：非 read 的命令进门登记、并在 ctx 上标出已进门，Drain 等它们走完；read 命令（update check 这类可能慢）不登记；
// 初始化向导建管理员（执行链之后 REST 还写会话）也登记。
func TestGateRegistersInflight(t *testing.T) {
	gate := &db.WriteGate{}
	release := make(chan struct{})
	entered := make(chan bool, 1)
	blocking := command.RunnerFunc(func(ctx context.Context, _ *command.Invocation) (any, error) {
		entered <- db.Entered(ctx)
		<-release
		return nil, nil
	})
	runner := gatedRunner(gate, command.Catalog(), blocking)
	drainsWithin := func(d time.Duration) bool {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return gate.Drain(ctx) == nil
	}

	done := make(chan struct{})
	go func() {
		runner.Run(context.Background(), &command.Invocation{Path: []string{"settings", "set"}})
		close(done)
	}()
	if !<-entered {
		t.Fatal("写命令在 ctx 上应当标出已进门")
	}
	gate.Suspend("正在升级", "")
	if drainsWithin(30 * time.Millisecond) {
		t.Fatal("写命令还没走完，Drain 应当等")
	}
	close(release)
	<-done
	if !drainsWithin(time.Second) {
		t.Fatal("写命令走完后 Drain 应当返回")
	}
	gate.Resume()

	release = make(chan struct{})
	done = make(chan struct{})
	go func() {
		runner.Run(context.Background(), &command.Invocation{Path: []string{"update", "check"}})
		close(done)
	}()
	if <-entered {
		t.Fatal("read 命令不登记进门")
	}
	gate.Suspend("正在升级", "")
	if !drainsWithin(time.Second) {
		t.Fatal("read 命令不该拖住 Drain")
	}
	close(release)
	<-done
	gate.Resume()

	// 慢慢发请求体的登录请求：请求体读完之前不登记，Drain 不等它；读完时开关已开，就被拒。
	bodyGate := make(chan struct{})
	pr, pw := io.Pipe()
	go func() { <-bodyGate; pw.Write([]byte(`{"username":"a"}`)); pw.Close() }()
	rec := httptest.NewRecorder()
	slowDone := make(chan struct{})
	slow := gatedSessions(gate, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	go func() {
		slow.ServeHTTP(rec, httptest.NewRequest("POST", command.APIPrefix+"session", pr))
		close(slowDone)
	}()
	time.Sleep(20 * time.Millisecond)
	gate.Suspend("正在升级", "")
	if !drainsWithin(time.Second) {
		t.Fatal("请求体还没读完的请求不该拖住 Drain")
	}
	close(bodyGate)
	<-slowDone
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("读完请求体时开关已开，应当 503：%d", rec.Code)
	}
	gate.Resume()

	release = make(chan struct{})
	done = make(chan struct{})
	h := gatedSessions(gate, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", command.APIPrefix+"setup/init", nil))
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	gate.Suspend("正在升级", "")
	if drainsWithin(30 * time.Millisecond) {
		t.Fatal("初始化向导的请求还没走完，Drain 应当等")
	}
	close(release)
	<-done
	if !drainsWithin(time.Second) {
		t.Fatal("走完后 Drain 应当返回")
	}
}
