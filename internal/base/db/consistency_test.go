package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
)

// 迁移后的实际表结构必须与注册表逐项一致，两库各验一次。
func TestMigratedSchemaMatchesRegistry(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		actual, err := db.Introspect(context.Background(), bdb)
		if err != nil {
			t.Fatal(err)
		}
		if diff := db.Diff(schema.Default(), db.DialectOf(bdb), actual); len(diff) > 0 {
			t.Fatalf("库与注册表有 %d 处不一致：\n  %s", len(diff), strings.Join(diff, "\n  "))
		}
	})
}

// driftedRegistry 复制默认注册表，并给 tasks 多加一列，模拟改了注册表没重新生成迁移。
func driftedRegistry() *schema.Registry {
	r := schema.New()
	for _, t := range schema.Default().Tables() {
		copied := *t
		if t.Name == "tasks" {
			copied.Columns = append(append([]schema.Column(nil), t.Columns...), schema.Column{Name: "colour", Type: schema.TypeText})
		}
		r.Add(copied)
	}
	return r
}

func TestDiffReportsRegistryDrift(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		actual, err := db.Introspect(context.Background(), bdb)
		if err != nil {
			t.Fatal(err)
		}
		diff := db.Diff(driftedRegistry(), db.DialectOf(bdb), actual)
		if len(diff) != 1 || !strings.Contains(diff[0], "列 colour") || !strings.Contains(diff[0], "tasks") {
			t.Fatalf("应当恰好报出 tasks 的列 colour，得到 %v", diff)
		}
	})
}

// enumDriftRegistry 复制默认注册表，并给 alerts.status 的枚举清单多加一个值，模拟改了 CHECK 没重新生成迁移。
func enumDriftRegistry() *schema.Registry {
	r := schema.New()
	for _, t := range schema.Default().Tables() {
		copied := *t
		if t.Name == "alerts" {
			copied.Columns = append([]schema.Column(nil), t.Columns...)
			for i := range copied.Columns {
				if copied.Columns[i].Name == "status" {
					copied.Columns[i].Enum = append(append([]string(nil), t.Columns[i].Enum...), "escalated")
				}
			}
		}
		r.Add(copied)
	}
	return r
}

func TestDiffReportsCheckDrift(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		actual, err := db.Introspect(context.Background(), bdb)
		if err != nil {
			t.Fatal(err)
		}
		diff := db.Diff(enumDriftRegistry(), db.DialectOf(bdb), actual)
		if len(diff) != 2 {
			t.Fatalf("应当报出注册表侧多一条 CHECK、库侧多一条 CHECK，得到 %v", diff)
		}
		for _, d := range diff {
			if !strings.Contains(d, "alerts") || !strings.Contains(d, "CHECK") {
				t.Errorf("差异应当点名 alerts 的 CHECK：%s", d)
			}
		}
		if !strings.Contains(strings.Join(diff, "\n"), "列 status") {
			t.Errorf("注册表侧的差异应当点名列 status：%v", diff)
		}
	})
}
