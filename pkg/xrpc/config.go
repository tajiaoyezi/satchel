package xrpc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// CanonicalJSON 将合法 JSON 归一化为键按字典序排列、无冗余空白、不转义 HTML 字符的确定性字节序列。
// 主控与节点共同调用本函数，避免因键序或缩进差异触发配置哈希伪冲突。
func CanonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("JSON 后存在多余内容")
		}
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// CanonicalConfigHash 计算配置文本的小写十六进制 SHA-256 哈希。
// 合法标准 JSON 先经 CanonicalJSON 归一化；若包含 sing-box 注释等非标准 JSON 语法，退回对去首尾空白原文字节计算哈希。
func CanonicalConfigHash(raw []byte) string {
	data, err := CanonicalJSON(raw)
	if err != nil {
		data = bytes.TrimSpace(raw)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ConfigStatusResult 是 config.status 的回复：报告节点当前 kernel.json 与内核运行态。
type ConfigStatusResult struct {
	Exists        bool       `json:"exists"`
	Path          string     `json:"path,omitempty"`
	Hash          string     `json:"hash,omitempty"`
	Size          int64      `json:"size,omitempty"`
	Modified      *time.Time `json:"modified,omitempty"`
	Inbounds      []string   `json:"inbounds"`
	KernelRunning bool       `json:"kernel_running"`
	KernelVersion string     `json:"kernel_version,omitempty"`
	Generation    uint64     `json:"generation"`
}

// ConfigTestParams 是 config.test 的参数：传入待校验的完整 sing-box 配置文本。
type ConfigTestParams struct {
	Config string `json:"config"`
}

// ConfigTestResult 是 config.test 的回复：无侵入校验结果、解析出的入站 tag 与规范哈希。
type ConfigTestResult struct {
	OK       bool     `json:"ok"`
	Error    string   `json:"error,omitempty"`
	Inbounds []string `json:"inbounds"`
	Hash     string   `json:"hash,omitempty"`
}

// ConfigApplyParams 是 config.apply 的参数：带期望基线哈希的原子配置事务请求。
// Restart 为 nil 时默认按 true 处理（落盘后同进程重启内核）。
type ConfigApplyParams struct {
	Config              string `json:"config"`
	ExpectedCurrentHash string `json:"expected_current_hash,omitempty"`
	ForceInitial        bool   `json:"force_initial,omitempty"`
	Restart             *bool  `json:"restart,omitempty"`
	ApplyID             string `json:"apply_id,omitempty"`
}

// WantRestart 返回本次 config.apply 是否要求落盘后重启内核（默认 true）。
func (p ConfigApplyParams) WantRestart() bool {
	if p.Restart == nil {
		return true
	}
	return *p.Restart
}

// ConfigApplyResult 是 config.apply 的回复：记录生效哈希、内核代次以及失败时的自动回滚结果。
type ConfigApplyResult struct {
	Applied       bool     `json:"applied"`
	AppliedHash   string   `json:"applied_hash,omitempty"`
	PreviousHash  string   `json:"previous_hash,omitempty"`
	CurrentHash   string   `json:"current_hash,omitempty"`
	Generation    uint64   `json:"generation"`
	KernelRunning bool     `json:"kernel_running"`
	Inbounds      []string `json:"inbounds"`
	RolledBack    bool     `json:"rolled_back,omitempty"`
	RollbackError string   `json:"rollback_error,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// ConfigFilesParams 是 config.files 的参数：Scope 取值 all（默认）、kernel 或 nginx。
type ConfigFilesParams struct {
	Scope string `json:"scope,omitempty"`
}

// ConfigFileInfo 是节点白名单目录内单个只读配置文件的元数据。
type ConfigFileInfo struct {
	Scope    string    `json:"scope"`
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// ConfigFilesResult 是 config.files 的回复。
type ConfigFilesResult struct {
	Files []ConfigFileInfo `json:"files"`
}

// ConfigReadParams 是 config.read 的参数：Path 必须落在节点白名单配置目录内。
type ConfigReadParams struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

// ConfigReadResult 是 config.read 的回复：单次返回上限受 ConfigReadMaxBytes 约束。
type ConfigReadResult struct {
	Scope     string    `json:"scope"`
	Path      string    `json:"path"`
	Content   string    `json:"content"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	Truncated bool      `json:"truncated,omitempty"`
	Hash      string    `json:"hash,omitempty"`
}
