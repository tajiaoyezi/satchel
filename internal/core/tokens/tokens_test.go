package tokens

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func seedUser(t *testing.T, bdb *bun.DB, name string) {
	t.Helper()
	u := &model.User{Username: name, Role: "admin", IsActive: true, PasswordHash: "h", RecoveryCodes: []byte(`[]`),
		NodeSpeedLimitOverrides: []byte(`{}`), NodeDeviceLimitOverrides: []byte(`{}`), CreatedAt: time.Now(), UpdatedAt: time.Now(), ResourceVersion: 1}
	if _, err := bdb.NewInsert().Model(u).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func repo(bdb *bun.DB) *Repo { return New(bdb, store.New(bdb, schema.Default())) }

func mustInsert(t *testing.T, r *Repo, tok *Token, hash string) {
	t.Helper()
	if err := r.Insert(context.Background(), tok, hash); err != nil {
		t.Fatalf("签发 %s 失败：%v", tok.Name, err)
	}
}

func wantCode(t *testing.T, err error, code v1.Code) {
	t.Helper()
	var e *v1.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("想要错误码 %s，得到 %v", code, err)
	}
}

// master-api-tokens「令牌的形状与存储」的仓储部分：插入、按哈希与 id 查、哈希唯一。
func TestInsertAndGet(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := repo(bdb)
		seedUser(t, bdb, "admin")
		exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		tok := &Token{Owner: "admin", Name: "ci", Grant: Grant{Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}, Danger: []v1.Danger{v1.DangerDelete}},
			Preset: "ops", ExpiresAt: &exp, Runtime: "claude-code@laptop"}
		mustInsert(t, r, tok, "hash-1")
		if tok.ID == 0 || tok.CreatedAt.IsZero() {
			t.Fatalf("应当回填 id 与创建时间：%+v", tok)
		}
		got, err := r.GetByHash(ctx, "hash-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != tok.ID || got.Owner != "admin" || got.Name != "ci" || got.Preset != "ops" || got.Runtime != "claude-code@laptop" ||
			len(got.Grant.Scopes) != 2 || len(got.Grant.Danger) != 1 || got.Grant.Danger[0] != v1.DangerDelete || got.Revoked ||
			got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) || got.LastUsedAt != nil {
			t.Fatalf("读回的令牌不对：%+v", got)
		}
		byID, err := r.GetByID(ctx, tok.ID)
		if err != nil || byID.Name != "ci" {
			t.Fatalf("按 id 读：%v %+v", err, byID)
		}
		if _, err := r.GetByHash(ctx, "nosuch"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在的哈希应当 ErrNotFound：%v", err)
		}
		if _, err := r.GetByID(ctx, 424242); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在的 id 应当 ErrNotFound：%v", err)
		}
		// 同一个哈希不能签两次（唯一索引）；没有 runtime 的令牌读回空串。
		wantCode(t, r.Insert(ctx, &Token{Owner: "admin", Name: "dup", Preset: "readonly", Grant: Grant{Scopes: []v1.Scope{v1.ScopeRead}}}, "hash-1"), v1.CodeConflict)
		plain := &Token{Owner: "admin", Name: "plain", Preset: "readonly", Grant: Grant{Scopes: []v1.Scope{v1.ScopeRead}}}
		mustInsert(t, r, plain, "hash-2")
		if got, _ := r.GetByID(ctx, plain.ID); got.Runtime != "" || got.ExpiresAt != nil {
			t.Fatalf("没有 runtime 与过期时间：%+v", got)
		}
		// 签发者必须是存在的账号（外键）。
		if err := r.Insert(ctx, &Token{Owner: "ghost", Name: "x", Preset: "readonly", Grant: Grant{Scopes: []v1.Scope{v1.ScopeRead}}}, "hash-3"); err == nil {
			t.Fatal("不存在的签发者应当被外键拒绝")
		}
	})
}

