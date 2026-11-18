package agentconn

import (
	"context"
	"fmt"
	"time"

	"github.com/satchel/satchel/internal/base/selfupdate"
	"github.com/satchel/satchel/pkg/xrpc"
)

type rotateWindow struct {
	oldToken  string
	node      Node
	expiresAt time.Time
}

type agentRotateWindow struct {
	oldAgentToken string
	expiresAt     time.Time
}

type conflictState struct {
	winnerIP    string
	lastActive  time.Time
	rejectUntil time.Time
}

// clearPending 在节点凭当前有效 token 完成认证或心跳时清除 rotation_pending / revoke_pending。
func (m *Manager) clearPending(ctx context.Context, id int64) error {
	c, ok := m.cfg.Directory.(PendingClearer)
	if !ok {
		return nil
	}
	return c.ClearPending(ctx, id)
}

// targetInfo 查询管理口与只读轮询口反向调用所需寻址信息。
func (m *Manager) targetInfo(ctx context.Context, id int64) (TargetInfo, bool, error) {
	td, ok := m.cfg.Directory.(TargetDirectory)
	if !ok {
		return TargetInfo{}, false, nil
	}
	return td.Target(ctx, id)
}

// BeginTokenWindow 为指定服务器在内存中开启短暂的双凭据过渡窗口，窗口内新旧 token 均可通过认证。
func (m *Manager) BeginTokenWindow(serverID int64, node Node, oldToken string) {
	if serverID <= 0 || oldToken == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, w := range m.rotations {
		if w.node.ID == serverID {
			delete(m.rotations, k)
		}
	}
	now := m.cfg.Now()
	m.rotations[oldToken] = &rotateWindow{
		oldToken:  oldToken,
		node:      node,
		expiresAt: now.Add(m.cfg.RotateWindow),
	}
}

// ConfirmTokenWindow 关闭该服务器的双凭据过渡窗口（节点确认轮换落盘或回滚时调用）。
func (m *Manager) ConfirmTokenWindow(serverID int64) {
	if serverID <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, w := range m.rotations {
		if w.node.ID == serverID {
			delete(m.rotations, k)
		}
	}
	delete(m.agentRotations, serverID)
}

// BeginAgentTokenWindow 记录轮换前旧的 agent_token（30 秒内供无活跃 WS 时向节点推送 token.rotate 使用）。
func (m *Manager) BeginAgentTokenWindow(serverID int64, oldAgentToken string) {
	if serverID <= 0 || oldAgentToken == "" {
		return
	}
	m.mu.Lock()
	m.agentRotations[serverID] = &agentRotateWindow{
		oldAgentToken: oldAgentToken,
		expiresAt:     m.cfg.Now().Add(m.cfg.RotateWindow),
	}
	m.mu.Unlock()
}

// ConfirmAgentTokenWindow 关闭该服务器的旧 agent_token 过渡窗口。
func (m *Manager) ConfirmAgentTokenWindow(serverID int64) {
	if serverID <= 0 {
		return
	}
	m.mu.Lock()
	delete(m.agentRotations, serverID)
	m.mu.Unlock()
}

func (m *Manager) agentCandidateTokens(serverID int64, currentToken, method string) []string {
	now := m.cfg.Now()
	m.mu.Lock()
	var oldTok string
	if w, ok := m.agentRotations[serverID]; ok {
		if now.Before(w.expiresAt) {
			oldTok = w.oldAgentToken
		} else {
			delete(m.agentRotations, serverID)
		}
	}
	m.mu.Unlock()
	if oldTok == "" || oldTok == currentToken {
		if currentToken == "" {
			return nil
		}
		return []string{currentToken}
	}
	if method == xrpc.MethodTokenRotate {
		return []string{oldTok, currentToken}
	}
	return []string{currentToken, oldTok}
}

// lookupToken 先查库中当前有效 token（isCurrent=true），未命中时再查未过期的内存轮换过渡旧 token（isCurrent=false）。
func (m *Manager) lookupToken(ctx context.Context, token string) (node Node, isCurrent, found bool, err error) {
	if token == "" {
		return Node{}, false, false, nil
	}
	node, ok, err := m.cfg.Directory.Lookup(ctx, token)
	if err != nil {
		return Node{}, false, false, err
	}
	if ok {
		return node, true, true, nil
	}
	now := m.cfg.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, w := range m.rotations {
		if !now.Before(w.expiresAt) {
			delete(m.rotations, k)
		}
	}
	if w, exists := m.rotations[token]; exists {
		return w.node, false, true, nil
	}
	return Node{}, false, false, nil
}

