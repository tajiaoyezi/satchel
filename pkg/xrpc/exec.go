package xrpc

import "time"

// 受控任意命令执行（server exec）的共享上下限常量。
const (
	ExecDefaultTimeoutMS = 30 * 1000
	ExecMinTimeoutMS     = 1000
	ExecMaxTimeoutMS     = 300 * 1000
	ExecMaxCommandBytes  = 8 << 10  // 8 KiB 单条命令上限
	ExecMaxOutputBytes   = 64 << 10 // 64 KiB 合并输出上限（与 jobs.OutputLimit 对齐）

	ExecStateRunning  = "running"
	ExecStateDone     = "done"
	ExecStateFailed   = "failed"
	ExecStateTimedOut = "timed_out"
	ExecStateCanceled = "canceled"
)

// ExecStartParams 是 exec.start 的参数。
type ExecStartParams struct {
	JobID     string `json:"job_id"`
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	MaxBytes  int    `json:"max_bytes,omitempty"`
}

// ExecStartResult 是 exec.start 的回复。
type ExecStartResult struct {
	JobID     string    `json:"job_id"`
	Accepted  bool      `json:"accepted"`
	PGID      int       `json:"pgid,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// ExecStatusParams 是 exec.status 的参数：Offset 为调用方已收到的字节偏移，用于 HTTP/pull 增量拉取与重连对账。
type ExecStatusParams struct {
	JobID  string `json:"job_id"`
	Offset int    `json:"offset,omitempty"`
}

// ExecStatusResult 是 exec.status 的回复。
type ExecStatusResult struct {
	JobID      string     `json:"job_id"`
	Found      bool       `json:"found"`
	State      string     `json:"state,omitempty"` // running | done | failed | timed_out | canceled
	Done       bool       `json:"done"`
	ExitCode   int        `json:"exit_code"`
	Chunk      string     `json:"chunk,omitempty"` // 自请求 Offset 起的增量输出片段
	NextOffset int        `json:"next_offset"`
	Output     string     `json:"output,omitempty"` // Offset==0 或终态对账时可返回完整已缓存输出
	Truncated  bool       `json:"truncated,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// ExecCancelParams 是 exec.cancel 的参数。
type ExecCancelParams struct {
	JobID string `json:"job_id"`
}

// ExecCancelResult 是 exec.cancel 的回复。
type ExecCancelResult struct {
	JobID    string `json:"job_id"`
	Canceled bool   `json:"canceled"`
}

// ExecEvent 是节点经 WebSocket 上行单向推送（TypeExecEvent）的流式输出与终态事件帧。
type ExecEvent struct {
	JobID      string    `json:"job_id"`
	Seq        uint64    `json:"seq"`
	Chunk      string    `json:"chunk,omitempty"`
	Offset     int       `json:"offset"` // 本次片段末尾累计字节偏移
	Done       bool      `json:"done,omitempty"`
	State      string    `json:"state,omitempty"`
	ExitCode   int       `json:"exit_code,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Error      string    `json:"error,omitempty"`
	At         time.Time `json:"at,omitempty"`
}
