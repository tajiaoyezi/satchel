package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/base/db"
)

// Format 是备份清单的格式版本。
const Format = "satchel-backup-v1"

// ZIP 里的固定条目（master-backup「备份的内容与格式」）。
const (
	EntryManifest = "manifest.json"
	EntrySQLite   = "database/satchel.db"
	EntryPostgres = "database/postgres.sql"
)

// 备份文件名的前缀：本机生成的、上传来的、恢复前自动生成的、自升级前生成的。
const (
	PrefixBackup        = "satchel-backup-"
	PrefixUploaded      = "uploaded-"
	PrefixBeforeRestore = "before-restore-"
	PrefixBeforeUpgrade = "before-upgrade-"
)

// Keep 是 backups/ 里最多留的 .zip 份数。
const Keep = 7

// Manifest 是 manifest.json 的内容。
type Manifest struct {
	Format     string    `json:"format"`
	CreatedAt  time.Time `json:"created_at"`
	Driver     db.Driver `json:"driver"`
	Migrations []string  `json:"migrations"`
	Version    string    `json:"version"`
	Schema     string    `json:"schema,omitempty"`
}

// Info 是 backups/ 里的一份备份；字段名就是 backup list 的输出字段名。CreatedAt 与 Driver 取自清单，读不出时为空。
type Info struct {
	Name      string     `json:"name"`
	Size      int64      `json:"size"`
	CreatedAt *time.Time `json:"created_at"`
	Driver    db.Driver  `json:"driver"`
}

// Dir 是数据目录下的本机备份目录。
func Dir(dataDir string) string { return filepath.Join(dataDir, db.BackupsDir) }

// dataFiles 是进备份的数据目录里的单个文件，dataDirs 是整个进备份的子目录（其余的一律不进）。
var (
	dataFiles = []string{db.ConfigFile, db.ConfigYAMLFile, db.MasterKeyFile}
	dataDirs  = []string{db.SubscribesDir, db.RuleTemplatesDir}
)

// restoredFiles 与 restoredDirs 是恢复时换掉的：database.json 与 config.yaml 保持当前的（design 第 7 条）。
var (
	restoredFiles = []string{db.MasterKeyFile}
	restoredDirs  = dataDirs
)

// NewName 按前缀与时间（UTC，精确到秒）取名；同一秒里已有同名的就加 -2、-3。
func NewName(dir, prefix string, at time.Time) string {
	base := prefix + at.UTC().Format("20060102T150405Z")
	name := base + ".zip"
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name
		}
		name = fmt.Sprintf("%s-%d.zip", base, i)
	}
}

// entryAllowed 报告 ZIP 里的一个条目名是不是允许出现的几项之一（master-backup「备份的校验」）。
func entryAllowed(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, ":") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return false
		}
	}
	switch name {
	case EntryManifest, EntrySQLite, EntryPostgres:
		return true
	}
	for _, f := range dataFiles {
		if name == f {
			return true
		}
	}
	for _, d := range dataDirs {
		if strings.HasPrefix(name, d+"/") {
			return true
		}
	}
	return false
}
