package agentconn

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/securechan"
	"github.com/satchel/satchel/pkg/xrpc"
)

type extDir struct {
	mu           sync.Mutex
	tokens       map[string]Node
	targets      map[int64]TargetInfo
	sameHosts    map[int64]bool
	clearPending int
	onlineIP     string
	onlineBoot   time.Time
	onlineCount  int
	offlineCount int
}

func newExtDir() *extDir {
	return &extDir{
		tokens:    map[string]Node{"tok": {ID: 1, Name: "hk-1"}, "tok2": {ID: 2, Name: "hk-2"}},
		targets:   map[int64]TargetInfo{},
		sameHosts: map[int64]bool{},
	}
}

func (d *extDir) Lookup(_ context.Context, token string) (Node, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.tokens[token]
	return n, ok, nil
}

func (d *extDir) Online(_ context.Context, id int64, ip string, _ bool, boot, _ time.Time) error {
	if id != 1 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onlineIP = ip
	if !boot.IsZero() {
		d.onlineBoot = boot
	}
	d.onlineCount++
	return nil
}

func (d *extDir) Offline(_ context.Context, _ int64, _ time.Time) error {
	d.mu.Lock()
	d.offlineCount++
	d.mu.Unlock()
	return nil
}

func (d *extDir) SetSameHost(_ context.Context, id int64, same bool) error {
	d.mu.Lock()
	d.sameHosts[id] = same
	d.mu.Unlock()
	return nil
}

func (d *extDir) ClearPending(_ context.Context, id int64) error {
	if id == 1 {
		d.mu.Lock()
		d.clearPending++
		d.mu.Unlock()
	}
	return nil
}

func (d *extDir) Target(_ context.Context, id int64) (TargetInfo, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.targets[id]
	return t, ok, nil
}

func serveHTTPAndWS(t *testing.T, cfg Config) (*Manager, *httptest.Server, ed25519.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg.Identity = priv
	m := New(cfg)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.Header.Get("X-Test-IP")
		switch r.URL.Path {
		case "/api/remote/ws":
			if m.Blocked(ip) {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			m.Serve(r.Context(), ws, ip)
		case xrpc.HTTPMasterPathHandshake:
			m.ServeHandshake(w, r, ip)
		case xrpc.HTTPMasterPathRPC:
			m.ServeRPC(w, r, ip)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return m, srv, pub
}

func httpHandshakeClient(t *testing.T, baseURL, ip string, pub ed25519.PublicKey) (string, *securechan.Session) {
	t.Helper()
	ephPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ephPub := ephPriv.PublicKey().Bytes()
	raw, _ := json.Marshal(xrpc.HTTPHandshakeReq{AgentEphemeral: ephPub})
	req, _ := http.NewRequest(http.MethodPost, baseURL+xrpc.HTTPMasterPathHandshake, bytes.NewReader(raw))
	req.Header.Set("X-Test-IP", ip)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("握手失败 %d: %s", resp.StatusCode, b)
	}
	var hs xrpc.HTTPHandshakeResp
	if err := json.NewDecoder(resp.Body).Decode(&hs); err != nil {
		t.Fatal(err)
	}
	if !securechan.VerifyHandshake(pub, ephPub, hs.MasterEphemeral, hs.Signature) {
		t.Fatal("主控握手签名验证失败")
	}
	masterEph, err := ecdh.X25519().NewPublicKey(hs.MasterEphemeral)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ephPriv.ECDH(masterEph)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := securechan.DeriveSession(shared, ephPub, hs.MasterEphemeral, false)
	if err != nil {
		t.Fatal(err)
	}
	return hs.SessionID, sess
}

func httpCallRPC(t *testing.T, baseURL, ip, sid string, sess *securechan.Session, rpcReq xrpc.AgentHTTPRPC) (int, xrpc.AgentHTTPRPCReply) {
	t.Helper()
	plain, _ := json.Marshal(rpcReq)
	env, err := sess.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, baseURL+xrpc.HTTPMasterPathRPC, bytes.NewReader(env))
	req.Header.Set("X-Test-IP", ip)
	req.Header.Set(securechan.HeaderSession, sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, xrpc.AgentHTTPRPCReply{}
	}
	dec, err := sess.Decrypt(body)
	if err != nil {
		t.Fatalf("解包加密响应失败：%v", err)
	}
	var reply xrpc.AgentHTTPRPCReply
	if err := json.Unmarshal(dec, &reply); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, reply
}

