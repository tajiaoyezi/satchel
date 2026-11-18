package xrpc

import (
	"slices"
	"time"
)

// 连接管控与流量的方法（m2-03 design 第 1 条，node-limiter「通道上的连接管控方法」、node-traffic「定时拉取计数快照」）。
// 都是主控发请求、节点回复；参数问题由节点回 CodeBadParams。
const (
	MethodLimiterSet        = "limiter.set"             // LimiterSetParams → LimiterState
	MethodLimiterUnset      = "limiter.unset"           // LimiterUnsetParams → LimiterState
	MethodConnectionsList   = "connections.list"        // ConnectionsListParams → ConnectionsList
	MethodConnectionsKick   = "connections.kick"        // ConnectionsKickParams → ConnectionsKicked
	MethodTrafficSnapshot   = "traffic.snapshot"        // 无参数 → TrafficSnapshot
	MethodKernelRestart     = "kernel.restart"          // 无参数 → KernelRestarted
	MethodTokenRotate       = "token.rotate"            // TokenRotateParams → TokenRotateAck
	MethodAgentUpdate       = "agent.update"            // AgentUpdateParams → AgentUpdateAck
	MethodAgentUninstall    = "agent.uninstall"         // AgentUninstallParams → AgentUninstallAck
	MethodAgentSwitchPorts  = "agent.switch_ports"      // SwitchPortsParams → SwitchPortsAck
	MethodAgentProbeMaster  = "agent.probe_master_url"  // ProbeMasterParams → ProbeMasterResult
	MethodAgentUpdateMaster = "agent.update_master_url" // UpdateMasterParams → UpdateMasterAck
)

// 节点远程升级确认失败时的磁盘三态口径（第 09 章）。
const (
	DiskStateOld            = "old"
	DiskStateNewUnconfirmed = "new_unconfirmed"
	DiskStateOldAfterSwap   = "old_after_swap"
)

// 限速来源：每份「入站 × 凭据」三份，生效值取已设且大于 0 的那几份里最小的一个（技术方案第 10 章）。
const (
	SourceStatic   = "static"    // 静态：M5 起由四层覆盖算出
	SourceMaster   = "master"    // 主控临时：超额处理与临时限速，同一时刻只有一份
	SourceNodeRule = "node_rule" // 节点规则临时：节点按套餐里的速率规则触发
)

// Sources 是三份来源，回复里按这个顺序列出。
var Sources = []string{SourceStatic, SourceMaster, SourceNodeRule}

// ValidSource 报告 s 是不是三份来源之一。
func ValidSource(s string) bool { return slices.Contains(Sources, s) }

// LimiterSetParams 是 limiter.set 的参数：给「入站 × 凭据」的某一份来源设值。BPS 是比特每秒，0 表示这一份显式不限速；
// Deadline 为空表示不带截止时间，否则到时这一份自动撤掉。
type LimiterSetParams struct {
	Inbound    string     `json:"inbound"`
	Credential string     `json:"credential"`
	Source     string     `json:"source"`
	BPS        int64      `json:"bps"`
	Deadline   *time.Time `json:"deadline,omitempty"`
}

// LimiterUnsetParams 是 limiter.unset 的参数：撤掉某一份来源，它退出合成。
type LimiterUnsetParams struct {
	Inbound    string `json:"inbound"`
	Credential string `json:"credential"`
	Source     string `json:"source"`
}

// LimitSource 是一份已设的来源。
type LimitSource struct {
	Source   string     `json:"source"`
	BPS      int64      `json:"bps"`
	Deadline *time.Time `json:"deadline,omitempty"`
}

// LimiterState 是 limiter.set 与 limiter.unset 的回复：这份凭据改完之后已设的来源（没设的不出现，按 Sources 的顺序）
// 与生效值；Limited 为假表示不限速，这时 EffectiveBPS 为 0。
type LimiterState struct {
	Inbound      string        `json:"inbound"`
	Credential   string        `json:"credential"`
	Sources      []LimitSource `json:"sources"`
	Limited      bool          `json:"limited"`
	EffectiveBPS int64         `json:"effective_bps"`
}

// ConnectionsListParams 是 connections.list 的参数。
type ConnectionsListParams struct {
	Inbound    string `json:"inbound"`
	Credential string `json:"credential"`
}

// Connection 是一条活动连接：网络类型是 tcp 或 udp，Peer 是对端地址。
type Connection struct {
	ID       uint64    `json:"id"`
	Network  string    `json:"network"`
	Peer     string    `json:"peer"`
	Outbound string    `json:"outbound"`
	Start    time.Time `json:"start"`
}

// ConnectionsList 是 connections.list 的回复。
type ConnectionsList struct {
	Connections []Connection `json:"connections"`
}

// ConnectionsKickParams 是 connections.kick 的参数：Inbound 为空时踢这个凭据名在所有入站上的连接。
type ConnectionsKickParams struct {
	Credential string `json:"credential"`
	Inbound    string `json:"inbound,omitempty"`
}

