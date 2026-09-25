// Package security 是仓储层的安全事件与 IP 封禁：读写 security_events（只追加）与 ip_bans（master-login-protection）。
// 两张表都不是 kind，直接用 bun；模型不出本包。时间一律按 UTC 存，与 SQLite 的文本比较保持一致。
package security

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 安全事件的种类（master-login-protection「安全事件」）：登录与当场验证的四种由 service/auth 写，其余由 service/security 写。
const (
	KindLoginFail    = "login_fail"
	KindLoginLocked  = "login_locked"
	KindVerifyFail   = "verify_fail"
	KindVerifyLocked = "verify_locked"
	KindProbe        = "probe"
	KindBan          = "ban"
	KindBanManual    = "ban_manual"
	KindUnban        = "unban"
)

// 封禁的原因（ip_bans.reason）。
const (
	ReasonBruteForce = "brute_force"
	ReasonManual     = "manual"
)

// Event 是一条安全事件；字段名就是 security events list 输出的字段名。
type Event struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	IP       string    `json:"ip"`
	Kind     string    `json:"kind"`
	Path     string    `json:"path"`
	Username string    `json:"username"`
	Detail   string    `json:"detail"`
	Actor    string    `json:"actor"`
}

// EventFilter 是 security events list 的过滤条件：种类与 IP 都是精确匹配，空表示不过滤。
type EventFilter struct {
	Kind string
	IP   string
}

