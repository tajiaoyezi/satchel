package agentconn

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/satchel/satchel/pkg/securechan"
	"github.com/satchel/satchel/pkg/xrpc"
)

func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}

func writeRemoteError(w http.ResponseWriter, status int, code, reason string) {
	writeJSONStatus(w, status, map[string]string{
		"code":   code,
		"reason": reason,
	})
}

func writeEncryptedReply(w http.ResponseWriter, sess *securechan.Session, reply xrpc.AgentHTTPRPCReply) {
	plain, err := json.Marshal(reply)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	env, err := sess.Encrypt(plain)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(env)
}

// ServeHandshake 处理节点的 POST /api/remote/handshake：
// 严格禁止节点携带 Authorization 头，交换临时公钥并用主控身份私钥双向签名后在 SessionCache 缓存会话。
func (m *Manager) ServeHandshake(w http.ResponseWriter, r *http.Request, ip string) {
	if r.Header.Get("Authorization") != "" {
		writeRemoteError(w, http.StatusBadRequest, "bad_request", "节点上行接口不得携带 Authorization 请求头")
		return
	}
	if m.Blocked(ip) {
		writeRemoteError(w, http.StatusTooManyRequests, "rate_limited", "这个来源 IP 最近认证失败太多次，暂时拒绝连接")
		return
	}
	m.mu.Lock()
	closing := m.closing
	m.mu.Unlock()
	if closing {
		writeRemoteError(w, http.StatusServiceUnavailable, "unavailable", "主控正在停止")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, preAuthReadLimit))
	if err != nil {
		writeRemoteError(w, http.StatusBadRequest, "bad_request", "读取握手请求失败")
		return
	}
	var req xrpc.HTTPHandshakeReq
	if err := json.Unmarshal(raw, &req); err != nil || len(req.AgentEphemeral) != 32 {
		writeRemoteError(w, http.StatusBadRequest, "bad_request", "握手请求格式或临时公钥长度不合法")
		return
	}
	agentEph, err := ecdh.X25519().NewPublicKey(req.AgentEphemeral)
	if err != nil {
		writeRemoteError(w, http.StatusBadRequest, "bad_request", "节点临时公钥不合法")
		return
	}
	masterEphPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		writeRemoteError(w, http.StatusInternalServerError, "internal", "生成主控临时密钥失败")
		return
	}
	shared, err := masterEphPriv.ECDH(agentEph)
	if err != nil {
		writeRemoteError(w, http.StatusBadRequest, "bad_request", "密钥协商失败")
		return
	}
	masterEphBytes := masterEphPriv.PublicKey().Bytes()
	sess, err := securechan.DeriveSession(shared, req.AgentEphemeral, masterEphBytes, true)
	if err != nil {
		writeRemoteError(w, http.StatusInternalServerError, "internal", "派生加密会话失败")
		return
	}
	sig := securechan.SignHandshake(m.cfg.Identity, req.AgentEphemeral, masterEphBytes)
	sid, err := securechan.GenerateSessionID()
	if err != nil {
		writeRemoteError(w, http.StatusInternalServerError, "internal", "生成会话标识失败")
		return
	}
	if err := m.uplinkSessions.Put(sid, sess, 0, "uplink"); err != nil {
		writeRemoteError(w, http.StatusInternalServerError, "internal", "缓存会话失败")
		return
	}
	writeJSONStatus(w, http.StatusOK, xrpc.HTTPHandshakeResp{
		SessionID:       sid,
		MasterEphemeral: masterEphBytes,
		Signature:       sig,
	})
}