// allowConflict 按传输层真实 IP 判断是否允许同 token 的接入；若同一服务器持有人仍在活跃窗口或处于冷却期内且来源 IP 不同，
// 开启或顺延 60 秒冷却并返回 false。
func (m *Manager) allowConflict(serverID int64, ip string) bool {
	if ip == "" {
		return true
	}
	now := m.cfg.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.conflicts[serverID]
	if st == nil || st.winnerIP == "" || st.winnerIP == ip {
		return true
	}
	if now.Sub(st.lastActive) < m.cfg.ConflictWindow || now.Before(st.rejectUntil) {
		st.rejectUntil = now.Add(m.cfg.ConflictCooldown)
		return false
	}
	return true
}

func (m *Manager) recordActiveIPLocked(serverID int64, ip string, now time.Time) {
	if ip == "" {
		return
	}
	st := m.conflicts[serverID]
	if st == nil {
		st = &conflictState{}
		m.conflicts[serverID] = st
	}
	st.winnerIP = ip
	st.lastActive = now
	st.rejectUntil = time.Time{}
}

func (m *Manager) touchConflict(serverID int64, ip string) {
	if ip == "" {
		return
	}
	m.mu.Lock()
	m.recordActiveIPLocked(serverID, ip, m.cfg.Now())
	m.mu.Unlock()
}

// authorizeNode 校验 WS 与 HTTP 上行通道共用的凭据、版本兼容与同 token 异 IP 复用冲突规则。
func (m *Manager) authorizeNode(ctx context.Context, ip, token string, controlProto int, minMasterVer string) (node Node, isCurrent bool, reason, msg string) {
	node, isCurrent, found, err := m.lookupToken(ctx, token)
	if err != nil {
		m.cfg.Logger.Warn("按 token 查服务器失败", "err", err)
		return Node{}, false, xrpc.AuthUnavailable, "主控查询服务器失败，稍后重试"
	}
	if !found {
		m.recordFailure(ip)
		m.cfg.Logger.Warn("节点认证失败：token 无效", "ip", ip)
		return Node{}, false, xrpc.AuthInvalidToken, "token 无效"
	}
	if controlProto > xrpc.ControlProtocol {
		m.cfg.Logger.Warn("节点认证失败：agent 比主控新", "server", node.Name, "agent_protocol", controlProto)
		return Node{}, false, xrpc.AuthIncompatible, fmt.Sprintf(
			"agent 的控制协议号 %d 比主控的 %d 新：agent 不能先于主控升级，先升级主控", controlProto, xrpc.ControlProtocol)
	}
	if minMasterVer != "" && !selfupdate.IsDev(m.cfg.Version) &&
		(!selfupdate.Valid(minMasterVer) || selfupdate.Compare(m.cfg.Version, minMasterVer) < 0) {
		m.cfg.Logger.Warn("节点认证失败：主控版本太旧或节点要求非法版本", "server", node.Name, "master", m.cfg.Version, "min", minMasterVer)
		return Node{}, false, xrpc.AuthIncompatible, fmt.Sprintf(
			"这个 agent 要求主控至少是 %s，当前主控是 %s：agent 不能先于主控升级，先升级主控", minMasterVer, m.cfg.Version)
	}
	if !m.allowConflict(node.ID, ip) {
		m.cfg.Logger.Warn("节点认证失败：同 token 异 IP 复用冲突", "server", node.Name, "ip", ip)
		return Node{}, false, xrpc.AuthIPConflict, "同 token 正在另一个 IP 上活跃，已拒绝接入；旧连接失活并冷却后才能切换"
	}
	return node, isCurrent, "", ""
}

// InvalidateServer 在吊销或凭据切换后立即清理缓存会话与活跃连接：
// dropUplink 为真时清空该服务器的上行 HTTP SessionCache、过渡窗口、冲突锁，并立刻踢断活跃 WS 连接；
// roles 指定需从下行客户端移除的节点端口会话角色（如 "manage"、"pull"）。
func (m *Manager) InvalidateServer(serverID int64, dropUplink bool, roles ...string) {
	if serverID <= 0 {
		return
	}
	if len(roles) > 0 && m.nodeClient != nil {
		m.nodeClient.ClearServerSessions(serverID, roles...)
	}
	if !dropUplink {
		return
	}
	m.uplinkSessions.DeleteByServer(serverID)
	m.ConfirmTokenWindow(serverID)

	m.mu.Lock()
	delete(m.conflicts, serverID)
	s := m.conns[serverID]
	if s != nil {
		s.revoked.Store(true)
	}
	m.mu.Unlock()
	if s != nil {
		s.conn.WS().CloseNow()
	}
}
