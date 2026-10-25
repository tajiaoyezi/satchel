package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

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

		// 加服务器。
		stdout, stderr, code := h.cli("server", "add", "hk-1", "--json")
		if code != 0 {
			t.Fatalf("server add：%s", stderr)
		}
		var added serverObj
		if err := json.Unmarshal([]byte(stdout), &added); err != nil || added.Spec.Name != "hk-1" || added.Spec.ConnectionMode != "websocket" ||
			added.Status.Status != "pending" || added.Metadata.ResourceVersion != 1 || !strings.Contains(stdout, `"token":"***"`) {
			t.Fatalf("新服务器的输出不对：%v\n%s", err, stdout)
		}
		token := h.addServer("hk-2") // 读库拿 token 原文
		if raw, err := base64.RawURLEncoding.DecodeString(token); len(token) != 43 || err != nil || len(raw) != 32 {
			t.Fatalf("token 应当是 43 个 base64url 字符：%q", token)
		}
		if e := h.cliErr("server", "add", "hk-1"); e.Code != v1.CodeNameTaken {
			t.Fatalf("名字重复应当是 name_taken：%+v", e)
		}
		if e := h.cliErr("server", "add", "hk-9", "--mode", "auto"); e.Code != v1.CodeBadRequest {
			t.Fatalf("其它连接模式应当是 bad_request：%+v", e)
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
		if s := h.server("hk-2"); s.Status.Token.Reveal() != v1.Redacted || s.Status.AgentToken != "" || s.Status.PullToken != "" {
			t.Fatalf("本机管理员也看不到 token 原文，另两把是空串：%q %q %q", s.Status.Token, s.Status.AgentToken, s.Status.PullToken)
		}
	})
}
