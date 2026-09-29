package main

import (
	"context"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
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
	h.app.writeGate.Suspend()

	before := h.auditCount()
	if _, stderr, code := h.cli("settings", "set", "--set", "branding_site_title=x", "--resource-version", "1"); code == 0 || !strings.Contains(stderr, "unavailable") {
		t.Fatalf("迁移期间写命令应当 unavailable：%d %s", code, stderr)
	}
	if _, stderr, code := h.cli("job", "list"); code != 0 {
		t.Fatalf("job list 应当放行：%s", stderr)
	}
	if h.auditCount() != before || !strings.Contains(h.logs.String(), "数据库迁移期间审计只记日志") {
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
