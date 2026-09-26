// Package logs 是业务层的系统日志查看（master-logs「查看日志行」「查看日志文件」）：logs list 与 logs files list 两条命令，
// 只对管理员开放。读文件与拆行在 base/logging；这里管过滤、分页与不许出日志目录。
package logs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/logging"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// scanLines 是 logs list 从文件末尾扫描的行数上限（照 mmwx）。
const scanLines = 50000

// levels 是 --level 的取值到级别高低的映射；日志行里的级别是大写的同名词。
var levels = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}

// Service 持有日志目录（<数据目录>/logs）。
type Service struct {
	dir string
}

// New 建日志查看服务；dataDir 是数据目录。
func New(dataDir string) *Service {
	return &Service{dir: filepath.Join(dataDir, db.LogsDir)}
}

// Bindings 是 logs * 两条命令的处理函数。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{"logs list": s.list, "logs files list": s.files}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "系统日志只对管理员开放")
	}
	return nil
}

func pageOf(inv *command.Invocation) (command.Page, int, error) {
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	if err := page.Normalize(); err != nil {
		return page, 0, err
	}
	offset, err := command.DecodeOffsetCursor(page.Cursor)
	return page, offset, err
}

// list 是 logs list：从文件末尾 scanLines 行里按级别与文本过滤，从新到旧按偏移量分页。
// 文件一直在追加，两次翻页之间有新日志时页与页会错开几行（design 第 3 条）。
func (s *Service) list(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page, offset, err := pageOf(inv)
	if err != nil {
		return nil, err
	}
	minRank := -1
	if lv := inv.String("level", ""); lv != "" {
		rank, ok := levels[lv]
		if !ok {
			return nil, v1.Newf(v1.CodeBadRequest, "--level 不认识 %q", lv).WithNext("写 debug、info、warn、error 之一")
		}
		minRank = rank
	}
	grep := inv.String("grep", "")
	name, err := s.fileName(inv.String("file", ""))
	if err != nil {
		return nil, err
	}
	lines, err := logging.Tail(filepath.Join(s.dir, name), scanLines)
	if errors.Is(err, os.ErrNotExist) {
		return &command.PageResult{Items: []any{}}, nil
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "读日志文件 "+name+" 失败", err)
	}
	var matched []logging.Entry
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if line == "" || grep != "" && !strings.Contains(line, grep) {
			continue
		}
		e := logging.Parse(line)
		if minRank >= 0 {
			rank, ok := levels[strings.ToLower(e.Level)]
			if !ok || rank < minRank {
				continue
			}
		}
		matched = append(matched, e)
	}
	res := &command.PageResult{Items: []any{}, Total: len(matched)}
	if offset >= len(matched) {
		return res, nil
	}
	end := offset + page.Limit
	if end < len(matched) {
		res.NextCursor = command.EncodeOffsetCursor(end)
	} else {
		end = len(matched)
	}
	for _, e := range matched[offset:end] {
		res.Items = append(res.Items, e)
	}
	return res, nil
}

// fileName 核对 --file：空是当前文件；否则必须是 logging.Files 列出的名字，所以读不到日志目录以外的文件。
func (s *Service) fileName(name string) (string, error) {
	if name == "" {
		return db.LogFile, nil
	}
	files, err := logging.Files(s.dir)
	if err != nil {
		return "", v1.Wrap(v1.CodeInternal, "列日志文件失败", err)
	}
	for _, f := range files {
		if f.Name == name {
			return name, nil
		}
	}
	return "", v1.Newf(v1.CodeNotFound, "没有日志文件 %q", name).WithNext("先用 logs files list 看有哪些文件")
}

// files 是 logs files list：当前文件排第一，其余按修改时间从新到旧；文件不多，一页给完也照走分页。
func (s *Service) files(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page, offset, err := pageOf(inv)
	if err != nil {
		return nil, err
	}
	files, err := logging.Files(s.dir)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "列日志文件失败", err)
	}
	res := &command.PageResult{Items: []any{}, Total: len(files)}
	for i := offset; i < len(files) && i < offset+page.Limit; i++ {
		res.Items = append(res.Items, files[i])
	}
	if offset+page.Limit < len(files) {
		res.NextCursor = command.EncodeOffsetCursor(offset + page.Limit)
	}
	return res, nil
}
