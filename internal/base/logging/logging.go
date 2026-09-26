// Package logging 是基础设施层的主控日志（master-logs）：建 serve 的 logger（stderr 加数据目录 logs/ 下按大小轮转的文件、
// 属性打码），以及读日志文件——从末尾往前读出若干行、把一行 slog 文本拆开、列出日志文件。谁能看、怎么过滤与分页由业务层决定。
package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/satchel/satchel/internal/base/db"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

const (
	// maxSizeMB 是当前文件轮转前的上限，maxBackups 是轮转下来的旧文件最多留几个（照 mmwx 的 lumberjack 配置）。
	maxSizeMB  = 50
	maxBackups = 3
)

// New 建 serve 的 logger：slog 文本格式，同时写 stderr 与 <dataDir>/logs/satchel.log，级别为 level。
// 返回的 Closer 关掉日志文件。日志文件打不开时返回错误（serve 据此启动失败，不只写 stderr 地跑下去）。
func New(level slog.Level, dataDir string) (*slog.Logger, io.Closer, error) {
	return newWith(level, dataDir, os.Stderr, maxSizeMB)
}

func newWith(level slog.Level, dataDir string, stderr io.Writer, sizeMB int) (*slog.Logger, io.Closer, error) {
	path := filepath.Join(dataDir, db.LogsDir, db.LogFile)
	// lumberjack 第一次写时才打开文件；这里先按 0600 打开一次，把打不开的情况提前报出来。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, v1.Wrap(v1.CodeInternal, "打开日志文件 "+path+" 失败", err)
	}
	_ = f.Close()
	rot := &lumberjack.Logger{Filename: path, MaxSize: sizeMB, MaxBackups: maxBackups}
	h := slog.NewTextHandler(io.MultiWriter(stderr, rot), &slog.HandlerOptions{Level: level, ReplaceAttr: redact})
	return slog.New(h), rot, nil
}

// redact 是日志里的兜底打码：属性名或它所在的任何一层属性组的名字（不分大小写）含 password 或 secret、或以 token 结尾的，
// 值写成 ***。slog 对属性组本身不调 ReplaceAttr，只把组名放进 groups 再对组里的每个属性调，所以组名要在这里一起看。
// token_id 这类编号不以 token 结尾，原样保留。
func redact(groups []string, a slog.Attr) slog.Attr {
	if sensitive(a.Key) {
		return slog.String(a.Key, "***")
	}
	for _, g := range groups {
		if sensitive(g) {
			return slog.String(a.Key, "***")
		}
	}
	return a
}

func sensitive(name string) bool {
	k := strings.ToLower(name)
	return strings.Contains(k, "password") || strings.Contains(k, "secret") || strings.HasSuffix(k, "token")
}
