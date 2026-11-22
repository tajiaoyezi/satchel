package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/projection/dashboard"
)

// TestDashboardWSAndTelemetryEndToEnd 端到端验证：
//  1. GET /api/v1/ws/dashboard 走真实三道门、Cookie 同源防 CSWSH、Bearer 令牌与未登录/普通用户拦截；
//  2. server speed / server traffic-stats set / server logs list --grep 审计打码 / job list --server-name 全链路；
//  3. 内置定时任务总数保持 12（SQLite）或 11（PostgreSQL）不变。
func TestDashboardWSAndTelemetryEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		base := h.tcpURL
		alice := setupAdmin(t, base)
		roToken := issueToken(t, alice, base, "readonly")

		// 添加一台服务器并核对 token 原文绝不出现在 WS 广播中。
		addOut, addErr, code := h.cli("server", "add", "hk-dash", "--json")
		if code != 0 {
			t.Fatalf("server add 失败：%s", addErr)
		}
		if !strings.Contains(addOut, `"traffic_source":"system"`) {
			t.Fatalf("新建服务器默认 traffic_source 应为 system：%s", addOut)
		}
		tokOut, tokErr, code := h.cli("server", "token", "show", "hk-dash", "--json")
		if code != 0 {
			t.Fatalf("server token show 失败：%s", tokErr)
		}
		var tokResp struct {
			Token      string `json:"token"`
			AgentToken string `json:"agent_token"`
			PullToken  string `json:"pull_token"`
		}
		if err := json.Unmarshal([]byte(tokOut), &tokResp); err != nil || tokResp.Token == "" {
			t.Fatalf("解析 token show 失败：%v %s", err, tokOut)
		}

		wsEndpoint := "ws" + strings.TrimPrefix(base, "http") + dashboard.Path
		baseURL, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		cookieHdr := alice.c.Jar.Cookies(baseURL)
		if len(cookieHdr) == 0 {
			t.Fatal("管理员会话应当带 satchel_session cookie")
		}
		cookieStr := cookieHdr[0].Name + "=" + cookieHdr[0].Value

		// 1. 未认证 401；带 ?token= 400；跨站 Origin Cookie 403。
		if _, resp, err := websocket.Dial(tctx(t), wsEndpoint, nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("未认证连 /api/v1/ws/dashboard 应返回 401：err=%v resp=%v", err, resp)
		}
		if _, resp, err := websocket.Dial(tctx(t), wsEndpoint+"?token="+roToken, nil); err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("带 ?token= 连 /api/v1/ws/dashboard 应返回 400：err=%v resp=%v", err, resp)
		}
		if _, resp, err := websocket.Dial(tctx(t), wsEndpoint, &websocket.DialOptions{
			HTTPHeader: http.Header{"Cookie": {cookieStr}, "Origin": {"https://evil.example"}},
		}); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("跨站 Origin 带 Cookie 连 /api/v1/ws/dashboard 应返回 403：err=%v resp=%v", err, resp)
		}

		// 2. 同源 Cookie 建连立即收到首帧且不含三把服务器密钥。
		conn, _, err := websocket.Dial(tctx(t), wsEndpoint, &websocket.DialOptions{
			HTTPHeader: http.Header{"Cookie": {cookieStr}, "Origin": {base}},
		})
		if err != nil {
			t.Fatalf("同源管理员 Cookie 连 /api/v1/ws/dashboard 应当成功：%v", err)
		}
		_, firstRaw, err := conn.Read(tctx(t))
		if err != nil {
			t.Fatalf("读取仪表盘首帧失败：%v", err)
		}
		for _, secret := range []string{tokResp.Token, tokResp.AgentToken, tokResp.PullToken} {
			if secret != "" && strings.Contains(string(firstRaw), secret) {
				t.Fatalf("仪表盘 WS 帧绝不能泄露服务器凭据：%s", firstRaw)
			}
		}
		var frame dashboard.Frame
		if err := json.Unmarshal(firstRaw, &frame); err != nil || frame.Type != "realtime" || frame.Summary.TotalServers != 1 || len(frame.Servers) != 1 || frame.Servers[0].Name != "hk-dash" {
			t.Fatalf("仪表盘首帧内容不对：%v %s", err, firstRaw)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "done")

		// 3. Bearer 管理员只读令牌也可连接。
		bconn, _, err := websocket.Dial(tctx(t), wsEndpoint, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + roToken}},
		})
		if err != nil {
			t.Fatalf("管理员 Bearer 令牌连 /api/v1/ws/dashboard 应当成功：%v", err)
		}
		if _, _, err := bconn.Read(tctx(t)); err != nil {
			t.Fatalf("Bearer 首帧读取失败：%v", err)
		}
		_ = bconn.Close(websocket.StatusNormalClosure, "done")

		// 4. server logs list --grep 参数在审计中打码；job list --server-name 正常返回空页。
		_, _, _ = h.cli("server", "logs", "list", "hk-dash", "--grep", "top-secret-grep-value", "--json")
		auditOut, auditErr, code := h.cli("audit", "list", "--command", "server logs", "--json")
		if code != 0 {
			t.Fatalf("audit list 失败：%s", auditErr)
		}
		if strings.Contains(auditOut, "top-secret-grep-value") || (!strings.Contains(auditOut, `"grep":"***"`) && !strings.Contains(auditOut, `\"grep\":\"***\"`)) {
			t.Fatalf("server logs list --grep 应当在审计摘要里打码：%s", auditOut)
		}
		jobOut, jobErr, code := h.cli("job", "list", "--server-name", "hk-dash", "--json")
		if code != 0 || !strings.Contains(jobOut, `"total":0`) {
			t.Fatalf("job list --server-name 应当返回 total=0：code=%d out=%s err=%s", code, jobOut, jobErr)
		}

		// 5. 验证内置定时任务总数未增加（SQLite 12 / PostgreSQL 11）。
		schedOut, schedErr, code := h.cli("schedule", "list", "--json")
		if code != 0 {
			t.Fatalf("schedule list 失败：%s", schedErr)
		}
		var schedPage struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal([]byte(schedOut), &schedPage); err != nil {
			t.Fatalf("解析 schedule list 失败：%v", err)
		}
		if schedPage.Total != 12 && schedPage.Total != 11 {
			t.Fatalf("内置定时任务总数不应当增加，得到 %d", schedPage.Total)
		}

		// 静默模式开启后（模拟公网来源）仪表盘 WS 同样归面板门拦截为 404。
		clk := newClock()
		h.setClock(clk)
		h.gates("silent_mode=true", "silent_mode_timeout=1", `trusted_proxies=[{"cidr":"127.0.0.1","header":"X-Real-IP"}]`)
		clk.advance(2 * time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, resp, err := websocket.Dial(ctx, wsEndpoint, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + roToken}, "X-Real-IP": {"203.0.113.55"}},
		}); err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
			t.Fatalf("静默模式锁定后面板 WS 应当被藏为 404：err=%v resp=%v", err, resp)
		}
	})
}

