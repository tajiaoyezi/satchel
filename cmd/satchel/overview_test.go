package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/pkg/xrpc"
)

// emptyOverview 断言一份 overview 的 JSON：四个分区一个不少，每个都是 items 为空数组、total 为 0（M1）。
func emptyOverview(t *testing.T, via, raw string) {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s 的 overview 不是 JSON 对象：%s", via, raw)
	}
	if str(out["apiVersion"]) == "" {
		t.Errorf("%s 的 overview 应当带 apiVersion：%s", via, raw)
	}
	for _, key := range []string{"servers", "users", "alerts", "tasks"} {
		var p struct {
			Items json.RawMessage `json:"items"`
			Total json.RawMessage `json:"total"`
		}
		if err := json.Unmarshal(out[key], &p); err != nil || string(p.Items) != "[]" || string(p.Total) != "0" {
			t.Errorf("%s 的分区 %s 应当是 items [] 与 total 0：%s", via, key, raw)
		}
	}
}

// master-overview 的端到端（双库）：没有身份是 unauthenticated；在建好管理员、写过一次设置、签过一把令牌的主控上，
// 本机管理员经 CLI（JSON 与文本）、普通用户的会话经 REST、只读令牌经 MCP 看到的都是四个空分区。
func TestOverviewEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		isolateHome(t)
		h := start(t, bdb)
		base := h.tcpURL

		if status, fields := get(t, http.DefaultClient, base+"/api/v1/overview"); status != 401 || str(fields["code"]) != "unauthenticated" {
			t.Fatalf("没有身份应当 401 unauthenticated：%d %v", status, fields)
		}

		// 建管理员、写一次设置、签一把只读令牌（新建默认只读）。
		if status, fields, _ := newBrowser(t).call("POST", base+"/api/v1/setup/init", `{"username":"admin","password":"secret12"}`, nil); status != 200 {
			t.Fatalf("setup init：%d %v", status, fields)
		}
		seedUser(t, bdb, "bob", "secret12")
		if _, stderr, code := h.cli("settings", "set", "--set", "branding_site_title=Satchel", "--resource-version", "1", "--json"); code != 0 {
			t.Fatalf("settings set：%d %s", code, stderr)
		}
		stdout, stderr, code := h.cliPrompt([]string{"secret12", ""}, "token", "create", "--name", "ro", "--verify-user", "admin", "--json")
		if code != 0 {
			t.Fatalf("token create：%d %s", code, stderr)
		}
		var ro createdToken
		decodeInto(t, stdout, &ro)
		if ro.Preset != "readonly" {
			t.Fatalf("新建的令牌应当默认只读：%s", stdout)
		}

		// 本机管理员经 socket 的 CLI：JSON 与文本形式。
		stdout, stderr, code = h.cli("overview", "--json")
		if code != 0 {
			t.Fatalf("satchel overview --json：%d %s", code, stderr)
		}
		emptyOverview(t, "CLI", stdout)
		if stdout, stderr, code = h.cli("overview"); code != 0 || stdout != "服务器：0\n用户：0\n告警：0\n待办：0\n" {
			t.Fatalf("文本形式应当是四行：%d %q %s", code, stdout, stderr)
		}

		// 普通用户的会话经 REST。
		bob := newBrowser(t)
		if status, fields := bob.login(base, "bob", "secret12", ""); status != 200 {
			t.Fatalf("bob 登录：%d %v", status, fields)
		}
		status, fields, _ := bob.call("GET", base+"/api/v1/overview", "", nil)
		if status != 200 {
			t.Fatalf("普通用户的会话应当看得到总览：%d %v", status, fields)
		}
		emptyOverview(t, "REST（普通用户）", fieldsJSON(fields))

		// 只读令牌经 MCP：工具结果不标为错误。
		text, isErr := runTool(t, mcpOverTCP(t, base, ro.Token), "overview")
		if isErr {
			t.Fatalf("只读令牌经 MCP 应当成功：%s", text)
		}
		emptyOverview(t, "MCP（只读令牌）", text)

		// 建 12 台服务器（其中 srv-01 连上主控），验证管理员在 servers 分区看到按 id 升序前 10 条与 total=12，
		// 普通用户的会话与令牌在 servers 分区仍只看到空数组与 0（且不报错）。
		tok1 := h.addServer("srv-01")
		a := mustDial(t, h)
		if !a.authenticate(masterPub(t, h), tok1, xrpc.ControlProtocol).OK {
			t.Fatal("srv-01 应当认证成功")
		}
		h.waitStatus("srv-01", "connected")
		for i := 2; i <= 12; i++ {
			h.addServer(fmt.Sprintf("srv-%02d", i))
		}

		stdout, stderr, code = h.cli("overview", "--json")
		if code != 0 {
			t.Fatalf("有服务器后 satchel overview --json：%d %s", code, stderr)
		}
		assertAdminServers := func(via, raw string) {
			t.Helper()
			var out map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				t.Fatalf("%s 的 overview 不是 JSON：%s", via, raw)
			}
			var servers struct {
				Items []struct {
					ID             int64   `json:"id"`
					Name           string  `json:"name"`
					ConnectionMode string  `json:"connection_mode"`
					Status         string  `json:"status"`
					LastHeartbeat  *string `json:"last_heartbeat"`
					IPAddress      *string `json:"ip_address"`
					IPAddressV6    string  `json:"ip_address_v6"`
				} `json:"items"`
				Total int `json:"total"`
			}
			if err := json.Unmarshal(out["servers"], &servers); err != nil {
				t.Fatalf("%s servers 解码失败：%v %s", via, err, raw)
			}
			if servers.Total != 12 || len(servers.Items) != 10 {
				t.Fatalf("%s 应当有 total=12、items=10：%d %d", via, servers.Total, len(servers.Items))
			}
			if servers.Items[0].Name != "srv-01" || servers.Items[0].Status != "connected" || servers.Items[0].IPAddress == nil || *servers.Items[0].IPAddress == "" ||
				servers.Items[0].LastHeartbeat == nil || servers.Items[9].Name != "srv-10" || servers.Items[9].Status != "pending" {
				t.Fatalf("%s 的服务器摘要内容不对：first=%+v last=%+v", via, servers.Items[0], servers.Items[9])
			}
			if strings.Contains(string(out["servers"]), tok1) || strings.Contains(string(out["servers"]), `"token"`) {
				t.Fatalf("%s 的 servers 分区不能包含令牌字段或原文：%s", via, string(out["servers"]))
			}
			for _, key := range []string{"users", "alerts", "tasks"} {
				if string(out[key]) != `{"items":[],"total":0}` {
					t.Fatalf("%s 未交付分区 %s 应当仍为空：%s", via, key, string(out[key]))
				}
			}
		}
		assertAdminServers("CLI", stdout)

		if stdout, stderr, code = h.cli("overview"); code != 0 ||
			!strings.HasPrefix(stdout, "服务器：12\n  srv-01\tconnected\twebsocket\t127.0.0.1\n  srv-02\tpending\twebsocket\t-\n") ||
			!strings.Contains(stdout, "\n用户：0\n告警：0\n待办：0\n") {
			t.Fatalf("有服务器时文本形式不对：%d %q %s", code, stdout, stderr)
		}

		text, isErr = runTool(t, mcpOverTCP(t, base, ro.Token), "overview")
		if isErr {
			t.Fatalf("管理员只读令牌经 MCP 应当成功：%s", text)
		}
		assertAdminServers("MCP（管理员只读令牌）", text)

		// 普通用户会话看 overview：servers 分区必须仍为空数组与 0。
		status, fields, _ = bob.call("GET", base+"/api/v1/overview", "", nil)
		if status != 200 {
			t.Fatalf("bob 看总览应当 200：%d %v", status, fields)
		}
		emptyOverview(t, "REST（普通用户有服务器时）", fieldsJSON(fields))

		// 普通用户签发的令牌看 overview：同样只看到空数组与 0。
		status, fields, _ = bob.call("POST", base+"/api/v1/token/create", `{"name":"bob-ro","verify-password":"secret12"}`, map[string]string{"Origin": base})
		if status != 200 {
			t.Fatalf("bob 签只读令牌：%d %v", status, fields)
		}
		text, isErr = runTool(t, mcpOverTCP(t, base, str(fields["token"])), "overview")
		if isErr {
			t.Fatalf("普通用户令牌经 MCP 看总览应当成功：%s", text)
		}
		emptyOverview(t, "MCP（普通用户令牌有服务器时）", text)
	})
}
