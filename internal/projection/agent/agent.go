package agent

import (
	"crypto/ed25519"
	_ "embed"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/projection/rest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/securechan"
)

// Prefix 是节点通道在顶层 mux 上的挂载点，归三道门表的机器入口（master-access-gates）。本 change 只有 websocket 一种接法。
const Prefix = "/api/remote/"

const (
	// WSPath 是节点的 websocket 连接（node-channel）。
	WSPath = Prefix + "ws"
	// InstallPath 是节点的安装脚本（node-pairing「主控提供的安装脚本」）。
	InstallPath = Prefix + "install.sh"
)

//go:embed install.sh.tmpl
var installTemplate string

// Script 渲染安装脚本：写进主控公钥的指纹与发布公钥清单（一行一把）。脚本里没有任何秘密。
func Script(pub ed25519.PublicKey, releaseKeys []string) []byte {
	return []byte(strings.NewReplacer(
		"@@MASTER_KEY_FINGERPRINT@@", securechan.Fingerprint(pub),
		"@@RELEASE_PUBLIC_KEYS@@", strings.Join(releaseKeys, "\n"),
	).Replace(installTemplate))
}

// Handler 处理 /api/remote/ 下的请求：websocket 交给连接管理，安装脚本直接给。都不要求身份（身份在通道里由 token 认）。
type Handler struct {
	conns    *agentconn.Manager
	script   []byte
	notFound http.Handler
}

// NewHandler 建处理器；script 是 Script 渲染好的安装脚本，notFound 是不认识的路径的回应。
func NewHandler(conns *agentconn.Manager, script []byte, notFound http.Handler) *Handler {
	return &Handler{conns: conns, script: script, notFound: notFound}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case WSPath:
		h.serveWS(w, r)
	case InstallPath:
		h.serveScript(w, r)
	default:
		h.notFound.ServeHTTP(w, r)
	}
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodGet)
	rest.WriteError(w, v1.Newf(v1.CodeBadRequest, "%s 只接受 GET，收到 %s", r.URL.Path, r.Method))
}

func (h *Handler) serveScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(h.script)
}

// serveWS 升级成 websocket 并交给连接管理；来源 IP 因连续认证失败被暂时拒绝时，升级之前回 429（node-channel「节点认证」）。
func (h *Handler) serveWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r)
		return
	}
	ip := v1.RemoteFrom(r.Context()).IP
	if h.conns.Blocked(ip) {
		rest.WriteError(w, v1.New(v1.CodeRateLimited, "这个来源 IP 最近认证失败太多次，暂时拒绝连接，稍后再试").
			WithNext("检查节点配置里的 token 是否与主控上这台服务器的一致；一分钟后会自动放开"))
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept 已经写了回应
	}
	h.conns.Serve(r.Context(), ws, ip)
}