// 30 秒双凭据窗口：窗口内旧 token 与新 token 都能通过；仅当使用当前新 token 时清除待确认标志；确认或超时后旧 token 失效。
func TestDualTokenWindowAndPendingClear(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	dir := newExtDir()
	m, srv, pub := serveHTTPAndWS(t, Config{Directory: dir, RotateWindow: 30 * time.Second, Now: clock})

	// 切换库里的当前凭据到 tok-new，并在内存开启 tok-old 过渡窗口。
	dir.mu.Lock()
	delete(dir.tokens, "tok")
	dir.tokens["tok-new"] = Node{ID: 1, Name: "hk-1"}
	dir.mu.Unlock()
	m.BeginTokenWindow(1, Node{ID: 1, Name: "hk-1"}, "tok")

	// 1. 窗口内拿旧 token 走 HTTP 认证可通过，但不能误清 pending 标志。
	sid1, sess1 := httpHandshakeClient(t, srv.URL, "198.51.100.1", pub)
	code, reply := httpCallRPC(t, srv.URL, "198.51.100.1", sid1, sess1, xrpc.AgentHTTPRPC{
		Method: xrpc.HTTPRPCMethodHeartbeat, Token: "tok", ControlProtocol: xrpc.ControlProtocol,
		Heartbeat: &xrpc.HTTPHeartbeatPayload{BootTime: now},
	})
	if code != http.StatusOK || !reply.OK {
		t.Fatalf("双窗期旧 token 应当认证成功：%d %+v", code, reply)
	}
	dir.mu.Lock()
	cleared := dir.clearPending
	dir.mu.Unlock()
	if cleared != 0 {
		t.Fatalf("旧 token 不应当清除 rotation_pending：%d", cleared)
	}

	// 2. 窗口确认关闭后，旧 token 立即失效；确认新 token 登录则会清 pending。
	m.ConfirmTokenWindow(1)
	code, reply = httpCallRPC(t, srv.URL, "198.51.100.1", sid1, sess1, xrpc.AgentHTTPRPC{
		Method: xrpc.HTTPRPCMethodHeartbeat, Token: "tok", ControlProtocol: xrpc.ControlProtocol,
	})
	if code != http.StatusOK || reply.OK || reply.Reason != xrpc.AuthInvalidToken {
		t.Fatalf("关闭窗口后旧 token 应当被拒：%d %+v", code, reply)
	}
	sid2, sess2 := httpHandshakeClient(t, srv.URL, "198.51.100.1", pub)
	code, reply = httpCallRPC(t, srv.URL, "198.51.100.1", sid2, sess2, xrpc.AgentHTTPRPC{
		Method: xrpc.HTTPRPCMethodHeartbeat, Token: "tok-new", ControlProtocol: xrpc.ControlProtocol,
	})
	if code != http.StatusOK || !reply.OK {
		t.Fatalf("新 token 应当认证成功：%d %+v", code, reply)
	}
	dir.mu.Lock()
	cleared = dir.clearPending
	dir.mu.Unlock()
	if cleared != 1 {
		t.Fatalf("当前新 token 应当触发清除 pending 1 次：%d", cleared)
	}

	// 3. 超时自动淘汰测试。
	m.BeginTokenWindow(1, Node{ID: 1, Name: "hk-1"}, "tok-exp")
	mu.Lock()
	now = now.Add(31 * time.Second)
	mu.Unlock()
	sid3, sess3 := httpHandshakeClient(t, srv.URL, "198.51.100.1", pub)
	_, reply = httpCallRPC(t, srv.URL, "198.51.100.1", sid3, sess3, xrpc.AgentHTTPRPC{
		Method: xrpc.HTTPRPCMethodHeartbeat, Token: "tok-exp", ControlProtocol: xrpc.ControlProtocol,
	})
	if reply.OK || reply.Reason != xrpc.AuthInvalidToken {
		t.Fatalf("窗口超时后旧 token 应当失效：%+v", reply)
	}
}

// 吊销清理：立即断开活跃 WS、清空上行 HTTP SessionCache，随后拿原会话发 HTTP RPC 立马回 412。
func TestInvalidateServerDropsWSAndPurgesHTTP(t *testing.T) {
	dir := newExtDir()
	m, srv, pub := serveHTTPAndWS(t, Config{Directory: dir})
	wsURL := "ws" + srv.URL[len("http"):] + "/api/remote/ws"
	wsAgent := mustConnect(t, wsURL, "198.51.100.2", pub)

	sid, sess := httpHandshakeClient(t, srv.URL, "198.51.100.2", pub)
	code, reply := httpCallRPC(t, srv.URL, "198.51.100.2", sid, sess, xrpc.AgentHTTPRPC{
		Method: xrpc.HTTPRPCMethodHeartbeat, Token: "tok", ControlProtocol: xrpc.ControlProtocol,
	})
	if code != http.StatusOK || !reply.OK {
		t.Fatalf("HTTP 心跳应当成功：%d %+v", code, reply)
	}

	m.InvalidateServer(1, true, roleManage, rolePull)
	if m.Connected(1) {
		t.Fatal("InvalidateServer 后 Connected(1) 应当立即为假")
	}
	rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
	defer rcancel()
	if _, err := wsAgent.conn.Read(rctx); err == nil {
		t.Fatal("活跃 WS 连接应当立即被主控断开")
	}
	code, _ = httpCallRPC(t, srv.URL, "198.51.100.2", sid, sess, xrpc.AgentHTTPRPC{
		Method: xrpc.HTTPRPCMethodHeartbeat, Token: "tok", ControlProtocol: xrpc.ControlProtocol,
	})
	if code != http.StatusPreconditionFailed {
		t.Fatalf("清空会话缓存后应返回 412，得到 %d", code)
	}
}

