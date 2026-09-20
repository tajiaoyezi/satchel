package users

import (
	"context"
	"sync"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func repo(t *testing.T, bdb *bun.DB) *Repo {
	t.Helper()
	return New(bdb, store.New(bdb, schema.Default()))
}

// master-setup-wizard「setup init」：空库建管理员，已有用户 conflict，并发只成功一个。
func TestInitAdmin(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := repo(t, bdb)
		ctx := context.Background()
		if n, _ := r.Count(ctx); n != 0 {
			t.Fatalf("空库应当 0 个用户，得到 %d", n)
		}
		a, err := r.InitAdmin(ctx, "admin", "$2a$10$hash", "a@b.c")
		if err != nil {
			t.Fatal(err)
		}
		if a.Role != v1.RoleAdmin || !a.IsActive || a.Username != "admin" || a.Email != "a@b.c" || a.ID == 0 || len(a.RecoveryCodes) != 0 {
			t.Fatalf("管理员账号不对：%+v", a)
		}
		got, err := r.GetByUsername(ctx, "admin")
		if err != nil || got.PasswordHash != "$2a$10$hash" || got.TOTPEnabled || got.Deleted {
			t.Fatalf("读回的账号不对：%+v %v", got, err)
		}
		if _, err := r.InitAdmin(ctx, "another", "h", ""); err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("已有用户应当 conflict：%v", err)
		}
		if _, err := r.GetByUsername(ctx, "nobody"); err != ErrNotFound {
			t.Fatalf("不存在应当 ErrNotFound：%v", err)
		}
	})
}

func TestInitAdminConcurrent(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := repo(t, bdb)
		var wg sync.WaitGroup
		results := make(chan error, 10)
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := r.InitAdmin(context.Background(), "admin"+string(rune('a'+i)), "h", "")
				results <- err
			}(i)
		}
		wg.Wait()
		close(results)
		ok := 0
		for err := range results {
			if err == nil {
				ok++
				continue
			}
			if c := v1.AsError(err).Code; c != v1.CodeConflict && c != v1.CodeNameTaken {
				t.Errorf("失败的应当是 conflict 或 name_taken：%v", err)
			}
		}
		if ok != 1 {
			t.Fatalf("应当恰好一个成功，得到 %d", ok)
		}
		if n, _ := r.Count(context.Background()); n != 1 {
			t.Fatalf("库里应当恰好 1 个用户，得到 %d", n)
		}
	})
}

// 人类专属列的写入原语与恢复码的前置条件。
func TestHumanColumns(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := repo(t, bdb)
		ctx := context.Background()
		a, err := r.InitAdmin(ctx, "admin", "h1", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SetPasswordHash(ctx, a.ID, "h2"); err != nil {
			t.Fatal(err)
		}
		if err := r.SetPendingTOTP(ctx, a.ID, "SECRET"); err != nil {
			t.Fatal(err)
		}
		got, _ := r.GetByUsername(ctx, "admin")
		if got.PasswordHash != "h2" || got.TOTPSecret != "SECRET" || got.TOTPEnabled {
			t.Fatalf("待启用状态不对：%+v", got)
		}
		if err := r.EnableTOTP(ctx, a.ID, "OTHER", []string{"x"}); err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("密钥不匹配应当 conflict：%v", err)
		}
		if err := r.EnableTOTP(ctx, a.ID, "SECRET", []string{"c1", "c2", "c3"}); err != nil {
			t.Fatal(err)
		}
		if err := r.EnableTOTP(ctx, a.ID, "SECRET", []string{"again"}); err == nil {
			t.Fatal("已启用再启用应当 conflict")
		}
		got, _ = r.GetByUsername(ctx, "admin")
		if !got.TOTPEnabled || len(got.RecoveryCodes) != 3 {
			t.Fatalf("启用后不对：%+v", got)
		}
		// 消耗一枚：带旧清单做前置条件；旧清单不对时 conflict。
		if err := r.SetRecoveryCodes(ctx, a.ID, []string{"c2", "c3"}, []string{"c1", "c2", "c3"}); err != nil {
			t.Fatal(err)
		}
		if err := r.SetRecoveryCodes(ctx, a.ID, []string{"c3"}, []string{"c1", "c2", "c3"}); err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("旧清单已变应当 conflict：%v", err)
		}
		got, _ = r.GetByUsername(ctx, "admin")
		if len(got.RecoveryCodes) != 2 || got.RecoveryCodes[0] != "c2" {
			t.Fatalf("消耗后清单不对：%v", got.RecoveryCodes)
		}
		if err := r.SetRecoveryCodes(ctx, a.ID, []string{"n1"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := r.DisableTOTP(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		got, _ = r.GetByUsername(ctx, "admin")
		if got.TOTPEnabled || got.TOTPSecret != "" || len(got.RecoveryCodes) != 0 {
			t.Fatalf("禁用后不对：%+v", got)
		}
	})
}
