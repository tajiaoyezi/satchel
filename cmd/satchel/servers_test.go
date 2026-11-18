package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/securechan"
	"github.com/satchel/satchel/pkg/xrpc"
)

// cliErr 跑一条应当失败的命令，返回它的四字段错误。
func (h *harness) cliErr(args ...string) *v1.Error {
	h.t.Helper()
	_, stderr, code := h.cli(append(args, "--json")...)
	if code == 0 {
		h.t.Fatalf("%v 应当失败", args)
	}
	var e v1.Error
	if err := json.Unmarshal([]byte(stderr), &e); err != nil {
		h.t.Fatalf("stderr 不是四字段错误：%s", stderr)
	}
	return &e
}

// node-pairing「加服务器」「生成安装命令」与 node-channel「看服务器与在线状态」（双库）。
func TestServerCommands(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		base := h.tcpURL
		admin := setupAdmin(t, base)
		ro := issueToken(t, admin, base, "readonly")
		ops := issueToken(t, admin, base, "ops")

		// 加服务器：省略 --mode 默认为 auto，三把凭据在输出中均打码。
		stdout, stderr, code := h.cli("server", "add", "hk-1", "--json")
		if code != 0 {
			t.Fatalf("server add：%s", stderr)
		}
		var added serverObj
		if err := json.Unmarshal([]byte(stdout), &added); err != nil || added.Spec.Name != "hk-1" || added.Spec.ConnectionMode != "auto" ||
			added.Status.Status != "pending" || added.Metadata.ResourceVersion != 1 ||
			!strings.Contains(stdout, `"token":"***"`) || !strings.Contains(stdout, `"agent_token":"***"`) || !strings.Contains(stdout, `"pull_token":"***"`) {
			t.Fatalf("新服务器的输出不对：%v\n%s", err, stdout)
		}
		// 建一台显式 websocket 模式的服务器 hk-2（测向后兼容的 install-command 与连接生命周期）。
		if _, stderr, code := h.cli("server", "add", "hk-2", "--mode", "websocket", "--json"); code != 0 {
			t.Fatalf("server add hk-2 websocket：%s", stderr)
		}
		var token string
		if err := h.db.NewSelect().TableExpr("servers").Column("token").Where("name = ?", "hk-2").Scan(context.Background(), &token); err != nil {
			t.Fatal(err)
		}
		if raw, err := base64.RawURLEncoding.DecodeString(token); len(token) != 43 || err != nil || len(raw) != 32 {
			t.Fatalf("token 应当是 43 个 base64url 字符：%q", token)
		}
		if e := h.cliErr("server", "add", "hk-1"); e.Code != v1.CodeNameTaken {
			t.Fatalf("名字重复应当是 name_taken：%+v", e)
		}
		if e := h.cliErr("server", "add", "hk-9", "--mode", "invalid"); e.Code != v1.CodeBadRequest {
			t.Fatalf("非法连接模式应当是 bad_request：%+v", e)
		}
		if e := h.cliErr("server", "add", "hk-9", "--listen-port", "23889", "--pull-port", "23889"); e.Code != v1.CodeBadRequest {
			t.Fatalf("相同端口应当是 bad_request：%+v", e)
		}
		if e := h.cliErr("server", "add", "hk-9", "--mode", "pull"); e.Code != v1.CodeBadRequest {
			t.Fatalf("pull 缺少可用地址应当是 bad_request：%+v", e)
		}
		if r := send(t, nil, "POST", base+"/api/v1/server/add/hk-3", map[string]string{"Authorization": "Bearer " + ro}, `{}`); r.status != 403 || r.code() != "forbidden" {
			t.Fatalf("只读令牌建不了：%d %s", r.status, r.body)
		}
		if n, _ := h.db.NewSelect().TableExpr("servers").Count(context.Background()); n != 2 {
			t.Fatalf("应当只有两台服务器：%d", n)
		}

		// 安装命令：没有主控地址、http 主控地址都是 conflict。
		if e := h.cliErr("server", "install-command", "hk-2"); e.Code != v1.CodeConflict || !strings.Contains(e.Next, "settings master-url set") {
			t.Fatalf("没有主控地址应当 conflict 并指出 settings master-url set：%+v", e)
		}
		setMasterURL := func(url string) {
			env, _, _ := h.settingsCLI("settings", "show")
			if _, stderr, code := h.human("settings", "master-url", "set", "--url", url, "--resource-version", jsonInt(env.Metadata.ResourceVersion)); code != 0 {
				t.Fatalf("master-url set %s：%s", url, stderr)
			}
		}
		setMasterURL("http://203.0.113.5:12889")
		if e := h.cliErr("server", "install-command", "hk-2"); e.Code != v1.CodeConflict {
			t.Fatalf("http 主控地址应当 conflict：%+v", e)
		}
		setMasterURL("https://panel.example.com")
		stdout, stderr, code = h.cli("server", "install-command", "hk-2", "--json")
		if code != 0 {
			t.Fatalf("install-command：%s", stderr)
		}
		var ic struct {
			Server, Command, MasterURL, Fingerprint string
		}
		var raw map[string]string
		_ = json.Unmarshal([]byte(stdout), &raw)
		ic.Server, ic.Command, ic.MasterURL, ic.Fingerprint = raw["server"], raw["command"], raw["master_url"], raw["master_key_fingerprint"]
		pub := masterPub(t, h)
		key := securechan.EncodePublicKey(pub)
		if !strings.HasPrefix(ic.Command, "curl -fsSL 'https://panel.example.com/api/remote/install.sh'") ||
			!strings.Contains(ic.Command, "--master-url 'https://panel.example.com'") || !strings.Contains(ic.Command, "--token '"+token+"'") ||
			!strings.Contains(ic.Command, "--master-key '"+key+"'") || !strings.HasSuffix(ic.Command, "--mode websocket") ||
			ic.Fingerprint != securechan.Fingerprint(pub) || ic.Server != "hk-2" || ic.MasterURL != "https://panel.example.com" {
			t.Fatalf("本机管理员拿到的安装命令不对：%s", stdout)
		}
		r := send(t, nil, "GET", base+"/api/v1/server/install-command/hk-2", map[string]string{"Authorization": "Bearer " + ops}, "")
		if r.status != 200 || strings.Contains(r.body, token) || !strings.Contains(str(r.fields["command"]), "--token '***'") ||
			!strings.Contains(str(r.fields["command"]), "--master-key '"+key+"'") {
			t.Fatalf("没开密钥读取时 token 应当打码、其余照常：%d %s", r.status, r.body)
		}
		if e := h.cliErr("server", "install-command", "no-such"); e.Code != v1.CodeNotFound {
			t.Fatalf("服务器不存在应当 not_found：%+v", e)
		}

		// 列出与查看：hk-2 连上，hk-1 还是 pending；只读令牌看得到，token 一律打码。
		a := mustDial(t, h)
		if !a.authenticate(pub, token, xrpc.ControlProtocol).OK {
			t.Fatal("hk-2 应当认证成功")
		}
		h.waitStatus("hk-2", "connected")
		r = send(t, nil, "GET", base+"/api/v1/server", map[string]string{"Authorization": "Bearer " + ro}, "")
		var page struct {
			Items []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
				Mode   string `json:"connection_mode"`
			} `json:"items"`
			Total int `json:"total"`
		}
		if r.status != 200 || json.Unmarshal([]byte(r.body), &page) != nil || page.Total != 2 || len(page.Items) != 2 ||
			page.Items[0].Name != "hk-1" || page.Items[0].Status != "pending" || page.Items[1].Status != "connected" || page.Items[1].Mode != "websocket" {
			t.Fatalf("列表不对：%d %s", r.status, r.body)
		}
		r = send(t, nil, "GET", base+"/api/v1/server/get/hk-2", map[string]string{"Authorization": "Bearer " + ro}, "")
		if r.status != 200 || str(r.fields["kind"]) != "Server" || strings.Contains(r.body, token) || !strings.Contains(r.body, `"token":"***"`) {
			t.Fatalf("server get 的 token 应当打码：%d %s", r.status, r.body)
		}
		if s := h.server("hk-2"); s.Status.Token.Reveal() != v1.Redacted || s.Status.AgentToken.Reveal() != v1.Redacted || s.Status.PullToken.Reveal() != v1.Redacted {
			t.Fatalf("本机管理员查看 server get 时三把凭据都应当打码：%q %q %q", s.Status.Token, s.Status.AgentToken, s.Status.PullToken)
		}

		// 普通用户（会话及其签发的 ops 令牌）不能执行任何 server 命令，一律返回 403 forbidden。
		seedUser(t, bdb, "bob", "secret12")
		bob := newBrowser(t)
		if status, fields := bob.login(base, "bob", "secret12", ""); status != 200 {
			t.Fatalf("bob 登录：%d %v", status, fields)
		}
		status, fields, _ := bob.call("POST", base+"/api/v1/token/create", `{"name":"bob-ops","preset":"ops","verify-password":"secret12"}`, map[string]string{"Origin": base})
		if status != 200 {
			t.Fatalf("bob 签 ops 令牌：%d %v", status, fields)
		}
		bobOps := str(fields["token"])
		calls := []struct {
			method, path, body string
		}{
			{"POST", "/api/v1/server/add/hk-bob", `{}`},
			{"GET", "/api/v1/server/install-command/hk-2", ""},
			{"GET", "/api/v1/server", ""},
			{"GET", "/api/v1/server/get/hk-2", ""},
			{"POST", "/api/v1/server/limit/set/hk-2", `{"inbound":"vless-in","credential":"a","mbps":5}`},
			{"POST", "/api/v1/server/limit/clear/hk-2", `{"inbound":"vless-in","credential":"a"}`},
			{"POST", "/api/v1/server/kick/hk-2", `{"credential":"a"}`},
			{"GET", "/api/v1/server/connections/hk-2?inbound=vless-in&credential=a", ""},
			{"GET", "/api/v1/server/traffic/hk-2", ""},
			{"POST", "/api/v1/server/restart-kernel/hk-2", `{"confirm":"hk-2"}`},
			{"GET", "/api/v1/server/token/show/hk-2", ""},
			{"POST", "/api/v1/server/token/rotate/hk-2", `{"confirm":"hk-2"}`},
			{"POST", "/api/v1/server/token/revoke/hk-2", `{"confirm":"hk-2"}`},
		}
		for _, tc := range calls {
			if r := send(t, bob.c, tc.method, base+tc.path, map[string]string{"Origin": base}, tc.body); r.status != 403 || r.code() != "forbidden" {
				t.Fatalf("普通用户会话调 %s %s 应当 403 forbidden，得到 %d %s", tc.method, tc.path, r.status, r.body)
			}
			if r := send(t, nil, tc.method, base+tc.path, map[string]string{"Authorization": "Bearer " + bobOps}, tc.body); r.status != 403 || r.code() != "forbidden" {
				t.Fatalf("普通用户 ops 令牌调 %s %s 应当 403 forbidden，得到 %d %s", tc.method, tc.path, r.status, r.body)
			}
		}
		if n, _ := h.db.NewSelect().TableExpr("servers").Count(context.Background()); n != 2 {
			t.Fatalf("普通用户被拒后服务器总数应当仍为 2：%d", n)
		}

		// 凭据查看、轮换、吊销与多模式 install-command 端到端验证。
		// 1. 多模式 install-command：固定 4 字段，按 secrets 权限打码或原文输出三把凭据与自定义端口。
		if _, stderr, code := h.cli("server", "add", "hk-auto", "--mode", "auto", "--listen-port", "24889", "--pull-port", "24890", "--pull-address", "203.0.113.5", "--json"); code != 0 {
			t.Fatalf("server add hk-auto：%s", stderr)
		}
		stdout, stderr, code = h.cli("server", "install-command", "hk-auto", "--json")
		if code != 0 {
			t.Fatalf("hk-auto install-command：%s", stderr)
		}
		var icMap map[string]string
		if err := json.Unmarshal([]byte(stdout), &icMap); err != nil {
			t.Fatalf("install-command JSON 解码失败：%s %v", stdout, err)
		}
		delete(icMap, "apiVersion")
		if len(icMap) != 4 {
			t.Fatalf("install-command 应当保持恰好四个业务 JSON 字段（server/command/master_url/master_key_fingerprint）：%s", stdout)
		}
		for _, k := range []string{"server", "command", "master_url", "master_key_fingerprint"} {
			if icMap[k] == "" {
				t.Fatalf("install-command 缺少字段 %q：%s", k, stdout)
			}
		}
		for _, must := range []string{"--mode auto", "--agent-token '", "--pull-token '", "--listen-port 24889", "--pull-port 24890"} {
			if !strings.Contains(icMap["command"], must) {
				t.Fatalf("auto 安装命令缺少 %q：%s", must, icMap["command"])
			}
		}
		if r := send(t, nil, "GET", base+"/api/v1/server/install-command/hk-auto", map[string]string{"Authorization": "Bearer " + ops}, ""); r.status != 200 ||
			r.header.Get("Cache-Control") != "no-store" ||
			!strings.Contains(str(r.fields["command"]), "--token '***'") ||
			!strings.Contains(str(r.fields["command"]), "--agent-token '***'") ||
			!strings.Contains(str(r.fields["command"]), "--pull-token '***'") {
			t.Fatalf("ops 无 secrets 时三把凭据均应打码且带 no-store：%d %q %s", r.status, r.header.Get("Cache-Control"), r.body)
		}
		if r := send(t, nil, "GET", base+"/api/v1/settings/show", map[string]string{"Authorization": "Bearer " + ops}, ""); r.status != 200 ||
			r.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("settings show 应当带 Cache-Control: no-store：%d %q %s", r.status, r.header.Get("Cache-Control"), r.body)
		}

		// 2. server token show：本机管理员读原文，ops 无 secrets 返回 403，no-such 返回 not_found，审计不含原文。
		stdout, stderr, code = h.cli("server", "token", "show", "hk-2", "--json")
		if code != 0 {
			t.Fatalf("token show：%s", stderr)
		}
		var tv struct {
			Server          string `json:"server"`
			Token           string `json:"token"`
			AgentToken      string `json:"agent_token"`
			PullToken       string `json:"pull_token"`
			RotationPending bool   `json:"rotation_pending"`
			RevokePending   bool   `json:"revoke_pending"`
		}
		if err := json.Unmarshal([]byte(stdout), &tv); err != nil || tv.Token != token || len(tv.AgentToken) != 43 || len(tv.PullToken) != 43 {
			t.Fatalf("token show 输出不对：%s %v", stdout, err)
		}
		if r := send(t, nil, "GET", base+"/api/v1/server/token/show/hk-2", map[string]string{"Authorization": "Bearer " + ops}, ""); r.status != 403 || r.code() != "forbidden" {
			t.Fatalf("无 secrets 的 ops 令牌查看凭据应当 403：%d %s", r.status, r.body)
		}
		if e := h.cliErr("server", "token", "show", "no-such"); e.Code != v1.CodeNotFound {
			t.Fatalf("不存在的服务器应报 not_found：%+v", e)
		}

		// 3. server token rotate：缺 confirm / 缺 permission 拒绝；离线不可达回滚返 unavailable；在线节点确认完成无缝更新。
		if e := h.cliErr("server", "token", "rotate", "hk-2"); e.Code != v1.CodeConfirmRequired {
			t.Fatalf("缺 confirm 应当报错：%+v", e)
		}
		if r := send(t, nil, "POST", base+"/api/v1/server/token/rotate/hk-2", map[string]string{"Authorization": "Bearer " + ops}, `{"confirm":"hk-2"}`); r.status != 403 || r.code() != "forbidden" {
			t.Fatalf("ops 令牌无 permission 类应当 403：%d %s", r.status, r.body)
		}
		if e := h.cliErr("server", "token", "rotate", "hk-1", "--confirm", "hk-1"); e.Code != v1.CodeUnavailable {
			t.Fatalf("离线节点轮换应返 unavailable 并回滚：%+v", e)
		}

		// 让假节点 hk-2 受理 token.rotate。
		rotatedCh := make(chan xrpc.TokenRotateParams, 1)
		aClosed := a.serveMethods(xrpc.Methods{
			xrpc.MethodTokenRotate: func(_ context.Context, raw json.RawMessage) (any, error) {
				var p xrpc.TokenRotateParams
				_ = json.Unmarshal(raw, &p)
				rotatedCh <- p
				return xrpc.TokenRotateAck{UpdatedAt: time.Now().UTC()}, nil
			},
		})
		stdout, stderr, code = h.cli("server", "token", "rotate", "hk-2", "--confirm", "hk-2", "--json")
		if code != 0 {
			t.Fatalf("在线轮换应当成功：%s", stderr)
		}
		rotParams := <-rotatedCh
		if rotParams.Token == "" || rotParams.Token == token || rotParams.AgentToken == "" || rotParams.PullToken == "" {
			t.Fatalf("节点应当收到三把新凭据：%+v", rotParams)
		}
		if s := h.server("hk-2"); s.Status.RotationPending || s.Status.LastRotatedAt == nil || s.Metadata.ResourceVersion != 1 {
			t.Fatalf("轮换确认后 rotation_pending 应当为 false、last_rotated_at 非空、rv=1：%+v", s)
		}

		// 4. server token revoke：立刻断开 WS、置 revoke_pending=true、旧 token 被拒、拿最新 token 连回心跳后清 revoke_pending。
		stdout, stderr, code = h.cli("server", "token", "revoke", "hk-2", "--which", "token", "--confirm", "hk-2", "--json")
		if code != 0 {
			t.Fatalf("吊销 token 应当成功：%s", stderr)
		}
		select {
		case <-aClosed:
		case <-time.After(5 * time.Second):
			t.Fatal("吊销 token 后原有 WS 连接应当立即被主控断开")
		}
		if s := h.server("hk-2"); !s.Status.RevokePending {
			t.Fatalf("曾上线服务器吊销 token 后 revoke_pending 应当为 true：%+v", s.Status)
		}
		badReconnect := mustDial(t, h)
		if res := badReconnect.authenticate(pub, rotParams.Token, xrpc.ControlProtocol); res.OK || res.Reason != xrpc.AuthInvalidToken {
			t.Fatalf("被吊销的旧 token 重连应当被拒 invalid_token：%+v", res)
		}
		// 取吊销后的最新 token 重连并心跳，revoke_pending 清除。
		var revokedToken string
		_ = h.db.NewSelect().TableExpr("servers").Column("token").Where("name = ?", "hk-2").Scan(context.Background(), &revokedToken)
		if res := mustDial(t, h).authenticate(pub, revokedToken, xrpc.ControlProtocol); !res.OK {
			t.Fatalf("使用吊销后新生成的 token 重连应当成功：%+v", res)
		}
		if s := h.server("hk-2"); s.Status.RevokePending {
			t.Fatalf("用新 token 重新认证上线后 revoke_pending 应当清回 false：%+v", s.Status)
		}
		// 审计记录中绝不出现任意凭据原文。
		auditOut, _, code := h.cli("audit", "list", "--json")
		if code != 0 || strings.Contains(auditOut, token) || strings.Contains(auditOut, rotParams.Token) || strings.Contains(auditOut, revokedToken) {
			t.Fatalf("审计记录中不得泄露任何凭据原文：%s", auditOut)
		}
	})
}
