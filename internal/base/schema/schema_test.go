package schema

import (
	"strings"
	"testing"
)

func serialTable(name string, cols ...Column) Table {
	return Table{Name: name, GoName: GoName(name), Columns: append([]Column{col("id", TypeSerial)}, cols...)}
}

func TestValidateReportsProblems(t *testing.T) {
	cases := []struct {
		name  string
		build func(r *Registry)
		want  string
	}{
		{"重复列", func(r *Registry) {
			r.Add(serialTable("a", col("x", TypeText), col("x", TypeText)))
		}, "列 x 重复"},
		{"无主键", func(r *Registry) {
			r.Add(Table{Name: "a", GoName: "A", Columns: []Column{col("x", TypeText)}})
		}, "没有主键"},
		{"外键引用不存在的表", func(r *Registry) {
			r.Add(serialTable("a", col("b_id", TypeInt)))
			r.tables["a"].ForeignKeys = []ForeignKey{fk("b_id", "b", "")}
		}, "不存在的表 b"},
		{"外键引用不存在的列", func(r *Registry) {
			r.Add(serialTable("a", col("b_id", TypeInt)))
			r.Add(serialTable("b"))
			r.tables["a"].ForeignKeys = []ForeignKey{{Columns: []string{"b_id"}, RefTable: "b", RefColumns: []string{"nope"}}}
		}, "没有的列 nope"},
		{"索引名超长", func(r *Registry) {
			r.Add(serialTable("a", col("x", TypeText)))
			r.tables["a"].Indexes = []Index{{Name: strings.Repeat("x", 64), Columns: []string{"x"}}}
		}, "超过 63"},
		{"外键成环", func(r *Registry) {
			r.Add(serialTable("a", col("b_id", TypeInt)))
			r.Add(serialTable("b", col("a_id", TypeInt)))
			r.tables["a"].ForeignKeys = []ForeignKey{fk("b_id", "b", "")}
			r.tables["b"].ForeignKeys = []ForeignKey{fk("a_id", "a", "")}
		}, "外键成环"},
		{"Kind 没给类别", func(r *Registry) {
			r.Add(Table{Name: "a", Kind: "A", Columns: []Column{col("id", TypeSerial)}})
		}, "要么都给"},
		{"表登记两次", func(r *Registry) {
			r.Add(serialTable("a"))
			r.Add(serialTable("a"))
		}, "登记了两次"},
		{"append-only 的配置类", func(r *Registry) {
			r.Add(Table{Name: "a", Kind: "A", KindClass: KindConfig, AppendOnly: true, Columns: []Column{col("id", TypeSerial)}})
		}, "append-only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			tc.build(r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("想要一条含 %q 的错误，Validate 却通过了", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("想要错误含 %q，得到：%v", tc.want, err)
			}
		})
	}
}

func TestAddFillsCommonColumns(t *testing.T) {
	cases := []struct {
		class                    KindClass
		wantVersion, wantDeleted bool
	}{
		{KindConfig, true, true},
		{KindAction, true, true},
		{KindMasterSettings, true, false},
		{KindSystem, false, false},
		{KindReadOnly, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.class.String(), func(t *testing.T) {
			r := New()
			r.Add(Table{Name: "a", Kind: "A", KindClass: tc.class, Columns: []Column{col("id", TypeSerial), col("created_at", TypeTime)}})
			tbl, _ := r.Table("a")
			if tbl.HasVersion() != tc.wantVersion {
				t.Fatalf("resource_version 存在 = %v，想要 %v", tbl.HasVersion(), tc.wantVersion)
			}
			_, hasDeleted := tbl.Column("deleted_at")
			if hasDeleted != tc.wantDeleted {
				t.Fatalf("deleted_at 存在 = %v，想要 %v", hasDeleted, tc.wantDeleted)
			}
			for _, name := range []string{"id", "created_at"} {
				c, _ := tbl.Column(name)
				if c.Class != ClassMeta {
					t.Fatalf("公共列 %s 应当自动归 meta 档，得到 %s", name, c.Class)
				}
			}
		})
	}
	t.Run("不是 kind 的表不补列", func(t *testing.T) {
		r := New()
		r.Add(serialTable("a"))
		tbl, _ := r.Table("a")
		if tbl.HasVersion() {
			t.Fatal("不是 kind 的表不该有 resource_version")
		}
	})
}

func TestTablesTopologicalOrder(t *testing.T) {
	r := New()
	r.Add(serialTable("x", col("y_id", TypeInt)))
	r.Add(serialTable("y", col("z_id", TypeInt)))
	r.Add(serialTable("z"))
	r.tables["x"].ForeignKeys = []ForeignKey{fk("y_id", "y", "")}
	r.tables["y"].ForeignKeys = []ForeignKey{fk("z_id", "z", "")}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tbl := range r.Tables() {
		got = append(got, tbl.Name)
	}
	if strings.Join(got, ",") != "z,y,x" {
		t.Fatalf("拓扑顺序应当是 z,y,x，得到 %v", got)
	}
}

func TestGoName(t *testing.T) {
	cases := map[string]string{"id": "ID", "dedup_key": "DedupKey", "alert_id": "AlertID", "object_kind": "ObjectKind", "at": "At"}
	for in, want := range cases {
		if got := GoName(in); got != want {
			t.Errorf("GoName(%q) = %q，想要 %q", in, got, want)
		}
	}
}
