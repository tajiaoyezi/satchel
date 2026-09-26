package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/core/taskruns"
)

// cliPage 经 CLI（本机 socket）跑一条列表命令，解出 items 与 total。
func (h *harness) cliPage(args ...string) ([]map[string]any, int) {
	h.t.Helper()
	stdout, stderr, code := h.cli(append(args, "--json")...)
	if code != 0 {
		h.t.Fatalf("%v：%s", args, stderr)
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal([]byte(stdout), &page); err != nil {
		h.t.Fatalf("%v 的输出不是一页：%s", args, stdout)
	}
	return page.Items, page.Total
}

// 日志与内置任务的端到端（双库）：master-logs 与 master-scheduler 经 CLI、REST 的样子。
func TestOpsEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		// master-scheduler「上次中断的记录」：库里预置一行 running，启动后变成 error。
		if _, err := taskruns.New(bdb).Start(context.Background(), "audit_cleanup", time.Now()); err != nil {
			t.Fatal(err)
		}
		h := start(t, bdb)
		base := h.tcpURL

		runs, total := h.cliPage("schedule", "runs", "list", "--status", "error")
		if total != 1 || runs[0]["task"] != "audit_cleanup" || runs[0]["detail"] != "主控停止时它还在运行" {
			t.Fatalf("中断的记录应当被收尾：%v", runs)
		}
		if _, n := h.cliPage("schedule", "runs", "list", "--status", "running"); n != 0 {
			t.Fatalf("不应当还有 running 的行：%d", n)
		}

		// master-scheduler「列出任务」「PostgreSQL 下没有检查点任务」。
		tasks, _ := h.cliPage("schedule", "list")
		var names []string
		for _, task := range tasks {
			names = append(names, task["name"].(string))
		}
		want := []string{"audit_cleanup", "ban_sweep", "db_health", "login_limit_sweep", "security_event_cleanup", "session_cleanup", "task_run_cleanup"}
		if db.DialectOf(bdb) == schema.SQLite {
			want = []string{"audit_cleanup", "ban_sweep", "db_checkpoint", "db_health", "login_limit_sweep", "security_event_cleanup", "session_cleanup", "task_run_cleanup"}
		}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("任务清单应当是 %v，得到 %v", want, names)
		}

		// master-logs：经 socket 读到启动日志（serve 的 goroutine 写这一行，稍等一下）。
		var lines []map[string]any
		for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if lines, _ = h.cliPage("logs", "list", "--grep", "主控已启动"); len(lines) > 0 || time.Now().After(deadline) {
				break
			}
		}
		if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["msg"] != "主控已启动" {
			t.Fatalf("经 socket 应当读到启动日志：%v", lines)
		}
		// --grep 可能是在找令牌或密码：审计摘要里打码（审查第 3 条）。
		if rec := h.lastAudit(); rec["command"] != "logs list" || strings.Contains(fmt.Sprint(rec["args_digest"]), "主控已启动") || !strings.Contains(fmt.Sprint(rec["args_digest"]), `"grep":"***"`) {
			t.Fatalf("--grep 的原文不应当进审计：%v", rec)
		}
		files, _ := h.cliPage("logs", "files", "list")
		if len(files) != 1 || files[0]["name"] != db.LogFile || files[0]["active"] != true {
			t.Fatalf("日志文件清单不对：%v", files)
		}

		// 经 REST：管理员会话能读；普通用户 forbidden。
		admin := setupAdmin(t, base)
		if status, fields, _ := admin.call("GET", base+"/api/v1/logs?level=info&grep="+"主控已启动", "", nil); status != 200 || !strings.Contains(string(fields["items"]), "主控已启动") {
			t.Fatalf("管理员经 REST 读日志：%d %v", status, fields)
		}
		seedUser(t, bdb, "carol", "carolpass")
		carol := newBrowser(t)
		if status, fields := carol.login(base, "carol", "carolpass", ""); status != 200 {
			t.Fatalf("普通用户登录：%d %v", status, fields)
		}
		for _, path := range []string{"/api/v1/logs", "/api/v1/logs/files", "/api/v1/schedule", "/api/v1/schedule/runs"} {
			if status, fields, _ := carol.call("GET", base+path, "", nil); status != 403 || str(fields["code"]) != "forbidden" {
				t.Fatalf("普通用户 GET %s 应当 403：%d %v", path, status, fields)
			}
		}
	})
}
