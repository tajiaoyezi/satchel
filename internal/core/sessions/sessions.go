// Package sessions 是仓储层的网页会话：只存令牌的哈希、用户名与过期时间（master-web-session）。表不是 kind，直接用 bun。
package sessions

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Session 是一条会话。
type Session struct {
	TokenHash string
	Username  string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Repo 是会话仓储。
type Repo struct {
	db *bun.DB
}

// New 建仓储。
func New(bdb *bun.DB) *Repo {
	return &Repo{db: bdb}
}

// ErrNotFound 表示没有这条会话。
var ErrNotFound = errors.New("session not found")

// Insert 写一条会话。
func (r *Repo) Insert(ctx context.Context, s Session) error {
	row := &model.Session{TokenHash: s.TokenHash, Username: s.Username, ExpiresAt: s.ExpiresAt.UTC(), CreatedAt: s.CreatedAt.UTC()}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	if _, err := r.db.NewInsert().Model(row).Exec(ctx); err != nil {
		return v1.Wrap(v1.CodeDatabase, "写入会话失败", err)
	}
	return nil
}

// GetByHash 按令牌哈希读会话；没有返回 ErrNotFound（过期的照样返回，调用方看 ExpiresAt）。
func (r *Repo) GetByHash(ctx context.Context, hash string) (*Session, error) {
	var row model.Session
	err := r.db.NewSelect().Model(&row).Where("token_hash = ?", hash).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取会话失败", err)
	}
	return &Session{TokenHash: row.TokenHash, Username: row.Username, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt}, nil
}

// Delete 删一条会话；不存在不算错。
func (r *Repo) Delete(ctx context.Context, hash string) error {
	if _, err := r.db.NewDelete().Model((*model.Session)(nil)).Where("token_hash = ?", hash).Exec(ctx); err != nil {
		return v1.Wrap(v1.CodeDatabase, "删除会话失败", err)
	}
	return nil
}

// DeleteByUser 删一个用户的全部会话；exceptHash 非空时留下那一条（改密码时保留当前会话）。
func (r *Repo) DeleteByUser(ctx context.Context, username, exceptHash string) (int, error) {
	q := r.db.NewDelete().Model((*model.Session)(nil)).Where("username = ?", username)
	if exceptHash != "" {
		q = q.Where("token_hash <> ?", exceptHash)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "删除会话失败", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteExpired 清掉过期的会话，返回条数。
func (r *Repo) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	res, err := r.db.NewDelete().Model((*model.Session)(nil)).Where("expires_at < ?", now.UTC()).Exec(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "清理过期会话失败", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CountByUser 数一个用户未过期的会话。
func (r *Repo) CountByUser(ctx context.Context, username string, now time.Time) (int, error) {
	n, err := r.db.NewSelect().Model((*model.Session)(nil)).Where("username = ?", username).Where("expires_at >= ?", now.UTC()).Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计会话失败", err)
	}
	return n, nil
}
