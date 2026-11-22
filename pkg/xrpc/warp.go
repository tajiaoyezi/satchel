package xrpc

import "time"

// WARP 默认常量与 MTU 上下限，供主控与节点共享。
const (
	DefaultWarpEndpoint      = "engage.cloudflareclient.com:2408"
	DefaultWarpPeerPublicKey = "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo="
	DefaultWarpAPIBase       = "https://api.cloudflareclient.com/v0a2158"
	DefaultWarpMTU           = 1280
	MinWarpMTU               = 1280
	MaxWarpMTU               = 1500
)

// WarpStatusResult 是 warp.status、warp.install 与 warp.authorize 的脱敏状态回复。
// 结构体故意不包含 private_key、access_token 或 license_key，防止在常规接口或任务输出中泄露敏感凭据。
type WarpStatusResult struct {
	Installed     bool       `json:"installed"`
	AccountType   string     `json:"account_type,omitempty"` // "free" | "plus" | "imported"
	DeviceID      string     `json:"device_id,omitempty"`
	PublicKey     string     `json:"public_key,omitempty"`
	PeerPublicKey string     `json:"peer_public_key,omitempty"`
	Endpoint      string     `json:"endpoint,omitempty"`
	AddressV4     string     `json:"address_v4,omitempty"`
	AddressV6     string     `json:"address_v6,omitempty"`
	Reserved      []int      `json:"reserved,omitempty"`
	MTU           int        `json:"mtu,omitempty"`
	HasLicense    bool       `json:"has_license,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
	Message       string     `json:"message,omitempty"`
}

// WarpInstallParams 是 warp.install 的参数。
type WarpInstallParams struct {
	LicenseKey string `json:"license_key,omitempty"`
	MTU        int    `json:"mtu,omitempty"`
	Endpoint   string `json:"endpoint,omitempty"`
	APIBase    string `json:"api_base,omitempty"`
	Force      bool   `json:"force,omitempty"`
	TimeoutMS  int    `json:"timeout_ms,omitempty"`
}

// WarpInstallResult 是 warp.install 的回复（不含任何私钥、令牌或 license 明文）。
type WarpInstallResult struct {
	WarpStatusResult
	AlreadyInstalled bool `json:"already_installed,omitempty"`
}

// WarpAuthorizeParams 是 warp.authorize 的参数：支持 WARP+ license 绑定或离线 WireGuard 凭据导入（二选一）。
type WarpAuthorizeParams struct {
	LicenseKey    string `json:"license_key,omitempty"`
	APIBase       string `json:"api_base,omitempty"`
	PrivateKey    string `json:"private_key,omitempty"`
	AddressV4     string `json:"address_v4,omitempty"`
	AddressV6     string `json:"address_v6,omitempty"`
	PeerPublicKey string `json:"peer_public_key,omitempty"`
	Endpoint      string `json:"endpoint,omitempty"`
	Reserved      []int  `json:"reserved,omitempty"`
	MTU           int    `json:"mtu,omitempty"`
	TimeoutMS     int    `json:"timeout_ms,omitempty"`
}

// WarpAuthorizeResult 是 warp.authorize 的回复（不含敏感明文）。
type WarpAuthorizeResult struct {
	WarpStatusResult
	Mode string `json:"mode,omitempty"` // "license" | "import"
}

// WarpRemoveParams 是 warp.remove 的参数。
type WarpRemoveParams struct{}

// WarpRemoveResult 是 warp.remove 的回复。
type WarpRemoveResult struct {
	Removed bool   `json:"removed"`
	Message string `json:"message,omitempty"`
}

// WarpCredentialsResult 是 warp.credentials 的回复：仅供主控内部构造 sing-box WireGuard 出站 Plan 使用。
type WarpCredentialsResult struct {
	Installed     bool   `json:"installed"`
	AccountType   string `json:"account_type,omitempty"`
	PrivateKey    string `json:"private_key"`
	PublicKey     string `json:"public_key,omitempty"`
	PeerPublicKey string `json:"peer_public_key"`
	Endpoint      string `json:"endpoint"`
	AddressV4     string `json:"address_v4"`
	AddressV6     string `json:"address_v6,omitempty"`
	Reserved      []int  `json:"reserved,omitempty"`
	MTU           int    `json:"mtu"`
}
