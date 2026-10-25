package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/securechan"
	"github.com/satchel/satchel/pkg/xrpc"
)

// fakeAgent 是测试里的节点：握手手工做（拿着会话，才能重放与伪造信封），每一帧都由用例决定发什么（m2-02 design 第 10 条）。
type fakeAgent struct {
	t    *testing.T
	ws   *websocket.Conn
	sess *securechan.Session
}

func wsURL(h *harness) string { return "ws" + strings.TrimPrefix(h.tcpURL, "http") + "/api/remote/ws" }

// masterPub 是这台主控的通信身份公钥（读数据目录里的 master.key）。
func masterPub(t *testing.T, h *harness) ed25519.PublicKey {
	t.Helper()
	key, err := agentconn.LoadIdentity(h.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return key.Public().(ed25519.PublicKey)
}

func tctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// dialAgent 连上节点通道；升级被拒时返回回应。
func dialAgent(t *testing.T, h *harness) (*fakeAgent, *http.Response, error) {
	t.Helper()
	ws, resp, err := websocket.Dial(tctx(t), wsURL(h), &websocket.DialOptions{HTTPHeader: http.Header{"User-Agent": {"satchel-agent/test"}}})
	if err != nil {
		return nil, resp, err
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &fakeAgent{t: t, ws: ws}, resp, nil
}

func mustDial(t *testing.T, h *harness) *fakeAgent {
	t.Helper()
	a, _, err := dialAgent(t, h)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (a *fakeAgent) writeText(typ string, payload any) {
	a.t.Helper()
	raw, _ := json.Marshal(payload)
	data, _ := json.Marshal(xrpc.Message{Type: typ, Payload: raw})
	if err := a.ws.Write(tctx(a.t), websocket.MessageText, data); err != nil {
		a.t.Fatal(err)
	}
}

// handshake 照协议手工握手，用 pub 验主控的签名。
func (a *fakeAgent) handshake(pub ed25519.PublicKey) {
	a.t.Helper()
	priv, eph, _ := securechan.GenerateEphemeral()
	a.writeText(xrpc.TypeKeyExchange, xrpc.KeyExchange{AgentEphemeral: eph})
	typ, data, err := a.ws.Read(tctx(a.t))
	if err != nil || typ != websocket.MessageText {
		a.t.Fatalf("应当收到明文的握手响应：%v", err)
	}
	var m xrpc.Message
	var resp xrpc.KeyExchangeResp
	if json.Unmarshal(data, &m) != nil || m.Type != xrpc.TypeKeyExchangeResp || xrpc.Decode(m, &resp) != nil {
		a.t.Fatalf("握手响应不对：%s", data)
	}
	if !securechan.VerifyHandshake(pub, eph, resp.MasterEphemeral, resp.Signature) {
		a.t.Fatal("主控的握手签名验不过")
	}
	shared, _ := securechan.ComputeSharedSecret(priv, resp.MasterEphemeral)
	if a.sess, err = securechan.DeriveSession(shared, eph, resp.MasterEphemeral, false); err != nil {
		a.t.Fatal(err)
	}
}

// send 加密发一条消息，返回发出去的信封（重放用）。
func (a *fakeAgent) send(typ string, payload any) []byte {
	a.t.Helper()
	raw, _ := json.Marshal(payload)
	data, _ := json.Marshal(xrpc.Message{Type: typ, Payload: raw})
	env, _ := a.sess.Encrypt(data)
	a.sendRaw(env)
	return env
}

func (a *fakeAgent) sendRaw(env []byte) {
	a.t.Helper()
	if err := a.ws.Write(tctx(a.t), websocket.MessageBinary, env); err != nil {
		a.t.Fatal(err)
	}
}

// recv 读一条加密消息；连接断开时返回错误。
func (a *fakeAgent) recv(d time.Duration) (xrpc.Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	typ, data, err := a.ws.Read(ctx)
	if err != nil {
		return xrpc.Message{}, err
	}
	if typ != websocket.MessageBinary {
		return xrpc.Message{}, errors.New("收到明文帧")
	}
	plain, err := a.sess.Decrypt(data)
	if err != nil {
		return xrpc.Message{}, err
	}
	var m xrpc.Message
	return m, json.Unmarshal(plain, &m)
}

// closedByMaster 报告主控是否在限时内断开了连接（读到的是关闭而不是超时）。
func (a *fakeAgent) closedByMaster() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := a.ws.Read(ctx); err != nil {
			return ctx.Err() == nil
		}
	}
}

// authenticate 握手并认证，返回认证结果。
func (a *fakeAgent) authenticate(pub ed25519.PublicKey, token string, protocol int) xrpc.AuthResult {
	a.t.Helper()
	a.handshake(pub)
	a.send(xrpc.TypeAuth, xrpc.Auth{Token: token, AgentVersion: "0.2.0-test", ControlProtocol: protocol})
	m, err := a.recv(5 * time.Second)
	var res xrpc.AuthResult
	if err != nil || m.Type != xrpc.TypeAuthResult || xrpc.Decode(m, &res) != nil {
		a.t.Fatalf("应当收到认证结果：%+v %v", m, err)
	}
	return res
}

// heartbeat 发一次心跳并等确认。
func (a *fakeAgent) heartbeat(boot time.Time) []byte {
	a.t.Helper()
	env := a.send(xrpc.TypeHeartbeat, xrpc.Heartbeat{BootTime: boot})
	if m, err := a.recv(5 * time.Second); err != nil || m.Type != xrpc.TypeHeartbeatAck {
		a.t.Fatalf("应当收到心跳确认：%+v %v", m, err)
	}
	return env
}

// serveCalls 让假节点在后台回应主控的请求（agent.info，不认识的方法照规矩回错误），并跳过心跳确认。
// 之后这条连接只能由它读；返回的通道在读到错误（连接被断开）时关闭。
func (a *fakeAgent) serveCalls() <-chan struct{} {
	methods := xrpc.Methods{xrpc.MethodAgentInfo: func(context.Context, json.RawMessage) (any, error) {
		return xrpc.AgentInfo{AgentVersion: "0.2.0-test", ControlProtocol: xrpc.ControlProtocol}, nil
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			m, err := a.recv(time.Minute)
			if err != nil {
				return
			}
			if m.Type != xrpc.TypeCall {
				continue
			}
			var call xrpc.Call
			_ = xrpc.Decode(m, &call)
			raw, _ := json.Marshal(xrpc.Message{Type: xrpc.TypeReply, Payload: mustJSON(methods.Serve(context.Background(), call))})
			env, _ := a.sess.Encrypt(raw)
			_ = a.ws.Write(context.Background(), websocket.MessageBinary, env)
		}
	}()
	return done
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// serverObj 是 server get 的输出（kind Server）。
type serverObj = v1.Object[v1.ServerSpec, v1.ServerStatus]

func (h *harness) server(name string) serverObj {
	h.t.Helper()
	stdout, stderr, code := h.cli("server", "get", name, "--json")
	if code != 0 {
		h.t.Fatalf("server get %s：%s", name, stderr)
	}
	var o serverObj
	if err := json.Unmarshal([]byte(stdout), &o); err != nil {
		h.t.Fatalf("server get 的输出不是 Server：%v\n%s", err, stdout)
	}
	return o
}

// addServer 建一台服务器，返回它的 token 原文（直接读库：输出里一律打码）。
func (h *harness) addServer(name string) string {
	h.t.Helper()
	if _, stderr, code := h.cli("server", "add", name, "--json"); code != 0 {
		h.t.Fatalf("server add %s：%s", name, stderr)
	}
	var token string
	if err := h.db.NewSelect().TableExpr("servers").Column("token").Where("name = ?", name).Scan(context.Background(), &token); err != nil {
		h.t.Fatal(err)
	}
	return token
}

// waitStatus 等这台服务器的状态变成 want。
func (h *harness) waitStatus(name, want string) serverObj {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		o := h.server(name)
		if o.Status.Status == want || time.Now().After(deadline) {
			if o.Status.Status != want {
				h.t.Fatalf("%s 的状态应当变成 %s，现在是 %s", name, want, o.Status.Status)
			}
			return o
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// node-channel「握手与加密信封」：跳过握手直接认证、握手之后发明文、重放、伪造的帧，都被主控断开。
func TestAgentFrameRules(t *testing.T) {
	h := start(t, migratedSQLite(t))
	pub := masterPub(t, h)
	token := h.addServer("hk-1")

	a := mustDial(t, h)
	a.writeText(xrpc.TypeAuth, xrpc.Auth{Token: token, ControlProtocol: xrpc.ControlProtocol})
	if !a.closedByMaster() {
		t.Fatal("跳过握手直接认证应当被断开")
	}
	b := mustDial(t, h)
	b.handshake(pub)
	b.writeText(xrpc.TypeAuth, xrpc.Auth{Token: token, ControlProtocol: xrpc.ControlProtocol})
	if !b.closedByMaster() {
		t.Fatal("握手之后发明文认证应当被断开")
	}
	if s := h.server("hk-1"); s.Status.Status != "pending" {
		t.Fatalf("两次都没有认证成功，应当还是 pending：%s", s.Status.Status)
	}
	big := mustDial(t, h) // 认证之前一帧最多 64 KiB：大帧直接断开，不让没认出身份的连接让主控备下大缓冲
	if err := big.ws.Write(tctx(t), websocket.MessageText, make([]byte, 100<<10)); err == nil && !big.closedByMaster() {
		t.Fatal("认证之前的大帧应当让主控断开")
	}

	c := mustDial(t, h)
	if res := c.authenticate(pub, token, xrpc.ControlProtocol); !res.OK {
		t.Fatalf("认证应当成功：%+v", res)
	}
	env := c.heartbeat(time.Now())
	c.sendRaw(env)
	if !c.closedByMaster() {
		t.Fatal("重放的帧应当让主控断开")
	}

	d := mustDial(t, h)
	d.authenticate(pub, token, xrpc.ControlProtocol)
	boot := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	forged := d.heartbeat(boot)
	binary.BigEndian.PutUint64(forged[1:9], 1_000_000)
	forged[len(forged)-1] ^= 0xFF
	d.sendRaw(forged)
	if !d.closedByMaster() {
		t.Fatal("伪造的帧应当让主控断开")
	}
	if s := h.server("hk-1"); s.Status.BootTime == nil || !s.Status.BootTime.Equal(boot) {
		t.Fatalf("伪造帧之前的心跳应当照常生效：%+v", s.Status.BootTime)
	}
}

// node-channel「节点认证」：token 不对、协议号过新都回认证失败；连续 3 次认证失败后同一 IP 被 429。
func TestAgentAuthFailures(t *testing.T) {
	h := start(t, migratedSQLite(t))
	pub := masterPub(t, h)
	h.addServer("hk-1")

	a := mustDial(t, h)
	if res := a.authenticate(pub, "no-such-token", xrpc.ControlProtocol); res.OK || res.Reason != xrpc.AuthInvalidToken {
		t.Fatalf("token 不对应当认证失败，原因 invalid_token：%+v", res)
	}
	if !a.closedByMaster() {
		t.Fatal("认证失败后应当断开")
	}
	token := h.addServer("hk-2")
	b := mustDial(t, h)
	res := b.authenticate(pub, token, xrpc.ControlProtocol+1)
	if res.OK || res.Reason != xrpc.AuthIncompatible || !strings.Contains(res.Message, "不能先于主控升级") {
		t.Fatalf("协议号过新应当拒绝并说明：%+v", res)
	}
	if s := h.server("hk-2"); s.Status.Status != "pending" {
		t.Fatalf("协议号过新时服务器状态不变：%s", s.Status.Status)
	}
	for i := 0; i < 2; i++ { // 加上第一次，共 3 次 token 不对
		c := mustDial(t, h)
		c.authenticate(pub, "wrong", xrpc.ControlProtocol)
		c.closedByMaster()
	}
	if _, resp, err := dialAgent(t, h); err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("连续 3 次认证失败后应当 429：%v %v", resp, err)
	}
}

// node-channel「心跳与在线状态」「新连接取代旧连接」「请求与回复」：上线、断开离线、心跳停了按阈值离线、心跳间隔跟着设置走、
// 新连接取代旧连接、向在线与不在线的服务器发请求。
func TestAgentOnlineLifecycle(t *testing.T) {
	h := start(t, migratedSQLite(t))
	pub := masterPub(t, h)
	token := h.addServer("hk-1")
	h.addServer("hk-2")

	settings, _, _ := h.settingsCLI("settings", "show")
	if _, e, _ := h.settingsCLI("settings", "set", "--set", "heartbeat_interval=10", "--resource-version", jsonInt(settings.Metadata.ResourceVersion)); e != nil {
		t.Fatalf("settings set：%v", e)
	}
	a := mustDial(t, h)
	res := a.authenticate(pub, token, xrpc.ControlProtocol)
	if !res.OK || res.HeartbeatInterval != 10 {
		t.Fatalf("认证成功的回复里心跳间隔应当是 10：%+v", res)
	}
	s := h.waitStatus("hk-1", "connected")
	if s.Status.LastHeartbeat == nil || time.Since(*s.Status.LastHeartbeat) > time.Minute || s.Status.IPAddress == nil ||
		*s.Status.IPAddress != "127.0.0.1" || s.Metadata.ResourceVersion != 1 {
		t.Fatalf("上线之后的 status 不对：%+v rv=%d", s.Status, s.Metadata.ResourceVersion)
	}

	// 请求：在线的服务器回 agent.info；不认识的方法回 unknown_method；不在线的立即报错。
	aClosed := a.serveCalls()
	id := s.Metadata.ID
	raw, err := h.app.conns.Call(context.Background(), id, xrpc.MethodAgentInfo, nil, 5*time.Second)
	var info xrpc.AgentInfo
	if err != nil || json.Unmarshal(raw, &info) != nil || info.AgentVersion != "0.2.0-test" || info.ControlProtocol != xrpc.ControlProtocol {
		t.Fatalf("agent.info：%s %v", raw, err)
	}
	var rerr *xrpc.Error
	if _, err := h.app.conns.Call(context.Background(), id, "no.such", nil, 5*time.Second); !errors.As(err, &rerr) || rerr.Code != xrpc.CodeUnknownMethod {
		t.Fatalf("不认识的方法应当回 unknown_method：%v", err)
	}
	other := h.server("hk-2").Metadata.ID
	began := time.Now()
	if _, err := h.app.conns.Call(context.Background(), other, xrpc.MethodAgentInfo, nil, 5*time.Second); !errors.Is(err, agentconn.ErrOffline) || time.Since(began) > time.Second {
		t.Fatalf("不在线的服务器应当立即报错：%v", err)
	}

	// 心跳停了：连接还在，按阈值离线（直接跑一次内置任务的逻辑，把「现在」拨到阈值之后）。
	run := h.app.servers.SweepOffline(30*time.Second, func() time.Time { return time.Now().Add(31 * time.Second) })
	if detail, err := run(context.Background()); err != nil || detail != "标为离线 1 台" {
		t.Fatalf("心跳过期应当被标离线：%q %v", detail, err)
	}
	if s := h.server("hk-1"); s.Status.Status != "offline" || s.Status.OfflineSince == nil {
		t.Fatalf("心跳停了之后应当离线：%+v", s.Status)
	}

	// 新连接取代旧连接：旧连接被断开后服务器仍是 connected（旧连接的收尾不能把它标离线），
	// 请求发到新连接，心跳来自新连接。
	b := mustDial(t, h)
	if !b.authenticate(pub, token, xrpc.ControlProtocol).OK {
		t.Fatal("新连接应当认证成功")
	}
	select {
	case <-aClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("旧连接应当被主控断开")
	}
	// 旧连接在服务端的收尾比节点看到断开稍晚，看一段时间，期间一直应当是 connected。
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if s := h.server("hk-1"); s.Status.Status != "connected" {
			t.Fatalf("旧连接断开之后仍应当是 connected：%s", s.Status.Status)
		}
	}
	b.serveCalls()
	if raw, err := h.app.conns.Call(context.Background(), id, xrpc.MethodAgentInfo, nil, 5*time.Second); err != nil {
		t.Fatalf("取代之后的请求应当发到新连接：%s %v", raw, err)
	}
	boot := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	b.send(xrpc.TypeHeartbeat, xrpc.Heartbeat{BootTime: boot}) // 心跳确认由 serveCalls 读走
	// 状态在认证时就已经是 connected，这里等 boot_time 变成新连接发的值，才算这次心跳落地。
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		s := h.server("hk-1")
		if s.Status.Status == "connected" && s.Status.BootTime != nil && s.Status.BootTime.Equal(boot) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("心跳应当来自新连接：%s %+v", s.Status.Status, s.Status.BootTime)
		}
	}

	// 断开立即离线。
	b.ws.Close(websocket.StatusNormalClosure, "bye")
	if s := h.waitStatus("hk-1", "offline"); s.Status.OfflineSince == nil {
		t.Fatalf("断开之后应当记 offline_since：%+v", s.Status)
	}
}