// Ban 是 ip_bans 的一行；字段名就是 security ban / unban / bans list 输出的字段名。
type Ban struct {
	IP         string     `json:"ip"`
	Reason     string     `json:"reason"`
	BannedAt   time.Time  `json:"banned_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Permanent  bool       `json:"permanent"`
	FailCount  int64      `json:"fail_count"`
	Actor      string     `json:"actor"`
	ReleasedAt *time.Time `json:"released_at"`
}

// Active 报告这条封禁在 now 是否生效：未解封，且永久或未到期。
func (b Ban) Active(now time.Time) bool {
	return b.ReleasedAt == nil && (b.Permanent || (b.ExpiresAt != nil && b.ExpiresAt.After(now)))
}

// ErrNotFound 表示这个 IP 没有生效中的封禁。
var ErrNotFound = errors.New("no active ban")

// Repo 是安全事件与封禁的仓储。
type Repo struct {
	db *bun.DB
}

// New 建仓储。
func New(bdb *bun.DB) *Repo {
	return &Repo{db: bdb}
}

func utc(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := utc(*t)
	return &u
}

// InsertEvent 追加一条事件；At 为零时用当前时间。
func (r *Repo) InsertEvent(ctx context.Context, e Event) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	row := &model.SecurityEvent{At: utc(e.At), IP: e.IP, Kind: e.Kind, Path: e.Path, Username: e.Username, Detail: e.Detail, Actor: e.Actor}
	if _, err := r.db.NewInsert().Model(row).Exec(ctx); err != nil {
		return v1.Wrap(v1.CodeDatabase, "写入安全事件失败", err)
	}
	return nil
}

func applyEventFilter(q *bun.SelectQuery, f EventFilter) *bun.SelectQuery {
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.IP != "" {
		q = q.Where("ip = ?", f.IP)
	}
	return q
}

// ListEvents 按 id 倒序取一页：beforeID 大于 0 时只取 id 小于它的（keyset 翻页），最多 limit 条。
func (r *Repo) ListEvents(ctx context.Context, f EventFilter, limit int, beforeID int64) ([]Event, error) {
	var rows []model.SecurityEvent
	q := applyEventFilter(r.db.NewSelect().Model(&rows), f).OrderExpr("id DESC").Limit(limit)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取安全事件失败", err)
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		out = append(out, Event{ID: row.ID, At: row.At, IP: row.IP, Kind: row.Kind, Path: row.Path, Username: row.Username, Detail: row.Detail, Actor: row.Actor})
	}
	return out, nil
}

// CountEvents 数过滤条件下的总条数。
func (r *Repo) CountEvents(ctx context.Context, f EventFilter) (int, error) {
	n, err := applyEventFilter(r.db.NewSelect().Model((*model.SecurityEvent)(nil)), f).Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计安全事件失败", err)
	}
	return n, nil
}

// UpsertBan 写一条封禁：同一 IP 已有行就整行覆盖（例如改成永久），并清掉解封时间。
func (r *Repo) UpsertBan(ctx context.Context, b Ban) error {
	row := &model.IPBan{IP: b.IP, Reason: b.Reason, BannedAt: utc(b.BannedAt), ExpiresAt: utcPtr(b.ExpiresAt), Permanent: b.Permanent, FailCount: b.FailCount, Actor: b.Actor}
	_, err := r.db.NewInsert().Model(row).
		On("CONFLICT (ip) DO UPDATE").
		Set("reason = EXCLUDED.reason").
		Set("banned_at = EXCLUDED.banned_at").
		Set("expires_at = EXCLUDED.expires_at").
		Set("permanent = EXCLUDED.permanent").
		Set("fail_count = EXCLUDED.fail_count").
		Set("released_at = NULL").
		Set("actor = EXCLUDED.actor").
		Exec(ctx)
	if err != nil {
		return v1.Wrap(v1.CodeDatabase, "写入 IP 封禁失败", err)
	}
	return nil
}

// active 限定生效中的封禁：未解封，且永久或未到期。
func active(q *bun.SelectQuery, now time.Time) *bun.SelectQuery {
	return q.Where("released_at IS NULL").WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("permanent = ?", true).WhereOr("expires_at > ?", utc(now))
	})
}

func banOf(row model.IPBan) Ban {
	return Ban{IP: row.IP, Reason: row.Reason, BannedAt: row.BannedAt, ExpiresAt: row.ExpiresAt, Permanent: row.Permanent,
		FailCount: row.FailCount, Actor: row.Actor, ReleasedAt: row.ReleasedAt}
}

// GetActiveBan 读这个 IP 生效中的封禁；没有（从没封过、已解封、已到期）返回 ErrNotFound。
func (r *Repo) GetActiveBan(ctx context.Context, ip string, now time.Time) (*Ban, error) {
	var row model.IPBan
	err := active(r.db.NewSelect().Model(&row).Where("ip = ?", ip), now).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取 IP 封禁失败", err)
	}
	b := banOf(row)
	return &b, nil
}

// ReleaseBan 把这个 IP 生效中的封禁标为已解封（写解封时间与操作者），返回解封后的那一行；没有生效中的封禁返回 ErrNotFound。
func (r *Repo) ReleaseBan(ctx context.Context, ip, actor string, now time.Time) (*Ban, error) {
	var out *Ban
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var row model.IPBan
		err := active(tx.NewSelect().Model(&row).Where("ip = ?", ip), now).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return v1.Wrap(v1.CodeDatabase, "读取 IP 封禁失败", err)
		}
		released := utc(now)
		if _, err := tx.NewUpdate().Model((*model.IPBan)(nil)).Set("released_at = ?", released).Set("actor = ?", actor).
			Where("ip = ?", ip).Exec(ctx); err != nil {
			return v1.Wrap(v1.CodeDatabase, "解除 IP 封禁失败", err)
		}
		row.ReleasedAt, row.Actor = &released, actor
		b := banOf(row)
		out = &b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListActiveBans 按封禁时间倒序（同一时刻按 IP）取生效中的封禁的一页：跳过前 offset 条，最多 limit 条。
func (r *Repo) ListActiveBans(ctx context.Context, now time.Time, limit, offset int) ([]Ban, error) {
	var rows []model.IPBan
	q := active(r.db.NewSelect().Model(&rows), now).OrderExpr("banned_at DESC, ip ASC").Limit(limit).Offset(offset)
	if err := q.Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取 IP 封禁失败", err)
	}
	out := make([]Ban, 0, len(rows))
	for _, row := range rows {
		out = append(out, banOf(row))
	}
	return out, nil
}

// CountActiveBans 数生效中的封禁。
func (r *Repo) CountActiveBans(ctx context.Context, now time.Time) (int, error) {
	n, err := active(r.db.NewSelect().Model((*model.IPBan)(nil)), now).Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计 IP 封禁失败", err)
	}
	return n, nil
}

// ActiveBans 取全部生效中的封禁：主控启动时灌回内存，重启不解封。
func (r *Repo) ActiveBans(ctx context.Context, now time.Time) ([]Ban, error) {
	var rows []model.IPBan
	if err := active(r.db.NewSelect().Model(&rows), now).OrderExpr("ip ASC").Scan(ctx); err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取 IP 封禁失败", err)
	}
	out := make([]Ban, 0, len(rows))
	for _, row := range rows {
		out = append(out, banOf(row))
	}
	return out, nil
}