// 同 token 异 IP 冲突锁定 60 秒与冷却后切换恢复（覆盖 WS 与 HTTP）。
func TestIPConflictLockAndCooldownRecovery(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	dir := newExtDir()
	m, srv, pub := serveHTTPAndWS(t, Config{
		Directory:        dir,
		ConflictWindow:   60 * time.Second,
		ConflictCooldown: 60 * time.Second,
		Now:              clock,
	})
	wsURL := "ws" + srv.URL[len("http"):] + "/api/remote/ws"

	// 1. 先到者 IP A (198.51.100.10) 连上。
	first := mustConnect(t, wsURL, "198.51.100.10", pub)

	// 2. 在 60s 内不同 IP B (203.0.113.20) 尝试 WS 与 HTTP 接入均收到 ip_conflict，先到者不受影响。
	_, resB, err := connect(t, wsURL, "203.0.113.20", pub)
	if err != nil || resB.OK || resB.Reason != xrpc.AuthIPConflict {
		t.Fatalf("异 IP WS 应当返回 ip_conflict：%+v %v", resB, err)
	}
	sidB, sessB := httpHandshakeClient(t, srv.URL, "203.0.113.20", pub)
	code, repB := httpCallRPC(t, srv.URL, "203.0.113.20", sidB, sessB, xrpc.AgentHTTPRPC{
		Method: xrpc.HTTPRPCMethodHeartbeat, Token: "tok", ControlProtocol: xrpc.ControlProtocol,
	})
	if code != http.StatusOK || repB.OK || repB.Reason != xrpc.AuthIPConflict {
		t.Fatalf("异 IP HTTP 应当返回 ip_conflict：%d %+v", code, repB)
	}
	if !m.Connected(1) {
		t.Fatal("冲突拒绝期间先到者必须保持在线")
	}

	// 3. 在冷却期内（65s > lastActive+60s 但 < rejectUntil=60+1s？等 65s-0s >=60s 但 rejectUntil=60s，先加到 50s 测试仍受保护），
	//    同 IP A 重连放行，异 IP 仍拒绝。
	mu.Lock()
	now = now.Add(50 * time.Second)
	mu.Unlock()
	_ = first.ws.CloseNow()
	reA := mustConnect(t, wsURL, "198.51.100.10", pub)
	_ = reA
	// 50s 时 A 刚重连更新 lastActive=50s；B 在 55s 再试将把 rejectUntil 推到 115s。
	mu.Lock()
	now = now.Add(5 * time.Second) // t=55s
	mu.Unlock()
	_, resB, _ = connect(t, wsURL, "203.0.113.20", pub)
	if resB.OK || resB.Reason != xrpc.AuthIPConflict {
		t.Fatalf("冷却内 B 仍应被拒：%+v", resB)
	}

	// 4. 拨钟让 A 失活超过 60s（>50+60=110s）且最新冷却期结束（>55+60=115s）：t=120s 时 B 成功接管。
	mu.Lock()
	now = now.Add(65 * time.Second) // t=120s
	mu.Unlock()
	_ = mustConnect(t, wsURL, "203.0.113.20", pub)
}

