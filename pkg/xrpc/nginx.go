package xrpc

import "time"

// SatchelManagedHeader 是所有 Satchel 纳管 nginx 配置文件首行统一标记前缀（用于防止覆盖或删除手工未纳管配置）。
const SatchelManagedHeader = "# satchel-managed:"

// SatchelMaster443Marker 是主控同机 443 stream 配置保护标记。
const SatchelMaster443Marker = "# satchel-master-443"

// KernelServiceStatus 描述节点内嵌 sing-box 内核服务状态。
type KernelServiceStatus struct {
	Running   bool       `json:"running"`
	Version   string     `json:"version,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

// NginxServiceStatus 描述节点 nginx 服务状态。
type NginxServiceStatus struct {
	Installed  bool   `json:"installed"`
	Running    bool   `json:"running"`
	Version    string `json:"version,omitempty"`
	InDocker   bool   `json:"in_docker"`
	InitSystem string `json:"init_system,omitempty"` // "systemd" | "openrc" | "direct" | ""
	ConfigOK   bool   `json:"config_ok"`
	Error      string `json:"error,omitempty"`
}

// AgentServiceStatus 描述节点 satchel-agent 进程自身状态。
type AgentServiceStatus struct {
	Running       bool   `json:"running"`
	Version       string `json:"version,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	InDocker      bool   `json:"in_docker"`
}

// ServiceStatusResult 是 service.status 的回复。
type ServiceStatusResult struct {
	Kernel    KernelServiceStatus `json:"kernel"`
	Nginx     NginxServiceStatus  `json:"nginx"`
	Agent     AgentServiceStatus  `json:"agent"`
	CheckedAt time.Time           `json:"checked_at"`
}

// ServiceControlParams 是 service.control 的参数。
type ServiceControlParams struct {
	Service string `json:"service"` // "kernel" | "nginx"
	Action  string `json:"action"`  // "start" | "stop" | "restart" | "reload"
}

// ServiceControlResult 是 service.control 的回复。
type ServiceControlResult struct {
	Service string              `json:"service"`
	Action  string              `json:"action"`
	Running bool                `json:"running"`
	Message string              `json:"message,omitempty"`
	Status  ServiceStatusResult `json:"status"`
}

// NginxInstallParams 是 nginx.install 的参数（保留结构体便于未来扩展）。
type NginxInstallParams struct{}

// NginxInstallResult 是 nginx.install 的回复。
type NginxInstallResult struct {
	Installed        bool   `json:"installed"`
	AlreadyInstalled bool   `json:"already_installed"`
	InDocker         bool   `json:"in_docker"`
	Version          string `json:"version,omitempty"`
	PackageManager   string `json:"package_manager,omitempty"`
	Message          string `json:"message,omitempty"`
}

// NginxUninstallParams 是 nginx.uninstall 的参数。
type NginxUninstallParams struct{}

// NginxUninstallResult 是 nginx.uninstall 的回复。
type NginxUninstallResult struct {
	Uninstalled bool   `json:"uninstalled"`
	InDocker    bool   `json:"in_docker"`
	Message     string `json:"message,omitempty"`
}

// NginxServerBlock 是 nginx.servers 返回的单个 HTTP 或 stream server 块摘要。
type NginxServerBlock struct {
	Kind        string   `json:"kind"` // "http" | "stream"
	ConfPath    string   `json:"conf_path"`
	Managed     bool     `json:"managed"`
	ServerNames []string `json:"server_names"`
	ListenPorts []string `json:"listen_ports"`
	ProxyPass   string   `json:"proxy_pass,omitempty"`
	Root        string   `json:"root,omitempty"`
	HasSSL      bool     `json:"has_ssl"`
}

// NginxServersResult 是 nginx.servers 的回复。
type NginxServersResult struct {
	Servers []NginxServerBlock `json:"servers"`
}

// NginxClearStreamParams 是 nginx.clear_stream 的参数。
type NginxClearStreamParams struct {
	Port             int  `json:"port"`
	ProtectMaster443 bool `json:"protect_master_443,omitempty"`
}

// NginxClearStreamResult 是 nginx.clear_stream 的回复。
type NginxClearStreamResult struct {
	Port          int      `json:"port"`
	RemovedFiles  []string `json:"removed_files"`
	Reloaded      bool     `json:"reloaded"`
	RolledBack    bool     `json:"rolled_back,omitempty"`
	RollbackError string   `json:"rollback_error,omitempty"`
	Message       string   `json:"message,omitempty"`
}

// ScannedWebsite 是 nginx.websites.scan 扫出的单个磁盘站点。
type ScannedWebsite struct {
	Domain      string   `json:"domain"`
	ConfPath    string   `json:"conf_path"`
	Managed     bool     `json:"managed"`
	Type        string   `json:"type"` // "static" | "proxy" | "unknown"
	Target      string   `json:"target,omitempty"`
	ListenPorts []string `json:"listen_ports"`
	HasSSL      bool     `json:"has_ssl"`
	Root        string   `json:"root,omitempty"`
	ProxyPass   string   `json:"proxy_pass,omitempty"`
}

// NginxWebsitesScanResult 是 nginx.websites.scan 的回复。
type NginxWebsitesScanResult struct {
	Sites []ScannedWebsite `json:"sites"`
}

// ManagedWebsiteFile 是 nginx.websites.apply 中主控下发的单条受管站点配置与可选静态默认页。
type ManagedWebsiteFile struct {
	Domain      string `json:"domain"`
	Conf        string `json:"conf"`
	HTMLPath    string `json:"html_path,omitempty"`
	HTMLContent string `json:"html_content,omitempty"`
}

// NginxWebsitesApplyParams 是 nginx.websites.apply 的参数。
type NginxWebsitesApplyParams struct {
	Sites            []ManagedWebsiteFile `json:"sites"`
	RemoveDomains    []string             `json:"remove_domains,omitempty"`
	ProtectMaster443 bool                 `json:"protect_master_443,omitempty"`
	StealSite        *ManagedWebsiteFile  `json:"steal_site,omitempty"`
	RemoveSteal      bool                 `json:"remove_steal,omitempty"`
}

// AppliedWebsiteFile 记录单条站点配置实际落盘路径。
type AppliedWebsiteFile struct {
	Domain   string `json:"domain"`
	ConfPath string `json:"conf_path"`
}

// NginxWebsitesApplyResult 是 nginx.websites.apply 的回复。
type NginxWebsitesApplyResult struct {
	Applied       []AppliedWebsiteFile `json:"applied"`
	Removed       []string             `json:"removed"`
	Reloaded      bool                 `json:"reloaded"`
	RolledBack    bool                 `json:"rolled_back,omitempty"`
	RollbackError string               `json:"rollback_error,omitempty"`
	Error         string               `json:"error,omitempty"`
	StealApplied  bool                 `json:"steal_applied,omitempty"`
	StealRemoved  bool                 `json:"steal_removed,omitempty"`
	StealConfPath string               `json:"steal_conf_path,omitempty"`
}
