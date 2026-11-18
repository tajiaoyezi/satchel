package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/satchel/satchel/internal/base/db"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/release"
)

// InDocker 判断是否在 Docker 容器里（master-self-update「应用升级的前提」）。
// root 在产品里是 "/"，测试换成临时目录。
func InDocker(root string) bool { return release.InDocker(root) }

// 升级标记的阶段（design 第 7 条）。
const (
	PhaseSwitching   = "switching"
	PhaseRollingBack = "rolling_back"
	// PhaseCommitted：新版本的健康检查已经通过，只剩写 job 的结局与删标记；之后的启动只把收尾做完，不再计启动次数、不回退。
	PhaseCommitted = "committed"
)

// Marker 是数据目录里的升级标记 upgrade-pending.json：在库之外，因为失败时库要被换回。
type Marker struct {
	JobID       string    `json:"job_id"`
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	Channel     string    `json:"channel"`
	Backup      string    `json:"backup"`
	Target      string    `json:"target"`   // 目标路径：正在运行的二进制解开符号链接后的路径
	Previous    string    `json:"previous"` // 上一版二进制：目标路径加 .bak
	Actor       string    `json:"actor"`
	RequestedAt time.Time `json:"requested_at"`
	Phase       string    `json:"phase"`
	Attempts    int       `json:"attempts"`
	Error       string    `json:"error,omitempty"` // 回退的原因
}

func markerPath(dataDir string) string { return filepath.Join(dataDir, db.UpgradePendingFile) }

// ReadMarker 读升级标记；没有时返回 nil。
func ReadMarker(dataDir string) (*Marker, error) {
	raw, err := os.ReadFile(markerPath(dataDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "读升级标记失败", err)
	}
	var m Marker
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "升级标记 "+db.UpgradePendingFile+" 不是合法的 JSON", err).
			WithNext("检查或删掉数据目录里的 " + db.UpgradePendingFile)
	}
	return &m, nil
}

// WriteMarker 写升级标记（0600）：先写临时文件、落盘，再改名，再把目录落盘。
func WriteMarker(dataDir string, m *Marker) error {
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := WriteFileAtomic(dataDir, markerPath(dataDir), raw, 0o600); err != nil {
		return v1.Wrap(v1.CodeInternal, "写升级标记失败", err)
	}
	return nil
}

// RemoveMarker 删掉升级标记并把目录落盘。
func RemoveMarker(dataDir string) error {
	if err := os.Remove(markerPath(dataDir)); os.IsNotExist(err) {
		return nil // 本来就没有：没有目录项要落盘
	} else if err != nil {
		return v1.Wrap(v1.CodeInternal, "删除升级标记失败", err)
	}
	if err := release.SyncDir(dataDir); err != nil {
		return v1.Wrap(v1.CodeInternal, "删除升级标记失败", err)
	}
	return nil
}

// PreviousPath 是目标路径的上一版二进制。
func PreviousPath(target string) string { return release.PreviousPath(target) }

// SavePrevious 把目标路径上的二进制复制成 .bak（落盘）。
func SavePrevious(target string) error { return release.SavePrevious(target) }

// Install 把 staged（与目标同目录的临时文件）改名到 target。
func Install(staged, target string) (replaced bool, err error) {
	return release.Install(staged, target)
}

// RestorePrevious 把 .bak 复制回目标路径。
func RestorePrevious(target string) error { return release.RestorePrevious(target) }

// WriteFileAtomic 先在同一目录写临时文件并落盘，再改名到 path 并把目录落盘。
func WriteFileAtomic(dir, path string, data []byte, mode os.FileMode) error {
	return release.WriteFileAtomic(dir, path, data, mode)
}
