// Package xrpc 是主控与节点之间经 websocket 的消息格式与请求 / 回复（技术方案第 06 章，m2-02 design 第 1、6 条）：
// 内容来自 mmwx 两边的 ws_rpc.go 与 remote_ws.go 里的消息类型；名字沿用技术方案里的 xrpc（mmwx 那个 internal/xrpc
// 是 Xray 的 gRPC 客户端，与这里无关）。主控与 satchel-agent 共用这一份：消息结构、控制协议号、帧的读写规则、握手、
// 请求编号与等待表。
//
// 帧的规则：握手阶段双方只发文本帧（JSON 的 Message）；握手之后只发二进制帧，内容是 securechan 的加密信封，
// 信封里是 JSON 的 Message。不符合规则的帧由读的一方报错，调用方断开连接。
package xrpc

import (
	"encoding/json"
	"time"
)

// ControlProtocol 是控制协议号：节点在认证消息里报自己的，比主控新时主控拒绝认证（agent 不能先于主控升级，第 09 章）。
// 线格式有不兼容的变化时加一。
const ControlProtocol = 1

// 消息类型。
const (
	TypeKeyExchange     = "key_exchange"
	TypeKeyExchangeResp = "key_exchange_resp"
	TypeAuth            = "auth"
	TypeAuthResult      = "auth_result"
	TypeHeartbeat       = "heartbeat"
	TypeHeartbeatAck    = "heartbeat_ack"
	TypeCall            = "rpc_call"
	TypeReply           = "rpc_reply"
)

// Message 是通道里每条消息的信封。
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// KeyExchange 是节点发的握手请求：节点的 X25519 临时公钥。
type KeyExchange struct {
	AgentEphemeral []byte `json:"agent_ephemeral"`
}

// KeyExchangeResp 是主控的握手响应：主控的 X25519 临时公钥，与主控身份私钥对这一次握手的签名（securechan.SignHandshake）。
type KeyExchangeResp struct {
	MasterEphemeral []byte `json:"master_ephemeral"`
	Signature       []byte `json:"signature"`
}

// Auth 是节点握手之后的第一条消息。MinMasterVersion 为空表示不要求。
type Auth struct {
	Token            string `json:"token"`
	AgentVersion     string `json:"agent_version"`
	ControlProtocol  int    `json:"control_protocol"`
	MinMasterVersion string `json:"min_master_version,omitempty"`
}

// AuthResult 是主控对认证的回复；成功时带主控要求的心跳间隔（秒），失败时带给机器读的原因与给人看的说明。
type AuthResult struct {
	OK                bool   `json:"ok"`
	Reason            string `json:"reason,omitempty"`
	Message           string `json:"message,omitempty"`
	HeartbeatInterval int    `json:"heartbeat_interval,omitempty"`
}

// 认证失败的原因：节点据此选退避（只有 token 无效会累计到长时间的惩罚等待）。
const (
	AuthInvalidToken = "invalid_token" // token 不对
	AuthIncompatible = "incompatible"  // 控制协议号或版本不兼容
	AuthUnavailable  = "unavailable"   // 主控暂时出错，稍后重试
)

// Heartbeat 是节点的心跳：节点进程的启动时间。
type Heartbeat struct {
	BootTime time.Time `json:"boot_time"`
}

// HeartbeatAck 是主控对心跳的确认。
type HeartbeatAck struct {
	ServerTime time.Time `json:"server_time"`
}

// Call 是一次请求：编号由发起方生成、在这条连接上唯一；TimeoutMS 告诉对方最多做多久。
type Call struct {
	ID        string          `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	TimeoutMS int64           `json:"timeout_ms,omitempty"`
}

// Reply 是对一次请求的回复：带同一个编号，结果与错误二选一。
type Reply struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// 回复里的错误码。
const (
	CodeUnknownMethod = "unknown_method" // 对方不认识这个方法
	CodeBadParams     = "bad_params"     // 参数解不开
	CodeFailed        = "failed"         // 方法执行失败
)

// Error 是回复里的错误。
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// MethodAgentInfo 是节点的 agent.info：回 AgentInfo。
const MethodAgentInfo = "agent.info"

// AgentInfo 是 agent.info 的结果。
type AgentInfo struct {
	AgentVersion    string `json:"agent_version"`
	ControlProtocol int    `json:"control_protocol"`
}
