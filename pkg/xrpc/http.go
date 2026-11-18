package xrpc

import "time"

// Agent → Master 加密 HTTP 上行入口与 Master → Agent 加密 HTTP 节点端入口。
const (
	HTTPMasterPathHandshake = "/api/remote/handshake"
	HTTPMasterPathRPC       = "/api/remote/rpc"

	HTTPNodePathHealth          = "/health"
	HTTPNodePathHandshakeInit   = "/handshake/init"
	HTTPNodePathHandshakeFinish = "/handshake/finish"
	HTTPNodePathRPC             = "/rpc"
	HTTPNodePathPullTraffic     = "/api/child/traffic"

	HTTPRPCMethodHeartbeat = "heartbeat"
	HTTPRPCMethodTraffic   = "traffic"
)

// HTTPHandshakeReq 是节点向主控 POST /api/remote/handshake 的请求体。
type HTTPHandshakeReq struct {
	AgentEphemeral []byte `json:"agent_ephemeral"`
}

// HTTPHandshakeResp 是主控 POST /api/remote/handshake 的响应体：签名用 securechan.SignHandshake(priv, agentEphPub, masterEphPub)。
type HTTPHandshakeResp struct {
	SessionID       string `json:"session_id"`
	MasterEphemeral []byte `json:"master_ephemeral"`
	Signature       []byte `json:"signature"`
}

// HTTPHeartbeatPayload 是 Agent → Master 加密 HTTP RPC 中 heartbeat 的载荷。
type HTTPHeartbeatPayload struct {
	BootTime time.Time `json:"boot_time"`
}

// AgentHTTPRPC 是节点在加密信封内通过 POST /api/remote/rpc 发给主控的载荷：
// 凭据始终只放在密文内部，严禁放进 HTTP Authorization 请求头。
type AgentHTTPRPC struct {
	Method           string                `json:"method"` // "heartbeat" 或 "traffic"
	Token            string                `json:"token"`
	AgentVersion     string                `json:"agent_version"`
	ControlProtocol  int                   `json:"control_protocol"`
	MinMasterVersion string                `json:"min_master_version,omitempty"`
	Heartbeat        *HTTPHeartbeatPayload `json:"heartbeat,omitempty"`
	Traffic          *TrafficSnapshot      `json:"traffic,omitempty"`
}

// AgentHTTPRPCReply 是主控在加密信封内返回 POST /api/remote/rpc 的响应。
type AgentHTTPRPCReply struct {
	OK                bool       `json:"ok"`
	Reason            string     `json:"reason,omitempty"`
	Message           string     `json:"message,omitempty"`
	HeartbeatInterval int        `json:"heartbeat_interval,omitempty"`
	ServerTime        *time.Time `json:"server_time,omitempty"`
}

// NodeHandshakeInitResp 是节点管理口 / 轮询口 POST /handshake/init 的响应。
type NodeHandshakeInitResp struct {
	HandshakeID    string `json:"handshake_id"`
	AgentEphemeral []byte `json:"agent_ephemeral"`
}

// NodeHandshakeFinishReq 是主控向节点发送 /handshake/finish 的请求，携带双方临时公钥对应的双向 Ed25519 签名。
type NodeHandshakeFinishReq struct {
	HandshakeID     string `json:"handshake_id"`
	MasterEphemeral []byte `json:"master_ephemeral"`
	Signature       []byte `json:"signature"`
}

// NodeHandshakeFinishResp 是节点验证签名后签发的会话标识。
type NodeHandshakeFinishResp struct {
	SessionID string `json:"session_id"`
}
