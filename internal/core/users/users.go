// Package users 是仓储层的用户：读账号、建第一个管理员、写人类专属的几列（密码哈希、两步验证密钥与开关、恢复码）。
// 用户的增删改查是 M3 的事，这里只有身份与账号命令要的那几样。模型不出本包。
package users

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Account 是账号的领域结构：身份判定与账号命令用到的字段。
type Account struct {
	ID            int64
	Username      string
	Email         string
	Nickname      string
	Role          v1.Role
	IsActive      bool
	PasswordHash  string
	TOTPSecret    string
	TOTPEnabled   bool
	RecoveryCodes []string // 哈希清单
	Deleted       bool
}

// Repo 是用户仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// ErrNotFound 表示没有这个账号。
var ErrNotFound = errors.New("account not found")

// GetByUsername 按用户名读账号（含已软删除的，调用方看 Deleted）。
func (r *Repo) GetByUsername(ctx context.Context, username string) (*Account, error) {
	var row model.User
	err := r.db.NewSelect().Model(&row).Where("username = ?", username).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取账号失败", err)
	}
	return toAccount(&row)
}

func toAccount(row *model.User) (*Account, error) {
	a := &Account{
		ID: row.ID, Username: row.Username, Role: v1.Role(row.Role), IsActive: row.IsActive,
		PasswordHash: row.PasswordHash, TOTPSecret: row.TotpSecret, TOTPEnabled: row.TotpEnabled, Deleted: row.DeletedAt != nil,
	}
	if row.Email != nil {
		a.Email = *row.Email
	}
	if row.Nickname != nil {
		a.Nickname = *row.Nickname
	}
	if len(row.RecoveryCodes) > 0 {
		if err := json.Unmarshal(row.RecoveryCodes, &a.RecoveryCodes); err != nil {
			return nil, v1.Wrap(v1.CodeDatabase, "账号的恢复码清单不是合法 JSON", err)
		}
	}
	if a.RecoveryCodes == nil {
		a.RecoveryCodes = []string{}
	}
	return a, nil
}

// Count 数账号（含已软删除的：初始化向导只看库里有没有过用户）。
func (r *Repo) Count(ctx context.Context) (int, error) {
	n, err := r.db.NewSelect().Model((*model.User)(nil)).Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计账号失败", err)
	}
	return n, nil
}

// initLockKey 是 PostgreSQL 上串行化 InitAdmin 的 advisory lock 键（固定常量）。
const initLockKey = 0x5a7c4e1

// InitAdmin 在事务里「数用户为 0 才插入」建第一个管理员；已有用户返回 conflict。
// SQLite 的 BEGIN IMMEDIATE 已把写事务串行化；PostgreSQL 用事务级 advisory lock 串行化（空表上 FOR UPDATE 锁不住任何行）。
func (r *Repo) InitAdmin(ctx context.Context, username, passwordHash, email string) (*Account, error) {
	var created *Account
	err := r.db.RunInTx(ctx, &sql.TxOptions{}, func(ctx context.Context, tx bun.Tx) error {
		if db.DialectOf(r.db) == schema.Postgres {
			if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", initLockKey); err != nil {
				return v1.Wrap(v1.CodeDatabase, "初始化加锁失败", err)
			}
		}
		n, err := tx.NewSelect().Model((*model.User)(nil)).Count(ctx)
		if err != nil {
			return v1.Wrap(v1.CodeDatabase, "统计账号失败", err)
		}
		if n > 0 {
			return v1.New(v1.CodeConflict, "主控已经初始化过，库里已有用户，不能再建第一个管理员").
				WithNext("用已有的管理员账号登录；忘了密码在主控本机执行 satchel admin reset-password")
		}
		row := &model.User{
			Username: username, Role: string(v1.RoleAdmin), IsActive: true, PasswordHash: passwordHash,
			RecoveryCodes: json.RawMessage(`[]`), NodeSpeedLimitOverrides: json.RawMessage(`{}`), NodeDeviceLimitOverrides: json.RawMessage(`{}`),
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), ResourceVersion: 1,
		}
		if email != "" {
			row.Email = &email
		}
		if _, err := tx.NewInsert().Model(row).Exec(ctx); err != nil {
			return v1.Wrap(v1.CodeDatabase, "创建管理员失败", err)
		}
		created, err = toAccount(row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// SetPasswordHash 改密码哈希（人类专属列）。
func (r *Repo) SetPasswordHash(ctx context.Context, id int64, hash string) error {
	return r.store.UpdateHuman(ctx, &model.User{ID: id, PasswordHash: hash}, []string{"password_hash"})
}

// SetPendingTOTP 存一把未启用的 TOTP 密钥：totp_secret 有值、totp_enabled 假、恢复码清空。
func (r *Repo) SetPendingTOTP(ctx context.Context, id int64, secret string) error {
	return r.store.UpdateHuman(ctx, &model.User{ID: id, TotpSecret: secret, TotpEnabled: false, RecoveryCodes: json.RawMessage(`[]`)},
		[]string{"totp_secret", "totp_enabled", "recovery_codes"})
}

// EnableTOTP 启用两步验证并写入恢复码哈希清单；前置条件是密钥仍是 secret 且尚未启用。
func (r *Repo) EnableTOTP(ctx context.Context, id int64, secret string, codeHashes []string) error {
	raw, _ := json.Marshal(codeHashes)
	return r.store.UpdateHuman(ctx, &model.User{ID: id, TotpEnabled: true, RecoveryCodes: raw}, []string{"totp_enabled", "recovery_codes"},
		store.Cond{Column: "totp_secret", Value: secret}, store.Cond{Column: "totp_enabled", Value: false})
}

// DisableTOTP 关闭两步验证：清密钥、关开关、清恢复码。
func (r *Repo) DisableTOTP(ctx context.Context, id int64) error {
	return r.store.UpdateHuman(ctx, &model.User{ID: id, TotpSecret: "", TotpEnabled: false, RecoveryCodes: json.RawMessage(`[]`)},
		[]string{"totp_secret", "totp_enabled", "recovery_codes"})
}

// SetRecoveryCodes 用新清单替换恢复码；expectedOld 非 nil 时以「当前清单等于 expectedOld」为前置条件，不满足报 conflict——
// 消耗一枚恢复码就是这样做的：读出清单、去掉那一枚、带前置条件写回，并发的第二个写者会因为清单已变而失败。
func (r *Repo) SetRecoveryCodes(ctx context.Context, id int64, codes []string, expectedOld []string) error {
	raw, _ := json.Marshal(codes)
	model := &model.User{ID: id, RecoveryCodes: raw}
	if expectedOld == nil {
		return r.store.UpdateHuman(ctx, model, []string{"recovery_codes"})
	}
	oldRaw, _ := json.Marshal(expectedOld)
	return r.store.UpdateHuman(ctx, model, []string{"recovery_codes"}, store.Cond{Column: "recovery_codes", Value: string(oldRaw)})
}
