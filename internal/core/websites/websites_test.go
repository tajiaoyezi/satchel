package websites_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/core/websites"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func insertTestServer(t *testing.T, ctx context.Context, st *store.Store, name string) int64 {
	t.Helper()
	srv := &model.Server{
		Name:           name,
		ConnectionMode: "auto",
		Token:          "tok-" + name,
		AgentToken:     "atok-" + name,
		PullToken:      "ptok-" + name,
	}
	if err := st.Insert(ctx, srv); err != nil {
		t.Fatalf("插测试服务器失败：%v", err)
	}
	return srv.ID
}

func TestWebsitesRepoCRUDScanAndUniqueKey(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		repo := websites.New(bdb, st)

		srv1 := insertTestServer(t, ctx, st, "hk-web-1")
		srv2 := insertTestServer(t, ctx, st, "hk-web-2")

		// 1. CreateTx + 唯一键防重名。
		var w1 *websites.Website
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			w1, err = repo.CreateTx(ctx, tx, v1.WebsiteSpec{
				ServerID: srv1, Domain: "App.Example.COM", Type: "proxy", Target: "ws://127.0.0.1:10000/ws",
			})
			return err
		}); err != nil {
			t.Fatalf("创建站点失败：%v", err)
		}
		if w1.Spec.Domain != "app.example.com" || w1.Metadata.ResourceVersion != 1 {
			t.Fatalf("创建站点规范化或版本不对：%+v", w1)
		}

		// 同机同域名拒绝重复创建。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := repo.CreateTx(ctx, tx, v1.WebsiteSpec{
				ServerID: srv1, Domain: "app.example.com", Type: "static",
			})
			return err
		}); err == nil {
			t.Fatal("同机重复域名应当被唯一索引拒绝")
		}

		// 不同服务器允许相同域名。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := repo.CreateTx(ctx, tx, v1.WebsiteSpec{
				ServerID: srv2, Domain: "app.example.com", Type: "decoy", Target: "blog",
			})
			return err
		}); err != nil {
			t.Fatalf("跨服务器同域名应当允许：%v", err)
		}

		// 2. UpdateScanStatus & UpdateManagedStatus 不抬升 resourceVersion。
		if err := repo.UpdateScanStatus(ctx, w1.Metadata.ID, json.RawMessage(`{"listen_ports":["80"]}`), true, "/etc/nginx/servers/satchel-site-app.example.com.conf"); err != nil {
			t.Fatalf("UpdateScanStatus 失败：%v", err)
		}
		got, err := repo.ByID(ctx, w1.Metadata.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Metadata.ResourceVersion != 1 || !got.Status.Managed || got.Status.ConfPath != "/etc/nginx/servers/satchel-site-app.example.com.conf" {
			t.Fatalf("扫盘回写不应抬升版本：%+v", got)
		}

		// 3. UpdateTx 禁止跨 server_id；合法更新抬升版本。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := repo.UpdateTx(ctx, tx, w1.Metadata.ID, 1, false, v1.WebsiteSpec{
				ServerID: srv2, Domain: "app.example.com", Type: "proxy", Target: "http://127.0.0.1:8080",
			})
			return err
		}); err == nil {
			t.Fatal("跨服务器改 server_id 应被拒绝")
		}
		var updated *websites.Website
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			updated, err = repo.UpdateTx(ctx, tx, w1.Metadata.ID, 1, false, v1.WebsiteSpec{
				ServerID: srv1, Domain: "app.example.com", Type: "proxy", Target: "http://127.0.0.1:8080",
			})
			return err
		}); err != nil {
			t.Fatalf("更新站点失败：%v", err)
		}
		if updated.Metadata.ResourceVersion != 2 || updated.Spec.Target != "http://127.0.0.1:8080" {
			t.Fatalf("更新站点结果不对：%+v", updated)
		}

		// 4. SoftDeleteTx 保留自然键占用，RestoreTx 恢复且继续递增版本。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := repo.SoftDeleteTx(ctx, tx, w1.Metadata.ID, 2, false)
			return err
		}); err != nil {
			t.Fatalf("软删除失败：%v", err)
		}
		rows, total, err := repo.List(ctx, websites.ListFilter{ServerID: srv1}, 50, 0)
		if err != nil || total != 0 || len(rows) != 0 {
			t.Fatalf("软删除后默认 List 应为空：total=%d len=%d err=%v", total, len(rows), err)
		}
		rows, total, err = repo.List(ctx, websites.ListFilter{ServerID: srv1, IncludeDeleted: true}, 50, 0)
		if err != nil || total != 1 || len(rows) != 1 || rows[0].Metadata.DeletedAt == nil {
			t.Fatalf("IncludeDeleted 应含已软删行：total=%d len=%d err=%v", total, len(rows), err)
		}

		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := repo.RestoreTx(ctx, tx, w1.Metadata.ID, 3, false)
			return err
		}); err != nil {
			t.Fatalf("恢复站点失败：%v", err)
		}
		restored, err := repo.ByID(ctx, w1.Metadata.ID)
		if err != nil || restored.Metadata.ResourceVersion != 4 || restored.Metadata.DeletedAt != nil {
			t.Fatalf("恢复后状态不对：%+v err=%v", restored, err)
		}
	})
}
