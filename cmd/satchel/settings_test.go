package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// envelope 是 settings show / set / rollback / master-url set 输出的形状（只取用例要看的部分）。
type envelope struct {
	Kind     string `json:"kind"`
	Metadata struct {
		ID              int64  `json:"id"`
		ResourceVersion int64  `json:"resourceVersion"`
		Name            string `json:"name"`
	} `json:"metadata"`
	Spec   map[string]json.RawMessage `json:"spec"`
	Status map[string]json.RawMessage `json:"status"`
}

func parseEnvelope(t *testing.T, raw string) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal([]byte(raw), &e); err != nil || e.Kind != "SystemSettings" {
		t.Fatalf("不是 SystemSettings 的信封：%v\n%s", err, raw)
	}
	return e
}

func (h *harness) snapshotCount() int {
	h.t.Helper()
	n, err := h.db.NewSelect().TableExpr("config_snapshots").Count(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// settingsCLI 经 socket 跑一条 settings 命令，成功时返回信封；失败时返回四字段错误与退出码。
func (h *harness) settingsCLI(args ...string) (env envelope, e *v1.Error, code int) {
	h.t.Helper()
	stdout, stderr, code := h.cli(append(args, "--json")...)
	if code == 0 {
		return parseEnvelope(h.t, stdout), nil, 0
	}
	var err v1.Error
	if jerr := json.Unmarshal([]byte(stderr), &err); jerr != nil {
		h.t.Fatalf("stderr 不是四字段错误：%s", stderr)
	}
	return envelope{}, &err, code
}

// 系统设置的端到端（双库）：单例行 → 读 → 写与校验 → 版本与 force → 快照与回滚 → 会话带当场验证改主控地址 → 普通用户 → 审计。
func TestSettingsEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		base := h.tcpURL

		// 空库起来就有版本 1 与默认值（master-settings「单例行的建立」「settings show」）。
		env, e, _ := h.settingsCLI("settings", "show")
		if e != nil || env.Metadata.ID != 1 || env.Metadata.ResourceVersion != 1 || env.Metadata.Name != "" {
			t.Fatalf("settings show：%v %+v", e, env.Metadata)
		}
		if string(env.Spec["heartbeat_interval"]) != "30" || string(env.Spec["default_theme"]) != `"flat"` || string(env.Spec["enable_short_link"]) != "true" ||
			string(env.Status["require_encryption"]) != "true" || string(env.Status["master_url"]) != `""` {
			t.Fatalf("默认值：%s %s %s %s", env.Spec["heartbeat_interval"], env.Spec["default_theme"], env.Status["require_encryption"], env.Status["master_url"])
		}
		if _, ok := env.Spec["master_url"]; ok {
			t.Fatal("master_url 不在 spec 里")
		}
		// 文本形式分三段。
		if stdout, _, code := h.cli("settings", "show"); code != 0 || !strings.Contains(stdout, "[spec]") || !strings.Contains(stdout, "[status]") || !strings.Contains(stdout, "heartbeat_interval：30") {
			t.Fatalf("文本输出：%d\n%s", code, stdout)
		}

		// 列与 key 一起改 → 版本 2、快照一条。
		env, e, _ = h.settingsCLI("settings", "set", "--set", "heartbeat_interval=45", "--set", "branding_site_title=Satchel", "--resource-version", "1")
		if e != nil || env.Metadata.ResourceVersion != 2 || string(env.Spec["heartbeat_interval"]) != "45" || string(env.Spec["branding_site_title"]) != `"Satchel"` || h.snapshotCount() != 1 {
			t.Fatalf("settings set：%v %+v %d", e, env.Metadata, h.snapshotCount())
		}
		// 混档、未知、类型错、规则错都整单拒绝、不写、不留快照。
		for _, tc := range []struct {
			args []string
			code v1.Code
			exit int
		}{
			{[]string{"--set", "heartbeat_interval=50", "--set", "master_url=https://a.example"}, v1.CodeFieldNotApplyable, 1},
			{[]string{"--set", "heartbeat=50"}, v1.CodeUnknownField, 1},
			{[]string{"--set", "heartbeat_interval=abc"}, v1.CodeBadRequest, 1},
			{[]string{"--set", "heartbeat_interval=4"}, v1.CodeBadRequest, 1},
			{[]string{"--set", "default_theme=dark"}, v1.CodeBadRequest, 1},
		} {
			_, e, code := h.settingsCLI(append([]string{"settings", "set", "--resource-version", "2"}, tc.args...)...)
			if e == nil || e.Code != tc.code || code != tc.exit {
				t.Errorf("%v 应当 %s / 退出码 %d，得到 %v / %d", tc.args, tc.code, tc.exit, e, code)
			}
		}
		if env, _, _ := h.settingsCLI("settings", "show"); env.Metadata.ResourceVersion != 2 || string(env.Spec["heartbeat_interval"]) != "45" || h.snapshotCount() != 1 {
			t.Fatalf("被拒的写不该改任何东西：%+v %d", env.Metadata, h.snapshotCount())
		}
		// 过期版本 → version_conflict（退出码 6）；--force 跳过比对。
		if _, e, code := h.settingsCLI("settings", "set", "--set", "heartbeat_interval=50", "--resource-version", "1"); e == nil || e.Code != v1.CodeVersionConflict || code != v1.ExitVersionConflict {
			t.Fatalf("过期版本：%v %d", e, code)
		}
		env, e, _ = h.settingsCLI("settings", "set", "--set", "heartbeat_interval=50", "--resource-version", "1", "--force")
		if e != nil || env.Metadata.ResourceVersion != 3 || string(env.Spec["heartbeat_interval"]) != "50" || h.snapshotCount() != 2 {
			t.Fatalf("force：%v %+v %d", e, env.Metadata, h.snapshotCount())
		}

		// 快照列表：两条、倒序、不带内容；文本形式是表。
		stdout, stderr, code := h.cli("settings", "snapshots", "list", "--json")
		if code != 0 {
			t.Fatalf("snapshots list：%s", stderr)
		}
		var page struct {
			Items []map[string]json.RawMessage `json:"items"`
			Total int                          `json:"total"`
		}
		if err := json.Unmarshal([]byte(stdout), &page); err != nil || page.Total != 2 || len(page.Items) != 2 || string(page.Items[0]["object_version"]) != "2" || string(page.Items[1]["object_version"]) != "1" {
			t.Fatalf("快照列表：%v %s", err, stdout)
		}
		if _, ok := page.Items[0]["content"]; ok || strings.Contains(stdout, `"content"`) {
			t.Fatal("列表不该带 content")
		}
		if stdout, _, _ := h.cli("settings", "snapshots", "list"); !strings.Contains(stdout, "OBJECT_VERSION") || !strings.Contains(stdout, "共 2 条") {
			t.Fatalf("文本表：\n%s", stdout)
		}
		firstID := string(page.Items[1]["id"])
		// 回滚到版本 1 的内容：heartbeat_interval 回到 30、版本 4、快照三条。
		env, e, _ = h.settingsCLI("settings", "rollback", firstID, "--resource-version", "3")
		if e != nil || env.Metadata.ResourceVersion != 4 || string(env.Spec["heartbeat_interval"]) != "30" || string(env.Spec["branding_site_title"]) != `""` || h.snapshotCount() != 3 {
			t.Fatalf("rollback：%v %+v %d", e, env.Metadata, h.snapshotCount())
		}
		if _, e, code := h.settingsCLI("settings", "rollback", "424242", "--resource-version", "4"); e == nil || e.Code != v1.CodeNotFound || code != v1.ExitNotFound {
			t.Fatalf("不存在的快照：%v %d", e, code)
		}

		// 建管理员账号并登录；带当场验证改主控地址：版本 5、归一、快照数不变。
		alice := newBrowser(t)
		if status, fields, _ := alice.call("POST", base+"/api/v1/setup/init", `{"username":"admin","password":"secret12"}`, nil); status != 200 {
			t.Fatalf("setup init：%d %v", status, fields)
		}
		status, fields, _ := alice.call("POST", base+"/api/v1/settings/master-url/set", `{"url":"https://panel.example.com/","resource-version":4,"verify-password":"secret12"}`, nil)
		if status != 200 {
			t.Fatalf("master-url set：%d %v", status, fields)
		}
		var viaREST envelope
		raw, _ := json.Marshal(fields)
		if err := json.Unmarshal(raw, &viaREST); err != nil || viaREST.Metadata.ResourceVersion != 5 || string(viaREST.Status["master_url"]) != `"https://panel.example.com"` || h.snapshotCount() != 3 {
			t.Fatalf("改主控地址：%v %+v %s %d", err, viaREST.Metadata, viaREST.Status["master_url"], h.snapshotCount())
		}
		// 密码错、没验证、MCP 都是 human_required；形状不对是 bad_request。
		if status, fields, _ := alice.call("POST", base+"/api/v1/settings/master-url/set", `{"url":"https://b.example","resource-version":5,"verify-password":"wrong"}`, nil); status != 403 || str(fields["code"]) != "human_required" {
			t.Fatalf("密码错：%d %v", status, fields)
		}
		if status, fields, _ := alice.call("POST", base+"/api/v1/settings/master-url/set", `{"url":"https://b.example","resource-version":5}`, nil); status != 403 || str(fields["code"]) != "human_required" {
			t.Fatalf("没验证：%d %v", status, fields)
		}
		if text, isErr := h.mcpRun("settings", "master-url", "set", "--url", "https://b.example", "--resource-version", "5"); !isErr || !strings.Contains(text, "human_required") {
			t.Fatalf("MCP：%v %s", isErr, text)
		}
		if status, fields, _ := alice.call("POST", base+"/api/v1/settings/master-url/set", `{"url":"https://panel.example.com/admin","resource-version":5,"verify-password":"secret12"}`, nil); status != 400 || str(fields["code"]) != "bad_request" {
			t.Fatalf("形状不对：%d %v", status, fields)
		}
		if env, _, _ := h.settingsCLI("settings", "show"); env.Metadata.ResourceVersion != 5 || string(env.Status["master_url"]) != `"https://panel.example.com"` {
			t.Fatalf("被拒的都没写：%+v", env.Metadata)
		}
		// 会话经 REST 用原生类型改设置；MCP 读得到；三处同一个对象。
		status, fields, _ = alice.call("POST", base+"/api/v1/settings/set", `{"set":{"heartbeat_interval":45,"agent_log_enabled":true,"probe_external_token_sha256":"abc123"},"resource-version":5}`, nil)
		if status != 200 {
			t.Fatalf("REST settings set：%d %v", status, fields)
		}
		text, isErr := h.mcpRun("settings", "show")
		if isErr || !strings.Contains(text, `"kind":"SystemSettings"`) || !strings.Contains(text, `"heartbeat_interval":45`) || !strings.Contains(text, `"probe_external_token_sha256":"***"`) || strings.Contains(text, "abc123") {
			t.Fatalf("MCP settings show：%v %s", isErr, text)
		}
		if text, isErr := h.mcpRun("settings", "set", "--set", "heartbeat_interval=46", "--resource-version", "6"); isErr || !strings.Contains(text, `"resourceVersion":7`) {
			t.Fatalf("MCP settings set：%v %s", isErr, text)
		}
		// 审计：settings set 的摘要含字段名，打码字段是 ***。
		stdout, _, _ = h.cli("audit", "list", "--json", "--limit", "10", "--command", "settings set")
		if strings.Contains(stdout, "abc123") || !strings.Contains(stdout, `probe_external_token_sha256\":\"***\"`) || !strings.Contains(stdout, `heartbeat_interval`) {
			t.Fatalf("审计摘要：%s", stdout)
		}
		// 普通用户：forbidden。
		seedUser(t, bdb, "bob", "secret12")
		bob := newBrowser(t)
		if status, _ := bob.login(base, "bob", "secret12", ""); status != 200 {
			t.Fatalf("bob 登录：%d", status)
		}
		if status, fields, _ := bob.call("GET", base+"/api/v1/settings/show", "", nil); status != 403 || str(fields["code"]) != "forbidden" {
			t.Fatalf("普通用户 show：%d %v", status, fields)
		}
		if status, fields, _ := bob.call("POST", base+"/api/v1/settings/set", `{"set":{"heartbeat_interval":45},"resource-version":7}`, nil); status != 403 || str(fields["code"]) != "forbidden" {
			t.Fatalf("普通用户 set：%d %v", status, fields)
		}
	})
}