// migratedSQLite 是迁移好的 SQLite 库：通道的用例与库无关（status 的双库写入由 core/servers 的测试覆盖），只跑 SQLite。
// node-channel「坏的密钥文件」：master.key 是 10 个字节时，newApp（serve 在监听之前调它）以错误码 config 拒绝、
// 点名 master.key，文件不被改写。serve 真启动的用例只在 Linux 上跑，这里直接测 newApp。
func TestBadMasterKey(t *testing.T) {
	dataDir := shortTempDir(t)
	if err := db.EnsureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, db.MasterKeyFile)
	bad := []byte("0123456789")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := newApp(dataDir, migratedSQLite(t), slog.New(slog.DiscardHandler), db.ServeConfig{})
	var e *v1.Error
	if !errors.As(err, &e) || e.Code != v1.CodeConfig || !strings.Contains(e.Reason, db.MasterKeyFile) {
		t.Fatalf("坏的 master.key 应当以 config 拒绝并点名它：%v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, bad) {
		t.Fatalf("master.key 不应当被改写：%q", got)
	}
}

// 下发的心跳间隔最多是离线阈值的三分之一：阈值 30 秒、设置 60 秒时认证结果里是 10 秒。
func TestAgentHeartbeatCapped(t *testing.T) {
	h := startWith(t, migratedSQLite(t), db.ServeConfig{OfflineThreshold: 30 * time.Second})
	pub := masterPub(t, h)
	token := h.addServer("hk-1")
	settings, _, _ := h.settingsCLI("settings", "show")
	if _, e, _ := h.settingsCLI("settings", "set", "--set", "heartbeat_interval=60", "--resource-version", jsonInt(settings.Metadata.ResourceVersion)); e != nil {
		t.Fatalf("settings set：%v", e)
	}
	if res := mustDial(t, h).authenticate(pub, token, xrpc.ControlProtocol); !res.OK || res.HeartbeatInterval != 10 {
		t.Fatalf("阈值 30 秒时心跳间隔应当下发成 10 秒：%+v", res)
	}
}

func migratedSQLite(t *testing.T) *bun.DB {
	t.Helper()
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	return bdb
}

func jsonInt(n int64) string { return strconv.FormatInt(n, 10) }