// HTTP 上行入口安全边界：拒带 Authorization、无/坏会话一律回 412、带会话发 JSON 明文回 400、跨服复用回 403、猜错 3 次 429、Traffic 回调入库。
func TestAgentToMasterHTTPSecurityAndTraffic(t *testing.T) {
	dir := newExtDir()
	var gotTraffic atomic.Int64
	_, srv, pub := serveHTTPAndWS(t, Config{
		Directory: dir,
		OnTraffic: func(_ context.Context, serverID int64, snap xrpc.TrafficSnapshot) error {
			if serverID == 1 && len(snap.Counts) == 1 {
				gotTraffic.Add(snap.Counts[0].Up)
			}
			return nil
		},
	})
	ip := "198.51.100.30"

	// 1. 携带 Authorization 头直接拒绝。
	for _, p := range []string{xrpc.HTTPMasterPathHandshake, xrpc.HTTPMasterPathRPC} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+p, bytes.NewReader([]byte(`{}`)))
		req.Header.Set("X-Test-IP", ip)
		req.Header.Set("Authorization", "Bearer tok")
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s 携带 Authorization 头应当回 400，得到 %d", p, resp.StatusCode)
		}
	}

	// 2. 缺 X-Satchel-Session 或伪造会话 ID 一律 412。
	for _, hdr := range []string{"", "non-existent-session"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+xrpc.HTTPMasterPathRPC, bytes.NewReader([]byte(`{"method":"heartbeat","token":"tok"}`)))
		req.Header.Set("X-Test-IP", ip)
		if hdr != "" {
			req.Header.Set(securechan.HeaderSession, hdr)
		}
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("sid=%q 应当回 412，得到 %d", hdr, resp.StatusCode)
		}
	}

	// 3. 有有效会话却发未经加密的明文 JSON -> 400 且业务不执行。
	sid, sess := httpHandshakeClient(t, srv.URL, ip, pub)
	plainReq, _ := http.NewRequest(http.MethodPost, srv.URL+xrpc.HTTPMasterPathRPC, bytes.NewReader([]byte(`{"method":"heartbeat","token":"tok","control_protocol":1}`)))
	plainReq.Header.Set("X-Test-IP", ip)
	plainReq.Header.Set(securechan.HeaderSession, sid)
	resp, _ := http.DefaultClient.Do(plainReq)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("明文载荷应当回 400，得到 %d", resp.StatusCode)
	}
	dir.mu.Lock()
	onlines := dir.onlineCount
	dir.mu.Unlock()
	if onlines != 0 {
		t.Fatalf("明文请求绝不应触发上线：%d", onlines)
	}

	// 4. 正常加密 traffic 上报与跨服会话绑定拒绝。
	code, rep := httpCallRPC(t, srv.URL, ip, sid, sess, xrpc.AgentHTTPRPC{
		Method:          xrpc.HTTPRPCMethodTraffic,
		Token:           "tok",
		ControlProtocol: xrpc.ControlProtocol,
		Traffic: &xrpc.TrafficSnapshot{
			BootID: "b1", Generation: 1, At: time.Now(),
			Counts: []xrpc.TrafficCount{{Inbound: "in", Outbound: "direct", Credential: "a", Up: 12345}},
		},
	})
	if code != http.StatusOK || !rep.OK || gotTraffic.Load() != 12345 {
		t.Fatalf("加密 traffic 上报失败：code=%d rep=%+v traffic=%d", code, rep, gotTraffic.Load())
	}
	// 同一 session 携带 tok2 试图跨服复用 -> 403 且会话被删，随后再发回 412。
	code, _ = httpCallRPC(t, srv.URL, ip, sid, sess, xrpc.AgentHTTPRPC{
		Method: xrpc.HTTPRPCMethodHeartbeat, Token: "tok2", ControlProtocol: xrpc.ControlProtocol,
	})
	if code != http.StatusForbidden {
		t.Fatalf("跨服复用会话应当回 403，得到 %d", code)
	}
	code, _ = httpCallRPC(t, srv.URL, ip, sid, sess, xrpc.AgentHTTPRPC{
		Method: xrpc.HTTPRPCMethodHeartbeat, Token: "tok", ControlProtocol: xrpc.ControlProtocol,
	})
	if code != http.StatusPreconditionFailed {
		t.Fatalf("跨服被删的会话应返回 412，得到 %d", code)
	}

	// 5. 连续 3 次 invalid_token 后同一 IP 被 429 封锁。
	sidBad, sessBad := httpHandshakeClient(t, srv.URL, ip, pub)
	for i := 0; i < 3; i++ {
		code, rep := httpCallRPC(t, srv.URL, ip, sidBad, sessBad, xrpc.AgentHTTPRPC{
			Method: xrpc.HTTPRPCMethodHeartbeat, Token: "wrong-tok", ControlProtocol: xrpc.ControlProtocol,
		})
		if code != http.StatusOK || rep.OK || rep.Reason != xrpc.AuthInvalidToken {
			t.Fatalf("第 %d 次错误 token 回复不对：%d %+v", i+1, code, rep)
		}
	}
	req429, _ := http.NewRequest(http.MethodPost, srv.URL+xrpc.HTTPMasterPathHandshake, bytes.NewReader([]byte(`{}`)))
	req429.Header.Set("X-Test-IP", ip)
	resp429, _ := http.DefaultClient.Do(req429)
	resp429.Body.Close()
	if resp429.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("连续 3 次猜错后应当 429，得到 %d", resp429.StatusCode)
	}
}

