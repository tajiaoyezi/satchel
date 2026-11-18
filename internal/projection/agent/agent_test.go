package agent

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/pkg/release"
	"github.com/satchel/satchel/pkg/securechan"
	"github.com/satchel/satchel/pkg/xrpc"
)

func render(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(path, Script(pub, release.PublicKeys), 0o700); err != nil {
		t.Fatal(err)
	}
	return pub, path
}

// node-pairing「主控提供的安装脚本」：脚本里写进了主控公钥指纹与发布公钥清单，没有剩下的占位符，sh -n 通过。
func TestScriptRendering(t *testing.T) {
	pub, path := render(t)
	data, _ := os.ReadFile(path)
	script := string(data)
	if strings.Contains(script, "@@") {
		t.Fatal("还有没替换的占位符")
	}
	if !strings.Contains(script, `MASTER_KEY_FINGERPRINT="`+securechan.Fingerprint(pub)+`"`) {
		t.Fatal("脚本里应当写进主控公钥的指纹")
	}
	for _, k := range release.PublicKeys {
		if !strings.Contains(script, "\n"+k+"\n") {
			t.Fatalf("脚本里应当有发布公钥 %s", k)
		}
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("sh -n 不通过：%v %s", err, out)
	}
}

// 用 sh 真跑一遍：缺参数就停；公钥指纹对不上时在检查 root 与下载之前就停，退出码非零、说明指纹不一致；
// 公钥对的时候过了指纹这一步（非 root 跑时接着停在 root 检查上）。
func TestScriptChecksFingerprint(t *testing.T) {
	for _, tool := range []string{"sh", "base64", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("没有 %s", tool)
		}
	}
	pub, path := render(t)
	run := func(args ...string) (string, error) {
		out, err := exec.Command("sh", append([]string{path}, args...)...).CombinedOutput()
		return string(out), err
	}
	if out, err := run("--master-url", "https://p.example.com", "--token", "t", "--mode", "websocket"); err == nil || !strings.Contains(out, "缺少 --master-key") {
		t.Fatalf("缺参数应当停下：%v %s", err, out)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	out, err := run("--master-url", "https://p.example.com", "--token", "t", "--master-key", securechan.EncodePublicKey(other), "--mode", "websocket")
	if err == nil || !strings.Contains(out, "主控公钥指纹不一致") {
		t.Fatalf("别的公钥应当在指纹这一步停下：%v %s", err, out)
	}
	if os.Geteuid() == 0 {
		return // root 跑时过了指纹就会接着装依赖、下载：不在测试里做
	}
	out, _ = run("--master-url", "https://p.example.com", "--token", "t", "--master-key", securechan.EncodePublicKey(pub), "--mode", "websocket")
	if !strings.Contains(out, "主控公钥指纹核对通过") {
		t.Fatalf("对的公钥应当过指纹这一步：%s", out)
	}
	out, _ = run("--master-url", "https://p.example.com", "--token", "t", "--agent-token", "a", "--pull-token", "p",
		"--listen-port", "24889", "--pull-port", "24890", "--master-key", securechan.EncodePublicKey(pub), "--mode", "auto")
	if !strings.Contains(out, "主控公钥指纹核对通过") {
		t.Fatalf("带四模式与端口参数时应当能解析并过指纹检查：%s", out)
	}
}

type routeTestDir struct{}

func (routeTestDir) Lookup(_ context.Context, tok string) (agentconn.Node, bool, error) {
	return agentconn.Node{ID: 1, Name: "hk-1"}, tok == "tok", nil
}
func (routeTestDir) Online(context.Context, int64, string, bool, time.Time, time.Time) error {
	return nil
}
func (routeTestDir) Offline(context.Context, int64, time.Time) error { return nil }

func TestHandlerHTTPRoutesAndMethodChecks(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	conns := agentconn.New(agentconn.Config{Identity: priv, Directory: routeTestDir{}})
	h := NewHandler(conns, Script(pub, release.PublicKeys), http.NotFoundHandler())

	// GET 到 POST-only 接口返回 400 并带 Allow: POST。
	for _, p := range []string{HandshakePath, RPCPath} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("GET %s 应当拒绝并带 Allow=POST：%d %s", p, rec.Code, rec.Header().Get("Allow"))
		}
	}

	// 无会话 POST /api/remote/rpc 回 412。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RPCPath, bytes.NewReader([]byte(`{}`))))
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("缺会话应当回 412，得到 %d", rec.Code)
	}

	// 完整握手 + 加密 heartbeat RPC。
	ephPriv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	ephPub := ephPriv.PublicKey().Bytes()
	hsBody, _ := json.Marshal(xrpc.HTTPHandshakeReq{AgentEphemeral: ephPub})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, HandshakePath, bytes.NewReader(hsBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("握手失败：%d %s", rec.Code, rec.Body.String())
	}
	var hs xrpc.HTTPHandshakeResp
	_ = json.Unmarshal(rec.Body.Bytes(), &hs)
	if !securechan.VerifyHandshake(pub, ephPub, hs.MasterEphemeral, hs.Signature) {
		t.Fatal("握手签名无效")
	}
	mPub, _ := ecdh.X25519().NewPublicKey(hs.MasterEphemeral)
	shared, _ := ephPriv.ECDH(mPub)
	sess, _ := securechan.DeriveSession(shared, ephPub, hs.MasterEphemeral, false)

	plain, _ := json.Marshal(xrpc.AgentHTTPRPC{Method: xrpc.HTTPRPCMethodHeartbeat, Token: "tok", ControlProtocol: xrpc.ControlProtocol})
	env, _ := sess.Encrypt(plain)
	rpcReq := httptest.NewRequest(http.MethodPost, RPCPath, bytes.NewReader(env))
	rpcReq.Header.Set(securechan.HeaderSession, hs.SessionID)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, rpcReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("RPC 失败：%d %s", rec.Code, rec.Body.String())
	}
	dec, err := sess.Decrypt(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var rep xrpc.AgentHTTPRPCReply
	if err := json.Unmarshal(dec, &rep); err != nil || !rep.OK {
		t.Fatalf("加密回复应当成功：%+v %v", rep, err)
	}
}
