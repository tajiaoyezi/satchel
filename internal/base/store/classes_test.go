package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 分档守卫：注册表里出现的每个分档都要有一个能写它的原语，并且真的写一次成功。
// 加新分档时要同时补这张表，否则测试直接失败。
func TestEveryClassHasAWritePrimitive(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		writers := map[schema.Class]func(t *testing.T){
			schema.ClassSpec: func(t *testing.T) {
				r := rule("spec")
				mustInsert(t, s, r)
				r.Trigger = "server_online"
				if err := s.UpdateSpec(ctx, r, false, "trigger"); err != nil {
					t.Fatal(err)
				}
			},
			schema.ClassStatus: func(t *testing.T) {
				a := alert("status")
				mustInsert(t, s, a)
				a.OccurrenceCount = 2
				if err := s.UpdateStatus(ctx, a, []string{"occurrence_count"}); err != nil {
					t.Fatal(err)
				}
			},
			schema.ClassAction: func(t *testing.T) {
				tk := task("action")
				mustInsert(t, s, tk)
				tk.Status = "claimed"
				if err := s.UpdateAction(ctx, tk, []string{"status"}); err != nil {
					t.Fatal(err)
				}
			},
			schema.ClassHuman: func(t *testing.T) {
				r := rule("human")
				mustInsert(t, s, r)
				by := "alice"
				r.ApprovedBy = &by
				if err := s.UpdateHuman(ctx, r, []string{"approved_by"}); err != nil {
					t.Fatal(err)
				}
			},
			schema.ClassMasterSelf: func(t *testing.T) {
				ch := &model.NotifyChannel{Name: "master", Type: "webhook"}
				mustInsert(t, s, ch)
				ch.Target = "https://x"
				if err := s.UpdateMasterSelf(ctx, ch, []string{"target"}); err != nil {
					t.Fatal(err)
				}
			},
			// meta 列不经分档更新写：由 Insert、SoftDelete / Restore 与版本抬升写。
			schema.ClassMeta: func(t *testing.T) {
				ch := &model.NotifyChannel{Name: "meta", Type: "webhook"}
				mustInsert(t, s, ch)
				if err := s.SoftDelete(ctx, ch, false); err != nil {
					t.Fatal(err)
				}
			},
		}
		seen := map[schema.Class]bool{}
		for _, tbl := range schema.Default().Tables() {
			for _, c := range tbl.Columns {
				seen[c.Class] = true
			}
		}
		if len(seen) == 0 {
			t.Fatal("注册表里没有列")
		}
		for class := range seen {
			write, ok := writers[class]
			if !ok {
				t.Errorf("分档 %s 没有对应的写入原语", class)
				continue
			}
			t.Run(class.String(), write)
		}
	})
}

// 每个原语只收本分档的列：把每个分档的一列喂给别的分档的原语，都要被 bad_request 拦下。
// 漏标 Class 的列会落成 spec，这张矩阵在下两个 change 录 mmwx 表时守着它。
func TestPrimitivesRejectOtherClasses(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := newStore(bdb)
		ch := &model.NotifyChannel{Name: "matrix", Type: "webhook"}
		mustInsert(t, s, ch)
		r := rule("matrix")
		mustInsert(t, s, r)
		tk := task("matrix")
		mustInsert(t, s, tk)
		// 每个分档挑一张表的一列作为样本。
		samples := map[schema.Class]struct {
			model  any
			column string
		}{
			schema.ClassSpec:       {ch, "name"},
			schema.ClassStatus:     {ch, "last_error"},
			schema.ClassMasterSelf: {ch, "target"},
			schema.ClassAction:     {tk, "status"},
			schema.ClassHuman:      {r, "approved_by"},
			schema.ClassMeta:       {ch, "resource_version"},
		}
		primitives := map[schema.Class]func(model any, column string) error{
			schema.ClassSpec:       func(m any, c string) error { return s.UpdateSpec(ctx, m, true, c) },
			schema.ClassStatus:     func(m any, c string) error { return s.UpdateStatus(ctx, m, []string{c}) },
			schema.ClassAction:     func(m any, c string) error { return s.UpdateAction(ctx, m, []string{c}) },
			schema.ClassHuman:      func(m any, c string) error { return s.UpdateHuman(ctx, m, []string{c}) },
			schema.ClassMasterSelf: func(m any, c string) error { return s.UpdateMasterSelf(ctx, m, []string{c}) },
		}
		for _, tbl := range schema.Default().Tables() {
			for _, c := range tbl.Columns {
				if _, ok := samples[c.Class]; !ok {
					t.Fatalf("分档 %s 没有样本列，矩阵要补", c.Class)
				}
			}
		}
		for sampleClass, sample := range samples {
			for primClass, write := range primitives {
				if sampleClass == primClass {
					continue
				}
				err := write(sample.model, sample.column)
				if e := wantCode(t, err, v1.CodeBadRequest); !strings.Contains(e.Reason, sample.column) {
					t.Errorf("%s 档的列 %s 喂给 %s 档的原语应当被拒并点名列，得到 %v", sampleClass, sample.column, primClass, err)
				}
			}
		}
	})
}
