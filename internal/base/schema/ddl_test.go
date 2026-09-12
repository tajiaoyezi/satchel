package schema

import (
	"strings"
	"testing"
)

// allTypesRegistry 覆盖八类类型、默认值、CHECK、复合主键、唯一键、部分索引、外键与 ON DELETE。
func allTypesRegistry() *Registry {
	r := New()
	r.Add(Table{
		Name: "parents", GoName: "Parent",
		Columns: []Column{
			col("id", TypeSerial),
			col("name", TypeText),
		},
		Indexes: []Index{{Name: "parents_name_key", Columns: []string{"name"}, Unique: true}},
	})
	r.Add(Table{
		Name: "things", GoName: "Thing",
		Columns: []Column{
			col("id", TypeSerial),
			col("count", TypeInt).def("0"),
			col("ratio", TypeFloat).null(),
			col("enabled", TypeBool).def("FALSE"),
			col("seen_at", TypeTime).def("CURRENT_TIMESTAMP"),
			col("payload", TypeJSON).def("'{}'"),
			col("state", TypeText).def("'new'").enum("new", "done"),
			col("raw", TypeBlob).null(),
			col("parent_id", TypeInt).null(),
		},
		Indexes: []Index{
			{Name: "things_state_active", Columns: []string{"state"}, Where: "state <> 'done'"},
		},
		ForeignKeys: []ForeignKey{fk("parent_id", "parents", "SET NULL")},
	})
	r.Add(Table{
		Name: "links", GoName: "Link",
		Columns:     []Column{col("thing_id", TypeInt), col("parent_id", TypeInt)},
		PrimaryKey:  []string{"thing_id", "parent_id"},
		ForeignKeys: []ForeignKey{fk("thing_id", "things", "CASCADE"), fk("parent_id", "parents", "RESTRICT")},
	})
	return r
}

func TestDDLSQLite(t *testing.T) {
	r := allTypesRegistry()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	ddl := DDL(r, SQLite)
	for _, want := range []string{
		"id INTEGER PRIMARY KEY AUTOINCREMENT",
		"count INTEGER NOT NULL DEFAULT 0",
		"ratio REAL",
		"enabled BOOLEAN NOT NULL DEFAULT FALSE",
		"seen_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP",
		"payload TEXT NOT NULL DEFAULT '{}'",
		"state TEXT NOT NULL DEFAULT 'new' CHECK (state IN ('new', 'done'))",
		"raw BLOB,",
		"PRIMARY KEY (thing_id, parent_id)",
		"CREATE UNIQUE INDEX parents_name_key ON parents (name);",
		"CREATE INDEX things_state_active ON things (state) WHERE state <> 'done';",
		"FOREIGN KEY (parent_id) REFERENCES parents (id) ON DELETE SET NULL",
		"FOREIGN KEY (thing_id) REFERENCES things (id) ON DELETE CASCADE",
		"FOREIGN KEY (parent_id) REFERENCES parents (id) ON DELETE RESTRICT",
		SplitMarker,
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("SQLite DDL 缺少 %q：\n%s", want, ddl)
		}
	}
	if strings.Index(ddl, "CREATE TABLE parents") > strings.Index(ddl, "CREATE TABLE things") {
		t.Error("被引用的表 parents 应当排在 things 前面")
	}
}

func TestDDLPostgres(t *testing.T) {
	ddl := DDL(allTypesRegistry(), Postgres)
	for _, want := range []string{
		"id BIGSERIAL PRIMARY KEY",
		"count BIGINT NOT NULL DEFAULT 0",
		"ratio DOUBLE PRECISION",
		"enabled BOOLEAN NOT NULL DEFAULT FALSE",
		"seen_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP",
		"payload JSONB NOT NULL DEFAULT '{}'",
		"state TEXT NOT NULL DEFAULT 'new' CHECK (state IN ('new', 'done'))",
		"raw BYTEA,",
		"PRIMARY KEY (thing_id, parent_id)",
		"CREATE UNIQUE INDEX parents_name_key ON parents (name);",
		"CREATE INDEX things_state_active ON things (state) WHERE state <> 'done';",
		"ON DELETE SET NULL",
		"ON DELETE CASCADE",
		"ON DELETE RESTRICT",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("PostgreSQL DDL 缺少 %q：\n%s", want, ddl)
		}
	}
}

func TestTypeNameCoversEveryType(t *testing.T) {
	for typ := TypeSerial; typ <= TypeBlob; typ++ {
		for _, d := range []Dialect{SQLite, Postgres} {
			if TypeName(typ, d) == "" {
				t.Errorf("%s 在 %s 里没有类型表达", typ, d)
			}
		}
	}
}