// 列表的过滤与两种排序、计数。
func TestList(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := repo(bdb)
		seedUser(t, bdb, "admin")
		seedUser(t, bdb, "bob")
		var ids []int64
		for i, spec := range []struct{ owner, runtime string }{{"admin", ""}, {"admin", "codex@a"}, {"bob", "hermes@b"}, {"bob", ""}, {"admin", "claude-code@c"}} {
			tok := &Token{Owner: spec.owner, Name: "t", Preset: "readonly", Grant: Grant{Scopes: []v1.Scope{v1.ScopeRead}}, Runtime: spec.runtime}
			mustInsert(t, r, tok, "h"+string(rune('a'+i)))
			ids = append(ids, tok.ID)
		}
		all, err := r.List(ctx, Filter{}, 10, 0)
		if err != nil || len(all) != 5 || all[0].ID != ids[4] || all[4].ID != ids[0] {
			t.Fatalf("全部按 id 倒序：%v %+v", err, all)
		}
		page, _ := r.List(ctx, Filter{}, 2, all[1].ID)
		if len(page) != 2 || page[0].ID != ids[2] {
			t.Fatalf("keyset 翻页：%+v", page)
		}
		mine, _ := r.List(ctx, Filter{Owner: "bob"}, 10, 0)
		if len(mine) != 2 || mine[0].Owner != "bob" {
			t.Fatalf("按签发者过滤：%+v", mine)
		}
		if n, _ := r.Count(ctx, Filter{RuntimeOnly: true}); n != 3 {
			t.Fatalf("绑了 runtime 的应当 3 把，得到 %d", n)
		}
		// 按最后使用时间：用过的在前（新的在前），没用过的在后（按 id 倒序）。
		now := time.Now().UTC()
		if err := r.TouchLastUsed(ctx, ids[1], now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := r.TouchLastUsed(ctx, ids[2], now); err != nil {
			t.Fatal(err)
		}
		used, err := r.ListByLastUsed(ctx, Filter{RuntimeOnly: true}, 10, 0)
		if err != nil || len(used) != 3 || used[0].ID != ids[2] || used[1].ID != ids[1] || used[2].ID != ids[4] || used[2].LastUsedAt != nil {
			t.Fatalf("按最后使用时间排序：%v %+v", err, used)
		}
		second, _ := r.ListByLastUsed(ctx, Filter{RuntimeOnly: true}, 2, 2)
		if len(second) != 1 || second[0].ID != ids[4] {
			t.Fatalf("偏移翻页：%+v", second)
		}
		// 软删除的行一律当不存在。
		if _, err := bdb.NewUpdate().Model((*model.ApiToken)(nil)).Set("deleted_at = ?", now).Where("id = ?", ids[0]).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if n, _ := r.Count(ctx, Filter{}); n != 4 {
			t.Fatalf("软删除的不算，得到 %d", n)
		}
		if _, err := r.GetByID(ctx, ids[0]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("软删除的按 id 找不到：%v", err)
		}
	})
}

// 改名字与权限范围、吊销、最后使用时间；已吊销的不能再改、不能再吊销。
func TestUpdateAndRevoke(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := repo(bdb)
		seedUser(t, bdb, "admin")
		tok := &Token{Owner: "admin", Name: "ci", Preset: "readonly", Grant: Grant{Scopes: []v1.Scope{v1.ScopeRead}}}
		mustInsert(t, r, tok, "h1")
		tok.Name, tok.Preset = "ci2", "ops"
		tok.Grant = Grant{Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}, Danger: []v1.Danger{}}
		exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		tok.ExpiresAt = &exp
		if err := r.Update(ctx, tok, "name", "preset", "scopes", "expires_at"); err != nil {
			t.Fatal(err)
		}
		got, _ := r.GetByID(ctx, tok.ID)
		if got.Name != "ci2" || got.Preset != "ops" || len(got.Grant.Scopes) != 2 || got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
			t.Fatalf("改后：%+v", got)
		}
		// 清掉过期时间。
		tok.ExpiresAt = nil
		if err := r.Update(ctx, tok, "expires_at"); err != nil {
			t.Fatal(err)
		}
		if got, _ := r.GetByID(ctx, tok.ID); got.ExpiresAt != nil {
			t.Fatalf("过期时间应当清掉：%+v", got)
		}
		if err := r.Update(ctx, tok, "owner"); err == nil {
			t.Fatal("owner 不能经 Update 写")
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		if err := r.Revoke(ctx, tok.ID, at); err != nil {
			t.Fatal(err)
		}
		got, _ = r.GetByID(ctx, tok.ID)
		if !got.Revoked || got.RevokedAt == nil || !got.RevokedAt.Equal(at) {
			t.Fatalf("吊销后：%+v", got)
		}
		wantCode(t, r.Revoke(ctx, tok.ID, at), v1.CodeConflict)
		tok.Name = "again"
		wantCode(t, r.Update(ctx, tok, "name"), v1.CodeConflict)
		if got, _ := r.GetByID(ctx, tok.ID); got.Name != "ci2" {
			t.Fatalf("已吊销的不该被改：%+v", got)
		}
		wantCode(t, r.Revoke(ctx, 424242, at), v1.CodeNotFound)
		// 最后使用时间照样能写（status 档，与吊销无关）。
		if err := r.TouchLastUsed(ctx, tok.ID, at); err != nil {
			t.Fatal(err)
		}
		if got, _ := r.GetByID(ctx, tok.ID); got.LastUsedAt == nil || !got.LastUsedAt.Equal(at) {
			t.Fatalf("最后使用时间：%+v", got)
		}
	})
}