// ConnectionsKicked 是 connections.kick 的回复：踢掉的连接条数。
type ConnectionsKicked struct {
	Kicked int `json:"kicked"`
}

// TrafficCount 是快照里一个（入站，出站，凭据）组合的累计字节数（应用层，上下行分开）。
type TrafficCount struct {
	Inbound    string `json:"inbound"`
	Outbound   string `json:"outbound"`
	Credential string `json:"credential"`
	Up         int64  `json:"up"`
	Down       int64  `json:"down"`
}

// TrafficSnapshot 是 traffic.snapshot 的回复。BootID 每个 satchel-agent 进程一个；Generation 是这个进程里内核成功启动过
// 几次；计数从进程启动起累计、跨内核重启不清零。接收方按重启基线的规则收成累计（agent-kernel「重启基线」）。
type TrafficSnapshot struct {
	BootID          string         `json:"boot_id"`
	Generation      uint64         `json:"generation"`
	At              time.Time      `json:"at"`
	Counts          []TrafficCount `json:"counts"`
	KernelRunning   bool           `json:"kernel_running"`
	KernelVersion   string         `json:"kernel_version,omitempty"`
	KernelStartedAt *time.Time     `json:"kernel_started_at,omitempty"`
}

// KernelRestarted 是 kernel.restart 的回复。
type KernelRestarted struct {
	Running   bool       `json:"running"`
	Version   string     `json:"version,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Inbounds  []string   `json:"inbounds"`
}

// TokenRotateParams 是 token.rotate 的参数：非空字段表示需要原子写进节点配置文件的对应新凭据。
type TokenRotateParams struct {
	Token      string `json:"token,omitempty"`
	AgentToken string `json:"agent_token,omitempty"`
	PullToken  string `json:"pull_token,omitempty"`
}

// TokenRotateAck 是 token.rotate 原子落盘后的确认回复。
type TokenRotateAck struct {
	UpdatedAt time.Time `json:"updated_at"`
}

// AgentUpdateParams 是 agent.update 的参数：通知节点触发纯 Go 自更新流程。
type AgentUpdateParams struct {
	Version                 string `json:"version"`
	Channel                 string `json:"channel,omitempty"`
	UseCDN                  bool   `json:"use_cdn"`
	ExpectedControlProtocol int    `json:"expected_control_protocol,omitempty"`
}

// AgentUpdateAck 是 agent.update 在原地 exec 前回传的确认与结果分类字段。
type AgentUpdateAck struct {
	Accepted        bool   `json:"accepted"`
	PreviousVersion string `json:"previous_version"`
	TargetVersion   string `json:"target_version"`
	Source          string `json:"source,omitempty"`
	DiskState       string `json:"disk_state,omitempty"`
	Retryable       bool   `json:"retryable,omitempty"`
	BatchContinue   bool   `json:"batch_continue,omitempty"`
	Message         string `json:"message,omitempty"`
}

// AgentUninstallParams 是 agent.uninstall 的参数：DetachDelayMS 指定回执写出后的脱离子进程等待毫秒数。
type AgentUninstallParams struct {
	DetachDelayMS int64 `json:"detach_delay_ms,omitempty"`
}

// AgentUninstallAck 是 agent.uninstall 派生脱离子进程后的确认回复。
type AgentUninstallAck struct {
	Accepted    bool      `json:"accepted"`
	ScheduledAt time.Time `json:"scheduled_at"`
	Spawner     string    `json:"spawner,omitempty"`
}

// SwitchPortsParams 是 agent.switch_ports 的参数：0 表示保持该端口不变。
type SwitchPortsParams struct {
	ListenPort int `json:"listen_port,omitempty"`
	PullPort   int `json:"pull_port,omitempty"`
}

// SwitchPortsAck 是 agent.switch_ports 落盘并完成进程内热重绑后的回复。
type SwitchPortsAck struct {
	ListenPort int       `json:"listen_port"`
	PullPort   int       `json:"pull_port"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ProbeMasterParams 是 agent.probe_master_url 的参数：目标地址由主控从已登记设置选出。
type ProbeMasterParams struct {
	URL       string `json:"url"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

// ProbeMasterResult 是 agent.probe_master_url 经节点本地主控公钥握手探测后的回复。
type ProbeMasterResult struct {
	URL       string `json:"url"`
	Reachable bool   `json:"reachable"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// UpdateMasterParams 是 agent.update_master_url 的参数：通知节点原子更新主控地址并原地重连。
type UpdateMasterParams struct {
	MasterURL string `json:"master_url"`
}

// UpdateMasterAck 是 agent.update_master_url 原子落盘后的确认回复。
type UpdateMasterAck struct {
	MasterURL string    `json:"master_url"`
	UpdatedAt time.Time `json:"updated_at"`
}