// TestDashboardWSRevocationEndToEnd 验证已建连的 /api/v1/ws/dashboard 在登出、吊销令牌、账号停用后立即停止推送并断开。
func TestDashboardWSRevocationEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		base := h.tcpURL
		admin := setupAdmin(t, base)
		roToken := issueToken(t, admin, base, "readonly")

		// 将仪表盘刷新间隔缩到最小 1000ms，便于快速验证账号停用的周期复核断开。
		showOut, showErr, code := h.cli("settings", "show", "--json")
		if code != 0 {
			t.Fatalf("settings show 失败：%s", showErr)
		}
		var setObj struct {
			Metadata struct {
				ResourceVersion int `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(showOut), &setObj); err != nil {
			t.Fatalf("解析 settings show 失败：%v", err)
		}
		if _, setErr, code := h.cli("settings", "set", "--set", "dashboard_refresh_interval_ms=1000", "--resource-version", strconv.Itoa(setObj.Metadata.ResourceVersion), "--json"); code != 0 {
			t.Fatalf("设置刷新间隔失败：%s", setErr)
		}

		wsEndpoint := "ws" + strings.TrimPrefix(base, "http") + dashboard.Path
		baseURL, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		cookieOf := func(b *browser) string {
			t.Helper()
			cs := b.c.Jar.Cookies(baseURL)
			if len(cs) == 0 {
				t.Fatal("缺少 satchel_session cookie")
			}
			return cs[0].Name + "=" + cs[0].Value
		}

		// 1. 管理员会话登出（DELETE /api/v1/session）后，已建连的 WS 立即以 1008 断开。
		sessConn, _, err := websocket.Dial(tctx(t), wsEndpoint, &websocket.DialOptions{
			HTTPHeader: http.Header{"Cookie": {cookieOf(admin)}, "Origin": {base}},
		})
		if err != nil {
			t.Fatalf("建连会话 WS 失败：%v", err)
		}
		if _, _, err := sessConn.Read(tctx(t)); err != nil {
			t.Fatalf("读会话首帧失败：%v", err)
		}
		if st, fields, _ := admin.call("DELETE", base+"/api/v1/session", "", map[string]string{"Origin": base}); st != http.StatusOK {
			t.Fatalf("登出应当 200，得到 %d %v", st, fields)
		}
		rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, rerr := sessConn.Read(rctx)
		rcancel()
		if websocket.CloseStatus(rerr) != websocket.StatusPolicyViolation {
			t.Fatalf("会话登出后 WS 应立即收到 StatusPolicyViolation (1008)，得到 %v", rerr)
		}

		// 重新登录以继续测令牌吊销与账号停用。
		admin = newBrowser(t)
		if st, fields, _ := admin.call("POST", base+"/api/v1/session", `{"username":"admin","password":"secret12"}`, nil); st != http.StatusOK {
			t.Fatalf("重新登录失败：%d %v", st, fields)
		}

		// 2. API 令牌吊销后，已建连的 Bearer WS 立即以 1008 断开。
		tokConn, _, err := websocket.Dial(tctx(t), wsEndpoint, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + roToken}},
		})
		if err != nil {
			t.Fatalf("建连 Bearer WS 失败：%v", err)
		}
		if _, _, err := tokConn.Read(tctx(t)); err != nil {
			t.Fatalf("读 Bearer 首帧失败：%v", err)
		}
		listOut, listErr, code := h.cli("token", "list", "--json")
		if code != 0 {
			t.Fatalf("token list 失败：%s", listErr)
		}
		var tokList struct {
			Items []struct {
				ID int64 `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(listOut), &tokList); err != nil || len(tokList.Items) == 0 {
			t.Fatalf("解析 token list 失败：%v %s", err, listOut)
		}
		if st, fields, _ := admin.call("POST", base+"/api/v1/token/revoke/"+strconv.FormatInt(tokList.Items[0].ID, 10), `{"verify-password":"secret12"}`, map[string]string{"Origin": base}); st != http.StatusOK {
			t.Fatalf("吊销令牌失败：%d %v", st, fields)
		}
		rctx2, rcancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, rerr2 := tokConn.Read(rctx2)
		rcancel2()
		if websocket.CloseStatus(rerr2) != websocket.StatusPolicyViolation {
			t.Fatalf("令牌吊销后 WS 应立即收到 StatusPolicyViolation (1008)，得到 %v", rerr2)
		}

		// 3. 账号被停用（is_active=false）后，已建连的会话 WS 在下一轮广播前被复核拦截并以 1008 断开、绝不再发帧。
		sessConn2, _, err := websocket.Dial(tctx(t), wsEndpoint, &websocket.DialOptions{
			HTTPHeader: http.Header{"Cookie": {cookieOf(admin)}, "Origin": {base}},
		})
		if err != nil {
			t.Fatalf("二次建连会话 WS 失败：%v", err)
		}
		if _, _, err := sessConn2.Read(tctx(t)); err != nil {
			t.Fatalf("读二次会话首帧失败：%v", err)
		}
		if _, err := bdb.NewUpdate().TableExpr("users").Set("is_active = ?", false).Where("username = ?", "admin").Exec(context.Background()); err != nil {
			t.Fatalf("停用账号失败：%v", err)
		}
		rctx3, rcancel3 := context.WithTimeout(context.Background(), 4*time.Second)
		_, rawAfterDisable, rerr3 := sessConn2.Read(rctx3)
		rcancel3()
		if websocket.CloseStatus(rerr3) != websocket.StatusPolicyViolation {
			t.Fatalf("账号停用后 WS 应在下一轮广播前断开（1008）且不再发帧，得到 err=%v data=%s", rerr3, rawAfterDisable)
		}
	})
}