// ServeRPC 处理节点的 POST /api/remote/rpc：
// 严格禁止节点携带 Authorization 头；未携带有效加密会话或会话已过期一律返回 HTTP 412，绝不回退处理明文；
// 密文解包后校验 token（支持 30 秒轮换双窗）、版本兼容、同 token 异 IP 冲突与会话绑定，再执行心跳或流量上报。
func (m *Manager) ServeRPC(w http.ResponseWriter, r *http.Request, ip string) {
	if r.Header.Get("Authorization") != "" {
		writeRemoteError(w, http.StatusBadRequest, "bad_request", "节点上行接口不得携带 Authorization 请求头")
		return
	}
	if m.Blocked(ip) {
		writeRemoteError(w, http.StatusTooManyRequests, "rate_limited", "这个来源 IP 最近认证失败太多次，暂时拒绝连接")
		return
	}
	sid := strings.TrimSpace(r.Header.Get(securechan.HeaderSession))
	if sid == "" {
		writeRemoteError(w, http.StatusPreconditionFailed, "session_required", "缺少加密会话头，请先完成握手")
		return
	}
	sess, _, _, ok := m.uplinkSessions.Get(sid)
	if !ok {
		writeRemoteError(w, http.StatusPreconditionFailed, "session_expired", "加密会话不存在或已过期，请重新握手")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, xrpc.ReadLimit))
	if err != nil {
		writeRemoteError(w, http.StatusBadRequest, "bad_request", "读取加密载荷失败")
		return
	}
	plain, err := sess.Decrypt(body)
	if err != nil {
		writeRemoteError(w, http.StatusBadRequest, "bad_request", "加密信封解包或验签失败")
		return
	}
	var req xrpc.AgentHTTPRPC
	if err := json.Unmarshal(plain, &req); err != nil {
		writeRemoteError(w, http.StatusBadRequest, "bad_request", "RPC 载荷解析失败")
		return
	}
	ctx := r.Context()
	node, isCurrent, reason, msg := m.authorizeNode(ctx, ip, req.Token, req.ControlProtocol, req.MinMasterVersion)
	if reason != "" {
		writeEncryptedReply(w, sess, xrpc.AgentHTTPRPCReply{OK: false, Reason: reason, Message: msg})
		return
	}
	if err := m.uplinkSessions.BindServer(sid, node.ID); err != nil {
		if errors.Is(err, securechan.ErrSessionServerMismatch) {
			m.uplinkSessions.Delete(sid)
			writeRemoteError(w, http.StatusForbidden, "forbidden", "同一加密会话不得跨服务器复用")
			return
		}
		writeRemoteError(w, http.StatusPreconditionFailed, "session_expired", "加密会话不存在或已过期，请重新握手")
		return
	}

	rt := NodeRuntime{
		AgentVersion:     req.AgentVersion,
		InDocker:         req.InDocker,
		MachineID:        req.MachineID,
		MasterURL:        req.MasterURL,
		MinMasterVersion: req.MinMasterVersion,
	}
	m.RecordRuntime(node.ID, rt)

	now := m.now()
	cfg := m.agentConfig(ctx)
	switch req.Method {
	case xrpc.HTTPRPCMethodHeartbeat:
		var boot time.Time
		if req.Heartbeat != nil {
			boot = req.Heartbeat.BootTime
		}
		if err := m.httpHeartbeat(ctx, node, ip, req.MachineID, boot, isCurrent); err != nil {
			writeEncryptedReply(w, sess, xrpc.AgentHTTPRPCReply{OK: false, Reason: xrpc.AuthUnavailable, Message: "记录心跳失败，稍后重试"})
			return
		}
		writeEncryptedReply(w, sess, xrpc.AgentHTTPRPCReply{OK: true, HeartbeatInterval: cfg.HeartbeatInterval, Config: &cfg, ServerTime: &now})
		if req.MasterURL != "" {
			go m.maybeSyncMasterURL(node.ID, node.Name, req.MasterURL)
		}
	case xrpc.HTTPRPCMethodTraffic:
		if req.Traffic == nil {
			writeEncryptedReply(w, sess, xrpc.AgentHTTPRPCReply{OK: false, Reason: xrpc.CodeBadParams, Message: "缺少 traffic 快照载荷"})
			return
		}
		m.touchConflict(node.ID, ip)
		if m.cfg.OnTraffic != nil {
			if err := m.cfg.OnTraffic(ctx, node.ID, *req.Traffic); err != nil {
				writeEncryptedReply(w, sess, xrpc.AgentHTTPRPCReply{OK: false, Reason: "traffic_error", Message: err.Error()})
				return
			}
		}
		writeEncryptedReply(w, sess, xrpc.AgentHTTPRPCReply{OK: true, HeartbeatInterval: cfg.HeartbeatInterval, Config: &cfg, ServerTime: &now})
	case xrpc.HTTPRPCMethodSpeed:
		if req.Speed == nil {
			writeEncryptedReply(w, sess, xrpc.AgentHTTPRPCReply{OK: false, Reason: xrpc.CodeBadParams, Message: "缺少 speed 载荷"})
			return
		}
		m.touchConflict(node.ID, ip)
		if m.cfg.OnSpeed != nil {
			m.cfg.OnSpeed(ctx, node.ID, *req.Speed)
		}
		writeEncryptedReply(w, sess, xrpc.AgentHTTPRPCReply{OK: true, HeartbeatInterval: cfg.HeartbeatInterval, Config: &cfg, ServerTime: &now})
	default:
		writeEncryptedReply(w, sess, xrpc.AgentHTTPRPCReply{OK: false, Reason: xrpc.CodeUnknownMethod, Message: "未知的 HTTP RPC 方法"})
	}
}

func (m *Manager) httpHeartbeat(ctx context.Context, node Node, ip, machineID string, boot time.Time, isCurrent bool) error {
	l := m.serverLock(node.ID)
	lockCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := l.lock(lockCtx); err != nil {
		return err
	}
	defer l.unlock()
	m.touchConflict(node.ID, ip)
	wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
	defer wcancel()
	if err := m.cfg.Directory.Online(wctx, node.ID, ip, isV6(ip), boot, m.now()); err != nil {
		return err
	}
	m.updateSameHost(wctx, node.ID, node.Name, ip, machineID)
	if isCurrent {
		if err := m.clearPending(wctx, node.ID); err != nil {
			m.cfg.Logger.Warn("清除待确认标志失败", "server", node.Name, "err", err)
		}
	}
	return nil
}