// fakeNodeEndpoint 模拟节点侧管理口 / 轮询口：验证 SilentAuth、VerifyHandshake、412 自动重握手以及请求方法分派。
type fakeNodeEndpoint struct {
	token      string
	masterPub  ed25519.PublicKey
	force412   atomic.Bool
	hangRPC    atomic.Bool
	nonces     *securechan.NonceWindow
	mu         sync.Mutex
	handshakes map[string]*ecdh.PrivateKey
	sessions   *securechan.SessionCache
	calls      []string
	masterURLs []string
}

func newFakeNodeEndpoint(t *testing.T, token string, masterPub ed25519.PublicKey) (*fakeNodeEndpoint, int64) {
	t.Helper()
	quit := make(chan struct{})
	n := &fakeNodeEndpoint{
		token:      token,
		masterPub:  masterPub,
		nonces:     securechan.NewNonceWindow(time.Minute, 100, nil),
		handshakes: make(map[string]*ecdh.PrivateKey),
		sessions:   securechan.NewSessionCache(securechan.SessionCacheConfig{TTL: time.Hour}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := r.Header.Get(securechan.HeaderNonce)
		sig := r.Header.Get(securechan.HeaderAuth)
		if !n.nonces.Allow(nonce) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n.mu.Lock()
		tok := n.token
		n.mu.Unlock()
		if _, ok := securechan.VerifySilentAuth([]string{tok}, r.Method, r.URL.Path, nonce, sig); !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case xrpc.HTTPNodePathHandshakeInit:
			priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
			hid, _ := securechan.GenerateSessionID()
			n.mu.Lock()
			n.handshakes[hid] = priv
			n.mu.Unlock()
			_ = json.NewEncoder(w).Encode(xrpc.NodeHandshakeInitResp{
				HandshakeID:    hid,
				AgentEphemeral: priv.PublicKey().Bytes(),
			})
		case xrpc.HTTPNodePathHandshakeFinish:
			var req xrpc.NodeHandshakeFinishReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n.mu.Lock()
			priv := n.handshakes[req.HandshakeID]
			delete(n.handshakes, req.HandshakeID)
			n.mu.Unlock()
			if priv == nil {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			agentPub := priv.PublicKey().Bytes()
			if !securechan.VerifyHandshake(n.masterPub, agentPub, req.MasterEphemeral, req.Signature) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			mPub, err := ecdh.X25519().NewPublicKey(req.MasterEphemeral)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			shared, _ := priv.ECDH(mPub)
			sess, _ := securechan.DeriveSession(shared, agentPub, req.MasterEphemeral, false)
			sid, _ := securechan.GenerateSessionID()
			_ = n.sessions.Put(sid, sess, 1, "node")
			_ = json.NewEncoder(w).Encode(xrpc.NodeHandshakeFinishResp{SessionID: sid})
		case xrpc.HTTPNodePathRPC:
			if n.force412.Swap(false) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			sid := r.Header.Get(securechan.HeaderSession)
			sess, _, _, ok := n.sessions.Get(sid)
			if !ok {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			body, _ := io.ReadAll(r.Body)
			if n.hangRPC.Load() {
				select {
				case <-r.Context().Done():
				case <-quit:
				}
				return
			}
			plain, err := sess.Decrypt(body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var call xrpc.Call
			_ = json.Unmarshal(plain, &call)
			n.mu.Lock()
			n.calls = append(n.calls, call.Method)
			n.mu.Unlock()
			var reply xrpc.Reply
			reply.ID = call.ID
			switch call.Method {
			case xrpc.MethodAgentInfo:
				reply.Result, _ = json.Marshal(xrpc.AgentInfo{AgentVersion: "0.2.0", ControlProtocol: 1})
			case xrpc.MethodTrafficSnapshot:
				reply.Result, _ = json.Marshal(xrpc.TrafficSnapshot{BootID: "pull-1", Generation: 2, KernelRunning: true})
			case xrpc.MethodTokenRotate:
				var params xrpc.TokenRotateParams
				if err := json.Unmarshal(call.Params, &params); err != nil {
					reply.Error = &xrpc.Error{Code: xrpc.CodeBadParams, Message: err.Error()}
				} else {
					if params.AgentToken != "" {
						n.mu.Lock()
						n.token = params.AgentToken
						n.mu.Unlock()
					}
					reply.Result, _ = json.Marshal(xrpc.TokenRotateAck{UpdatedAt: time.Now().UTC()})
				}
			case xrpc.MethodAgentUpdateMaster:
				var params xrpc.UpdateMasterParams
				if err := json.Unmarshal(call.Params, &params); err != nil {
					reply.Error = &xrpc.Error{Code: xrpc.CodeBadParams, Message: err.Error()}
				} else {
					n.mu.Lock()
					n.masterURLs = append(n.masterURLs, params.MasterURL)
					n.mu.Unlock()
					reply.Result, _ = json.Marshal(xrpc.UpdateMasterAck{MasterURL: params.MasterURL, UpdatedAt: time.Now().UTC()})
				}
			default:
				reply.Error = &xrpc.Error{Code: xrpc.CodeUnknownMethod, Message: "unknown"}
			}
			rbytes, _ := json.Marshal(reply)
			env, _ := sess.Encrypt(rbytes)
			_, _ = w.Write(env)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		close(quit)
		srv.CloseClientConnections()
	})
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.ParseInt(portStr, 10, 64)
	return n, port
}

// Master → Agent 加密 HTTP 管理口与只读轮询口调用、412 自动重握手一次、候选寻址回落及不可达 vs 超时区分。
func TestMasterToAgentEncryptedHTTPClient(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dir := newExtDir()
	// 取空闲端口用于模拟不可达的 Domain 首位候选。
	deadLn, _ := net.Listen("tcp", "127.0.0.1:0")
	deadPort := deadLn.Addr().String()
	deadLn.Close()

	manageNode, managePort := newFakeNodeEndpoint(t, "agent-tok-1", pub)
	pullNode, pullPort := newFakeNodeEndpoint(t, "pull-tok-1", pub)

	dir.mu.Lock()
	dir.targets[1] = TargetInfo{
		ID:             1,
		Name:           "hk-http",
		ConnectionMode: "http",
		Domain:         deadPort,    // 首位故意不可达，验证顺序回 fallback 到 IPAddress
		IPAddress:      "127.0.0.1", // 次位可达
		ListenPort:     managePort,
		PullAddress:    "127.0.0.1",
		PullPort:       pullPort,
		AgentToken:     "agent-tok-1",
		PullToken:      "pull-tok-1",
	}
	dir.targets[2] = TargetInfo{
		ID:             2,
		Name:           "hk-ws",
		ConnectionMode: "websocket",
		IPAddress:      "127.0.0.1",
		ListenPort:     managePort,
		AgentToken:     "agent-tok-1",
	}
	dir.mu.Unlock()

	m := New(Config{Identity: priv, Directory: dir, HTTPDialTimeout: 500 * time.Millisecond})

	// 1. 无活跃 WS 时经管理口调用成功（并自动跳过死域名回退到 127.0.0.1）。
	raw, err := m.Call(context.Background(), 1, xrpc.MethodAgentInfo, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("管理口 Call 应当成功：%v", err)
	}
	var info xrpc.AgentInfo
	if err := json.Unmarshal(raw, &info); err != nil || info.AgentVersion != "0.2.0" {
		t.Fatalf("agent.info 回复不对：%s %v", raw, err)
	}

	// 2. 强制节点下一包回 412：主控应清缓存自动重握手一次并完成 RPC。
	manageNode.force412.Store(true)
	if _, err := m.Call(context.Background(), 1, xrpc.MethodAgentInfo, nil, 2*time.Second); err != nil {
		t.Fatalf("412 后应当自动重握手并成功：%v", err)
	}

	// 3. 节点 /rpc 挂起超时应返回 xrpc.ErrTimeout，而不混同 ErrOffline。
	manageNode.hangRPC.Store(true)
	_, err = m.Call(context.Background(), 1, xrpc.MethodAgentInfo, nil, 150*time.Millisecond)
	if !errors.Is(err, xrpc.ErrTimeout) || errors.Is(err, ErrOffline) {
		t.Fatalf("/rpc 超时应当返回 ErrTimeout 且不是 ErrOffline：%v", err)
	}
	manageNode.hangRPC.Store(false)

	// 4. websocket 模式即使写了 IP/端口也不能走管理口回退。
	if _, err := m.Call(context.Background(), 2, xrpc.MethodAgentInfo, nil, time.Second); !errors.Is(err, ErrOffline) {
		t.Fatalf("websocket 模式无活跃 WS 时应直接返 ErrOffline：%v", err)
	}

	// 5. PullTraffic 经只读轮询口采集快照并返还命中的地址。
	snap, host, v6, err := m.PullTraffic(context.Background(), 1, 2*time.Second)
	if err != nil || snap.BootID != "pull-1" || host != "127.0.0.1" || v6 {
		t.Fatalf("PullTraffic 结果不对：snap=%+v host=%q v6=%v err=%v", snap, host, v6, err)
	}
	if len(pullNode.calls) != 1 || pullNode.calls[0] != xrpc.MethodTrafficSnapshot {
		t.Fatalf("pull 口应当收到一次 traffic.snapshot：%v", pullNode.calls)
	}

	// 6. 全端口不可达时返回 ErrOffline。
	dir.mu.Lock()
	dir.targets[3] = TargetInfo{ID: 3, Name: "hk-down", ConnectionMode: "http", IPAddress: deadPort, ListenPort: 1, AgentToken: "x"}
	dir.mu.Unlock()
	if _, err := m.Call(context.Background(), 3, xrpc.MethodAgentInfo, nil, time.Second); !errors.Is(err, ErrOffline) {
		t.Fatalf("端口不可达应当返回 ErrOffline：%v", err)
	}
	_ = v1.CodeUnavailable
}

// TestRotateAgentTokenOverHTTPManagementPort 验证无活跃 WS 时 DB 已提前写入新 agent_token 后：
// token.rotate 通过 BeginAgentTokenWindow 优先使用旧 agent_token 签 SilentAuth 推送成功，
// ConfirmAgentTokenWindow 后后续重握手改用新 agent_token；窗口期内普通 RPC 仍能新优先、旧兜底。
func TestRotateAgentTokenOverHTTPManagementPort(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dir := newExtDir()
	manageNode, managePort := newFakeNodeEndpoint(t, "old-agent-tok", pub)

	dir.mu.Lock()
	dir.targets[1] = TargetInfo{
		ID:             1,
		Name:           "hk-http-rot",
		ConnectionMode: "http",
		IPAddress:      "127.0.0.1",
		ListenPort:     managePort,
		AgentToken:     "new-agent-tok",
	}
	dir.mu.Unlock()

	m := New(Config{Identity: priv, Directory: dir, HTTPDialTimeout: 500 * time.Millisecond})

	// 1. 开启旧 agent_token 过渡窗口：token.rotate 应优先用 old-agent-tok 鉴权并完成原子切换。
	m.BeginAgentTokenWindow(1, "old-agent-tok")
	if _, err := m.Call(context.Background(), 1, xrpc.MethodTokenRotate, xrpc.TokenRotateParams{AgentToken: "new-agent-tok"}, 2*time.Second); err != nil {
		t.Fatalf("http 管理口推送 token.rotate 应当成功：%v", err)
	}
	manageNode.mu.Lock()
	gotTok := manageNode.token
	manageNode.mu.Unlock()
	if gotTok != "new-agent-tok" {
		t.Fatalf("节点应当切换为新 agent_token，实际 %q", gotTok)
	}

	// 2. 确认关闭旧窗口并强制 412 重握手，验证完全使用新 agent_token 建立新会话。
	m.ConfirmAgentTokenWindow(1)
	manageNode.force412.Store(true)
	if _, err := m.Call(context.Background(), 1, xrpc.MethodAgentInfo, nil, 2*time.Second); err != nil {
		t.Fatalf("确认窗口后重握手使用新 agent_token 应当成功：%v", err)
	}

	// 3. 过渡窗口尚未关闭时（如等待确认或回滚前），普通 MethodAgentInfo 仍可通过旧 token 兜底。
	dir.mu.Lock()
	dir.targets[1] = TargetInfo{
		ID:             1,
		Name:           "hk-http-rot",
		ConnectionMode: "http",
		IPAddress:      "127.0.0.1",
		ListenPort:     managePort,
		AgentToken:     "not-applied-yet",
	}
	dir.mu.Unlock()
	manageNode.force412.Store(true)
	m.BeginAgentTokenWindow(1, "new-agent-tok")
	if _, err := m.Call(context.Background(), 1, xrpc.MethodAgentInfo, nil, 2*time.Second); err != nil {
		t.Fatalf("过渡窗口内普通 RPC 回落旧 agent_token 应当成功：%v", err)
	}
}

func TestRuntimeMetadataSameHostAndMasterURLCatchUp(t *testing.T) {
	// 1. HostMachineID 顺序与兜底。
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "missing-id")
	fallback := filepath.Join(tmp, "dbus-id")
	if err := os.WriteFile(fallback, []byte("  mid-host-42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := HostMachineID(missing, fallback); got != "mid-host-42" {
		t.Fatalf("HostMachineID 兜底读取失败：got %q", got)
	}
	if got := HostMachineID(missing); got != "" {
		t.Fatalf("不存在文件应当返回空串：got %q", got)
	}

	dir := newExtDir()
	var currentMaster atomic.Value
	currentMaster.Store("https://new.example.com")

	m, srv, pub := serveHTTPAndWS(t, Config{
		Directory:        dir,
		MasterMachineID:  "mid-host-42",
		CurrentMasterURL: func(context.Context) string { return currentMaster.Load().(string) },
		HTTPDialTimeout:  500 * time.Millisecond,
	})
	wsURL := "ws" + srv.URL[len("http"):] + "/api/remote/ws"

	// 2. 远程 IP + 不同 MachineID -> same_host=false；阻断期间不上推 master_url。
	m.SetMasterSyncBlocked(1, true)
	agentA, resA, err := connectWithAuth(t, wsURL, "203.0.113.10", pub, xrpc.Auth{
		Token:           "tok",
		AgentVersion:    "0.2.0",
		ControlProtocol: xrpc.ControlProtocol,
		InDocker:        true,
		MachineID:       "mid-remote-99",
		MasterURL:       "https://old.example.com",
	})
	if err != nil || !resA.OK {
		t.Fatalf("agentA 认证应当成功：%v %+v", err, resA)
	}
	dir.mu.Lock()
	same1 := dir.sameHosts[1]
	dir.mu.Unlock()
	if same1 {
		t.Fatal("远程 IP 且 MachineID 不同时 same_host_as_master 不得为 true")
	}
	rt1, ok := m.Runtime(1)
	if !ok || rt1.AgentVersion != "0.2.0" || !rt1.InDocker || rt1.MachineID != "mid-remote-99" || rt1.MasterURL != "https://old.example.com" {
		t.Fatalf("Runtime(1) 缓存不对：%v %+v", ok, rt1)
	}

	// 3. 远程 IP 但 MachineID 与主控一致 -> same_host=true，且解除阻断发心跳时自动补推 agent.update_master_url。
	m.SetMasterSyncBlocked(1, false)
	pushedCh := make(chan string, 1)
	go func() {
		for {
			msg, err := agentA.conn.Read(context.Background())
			if err != nil {
				return
			}
			switch msg.Type {
			case xrpc.TypeCall:
				var call xrpc.Call
				_ = xrpc.Decode(msg, &call)
				if call.Method == xrpc.MethodAgentUpdateMaster {
					var p xrpc.UpdateMasterParams
					_ = json.Unmarshal(call.Params, &p)
					select {
					case pushedCh <- p.MasterURL:
					default:
					}
					res, _ := json.Marshal(xrpc.UpdateMasterAck{MasterURL: p.MasterURL, UpdatedAt: time.Now().UTC()})
					_ = agentA.conn.Write(context.Background(), xrpc.TypeReply, xrpc.Reply{ID: call.ID, Result: res})
				}
			}
		}
	}()
	if err := agentA.conn.Write(context.Background(), xrpc.TypeHeartbeat, xrpc.Heartbeat{
		BootTime:     time.Now().UTC(),
		AgentVersion: "0.2.1",
		MachineID:    "mid-host-42",
		MasterURL:    "https://old.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pushedCh:
		if got != "https://new.example.com" {
			t.Fatalf("WS 补推的 master_url 不对：%q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待 WS 重连自动补推 agent.update_master_url 超时")
	}
	waitFor(t, "same_host=true", func() bool {
		dir.mu.Lock()
		defer dir.mu.Unlock()
		return dir.sameHosts[1]
	})

	// 4. HTTP 心跳从回环地址上行 -> same_host=true 且对旧地址自动经管理口补推 master_url。
	manageNode, managePort := newFakeNodeEndpoint(t, "agent-tok-2", pub)
	dir.mu.Lock()
	dir.targets[2] = TargetInfo{ID: 2, Name: "hk-2", ConnectionMode: "http", IPAddress: "127.0.0.1", ListenPort: managePort, AgentToken: "agent-tok-2"}
	dir.mu.Unlock()
	sid2, sess2 := httpHandshakeClient(t, srv.URL, "127.0.0.1", pub)
	code, rep := httpCallRPC(t, srv.URL, "127.0.0.1", sid2, sess2, xrpc.AgentHTTPRPC{
		Method:          xrpc.HTTPRPCMethodHeartbeat,
		Token:           "tok2",
		AgentVersion:    "0.2.0",
		ControlProtocol: xrpc.ControlProtocol,
		MachineID:       "mid-different",
		MasterURL:       "https://old.example.com",
		Heartbeat:       &xrpc.HTTPHeartbeatPayload{BootTime: time.Now().UTC()},
	})
	if code != http.StatusOK || !rep.OK {
		t.Fatalf("HTTP 心跳应当成功：%d %+v", code, rep)
	}
	waitFor(t, "http catch-up master_url", func() bool {
		manageNode.mu.Lock()
		defer manageNode.mu.Unlock()
		return len(manageNode.masterURLs) == 1 && manageNode.masterURLs[0] == "https://new.example.com"
	})
	dir.mu.Lock()
	same2 := dir.sameHosts[2]
	dir.mu.Unlock()
	if !same2 {
		t.Fatal("回环地址上行时 same_host_as_master 应当为 true")
	}

	// 5. Disconnect 断连并标为离线。
	if err := m.Disconnect(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if m.Connected(1) {
		t.Fatal("Disconnect 后不应当仍显示在线")
	}
	dir.mu.Lock()
	offCount := dir.offlineCount
	dir.mu.Unlock()
	if offCount == 0 {
		t.Fatal("Disconnect 应当调用 Directory.Offline")
	}
}
