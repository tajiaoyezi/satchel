package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
)

func insertTestServer(t *testing.T, bdb *bun.DB, name string) int64 {
	t.Helper()
	st := store.New(bdb, schema.Default())
	srv := &model.Server{
		Name: name, ConnectionMode: "auto", Token: "t-" + name, AgentToken: "a-" + name, PullToken: "p-" + name,
		Status: "connected", CoreLogLevel: "warn", CoreDNS: json.RawMessage(`{}`),
	}
	if err := st.Insert(context.Background(), srv); err != nil {
		t.Fatal(err)
	}
	return srv.ID
}

func TestEvidenceRepoCRUDAndPrune(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		repo := New(bdb, store.New(bdb, schema.Default()))
		s1 := insertTestServer(t, bdb, "ev-s1")
		s2 := insertTestServer(t, bdb, "ev-s2")

		now := time.Now().UTC().Truncate(time.Second)
		past := now.Add(-time.Hour)
		future := now.Add(24 * time.Hour)
		cerr := "node offline"

		p1, err := repo.Insert(ctx, CreateInput{
			Category: "general", ServerID: &s1, CollectedAt: now.Add(-2 * time.Hour),
			Items: json.RawMessage(`{"data_only":true}`), SizeBytes: 18, ExpiresAt: &past,
		})
		if err != nil {
			t.Fatal(err)
		}
		p2, err := repo.Insert(ctx, CreateInput{
			Category: "offline", ServerID: &s2, CollectedAt: now,
			Items: json.RawMessage(`{"data_only":true,"offline":true}`), CollectError: &cerr, SizeBytes: 33, ExpiresAt: &future,
		})
		if err != nil {
			t.Fatal(err)
		}

		got, err := repo.Get(ctx, p2.Metadata.ID)
		if err != nil || got.Spec.Category != "offline" || got.Status.CollectError == nil || *got.Status.CollectError != cerr {
			t.Fatalf("Get 异常：%+v err=%v", got, err)
		}
		if _, err := repo.Get(ctx, 999999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在的 ID 应报 ErrNotFound，得到 %v", err)
		}

		cnt, err := repo.Count(ctx, Filter{ServerID: &s2, Category: "offline"})
		if err != nil || cnt != 1 {
			t.Fatalf("Count 异常：cnt=%d err=%v", cnt, err)
		}
		list, err := repo.List(ctx, Filter{}, 10, 0)
		if err != nil || len(list) != 2 || list[0].ID != p2.Metadata.ID || list[1].ID != p1.Metadata.ID {
			t.Fatalf("List 倒序异常：%+v err=%v", list, err)
		}

		pruned, err := repo.DeleteExpiredBefore(ctx, now)
		if err != nil || pruned != 1 {
			t.Fatalf("DeleteExpiredBefore 应删 1 条，得到 %d err=%v", pruned, err)
		}
		if _, err := repo.Get(ctx, p1.Metadata.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("过期证据包应已清理：err=%v", err)
		}
		if _, err := repo.Get(ctx, p2.Metadata.ID); err != nil {
			t.Fatalf("未过期证据包不应被清理：%v", err)
		}
	})
}
