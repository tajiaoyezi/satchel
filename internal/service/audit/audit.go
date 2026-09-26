// Package audit 是业务层的审计：记一条（横切层的 audit 中间件调）与 audit list 命令（master-audit-log）。
package audit

import (
	"context"
	"time"

	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/audit"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Entry 是要记的一条：除 id 外与 Record 相同。
type Entry struct {
	At         time.Time
	Actor      string
	ActorKind  v1.ActorKind
	TokenID    *int64
	Command    string
	ArgsDigest string
	PlanID     *int64
	Result     string
}

// Retention 是审计记录的保留期（master-scheduler「本站的内置任务」的 audit_cleanup）。
const Retention = 180 * 24 * time.Hour

// Service 持有仓储。
type Service struct {
	repo *core.Repo
}

// New 建服务。
func New(repo *core.Repo) *Service {
	return &Service{repo: repo}
}

// Record 写一条审计记录；At 为零时用当前时间。
func (s *Service) Record(ctx context.Context, e Entry) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	return s.repo.Insert(ctx, core.Record{
		At: e.At, Actor: e.Actor, ActorKind: e.ActorKind, TokenID: e.TokenID,
		Command: e.Command, ArgsDigest: e.ArgsDigest, PlanID: e.PlanID, Result: e.Result,
	})
}

// Prune 删掉保留期以前的审计记录，返回删掉的条数（audit_cleanup 任务调）。
func (s *Service) Prune(ctx context.Context) (int, error) {
	return s.repo.DeleteBefore(ctx, time.Now().Add(-Retention))
}

// List 按时间倒序取一页（keyset 游标是最后一条的 id）。
func (s *Service) List(ctx context.Context, f core.Filter, page command.Page) (*command.PageResult, error) {
	if err := page.Normalize(); err != nil {
		return nil, err
	}
	beforeID, err := command.DecodeIDCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	total, err := s.repo.Count(ctx, f)
	if err != nil {
		return nil, err
	}
	// 多取一条，用来判断有没有下一页。
	rows, err := s.repo.List(ctx, f, page.Limit+1, beforeID)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: make([]any, 0, len(rows)), Total: total}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		res.NextCursor = command.EncodeIDCursor(rows[len(rows)-1].ID)
	}
	for _, r := range rows {
		res.Items = append(res.Items, r)
	}
	return res, nil
}

// ListHandler 是 audit list 命令：只对管理员身份开放，解析 --actor / --command / --since 与分页。
func (s *Service) ListHandler() command.Handler {
	return func(ctx context.Context, inv *command.Invocation) (any, error) {
		if !v1.IdentityFrom(ctx).IsAdmin() {
			return nil, v1.New(v1.CodeForbidden, "审计记录只对管理员开放")
		}
		f := core.Filter{Actor: inv.String("actor", ""), CommandPrefix: inv.String("command", "")}
		if since := inv.String("since", ""); since != "" {
			ts, err := time.Parse(time.RFC3339, since)
			if err != nil {
				return nil, v1.Newf(v1.CodeBadRequest, "参数 since 的值 %s 不是 RFC 3339 时间（如 2026-09-19T00:00:00Z）", since)
			}
			f.Since = &ts
		}
		page := command.Page{}
		if inv.Page != nil {
			page = *inv.Page
		}
		return s.List(ctx, f, page)
	}
}
