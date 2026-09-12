package schema

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustParse(t *testing.T, name string, src []byte) {
	t.Helper()
	if _, err := parser.ParseFile(token.NewFileSet(), name, src, parser.AllErrors); err != nil {
		t.Fatalf("%s 不是合法的 Go：%v\n%s", name, err, src)
	}
}

func TestGenerateModels(t *testing.T) {
	src, err := GenerateModels(allTypesRegistry())
	if err != nil {
		t.Fatal(err)
	}
	mustParse(t, "zz_generated.go", src)
	for _, want := range []string{
		"type Thing struct",
		"bun.BaseModel `bun:\"table:things\"`",
		"ID int64 `bun:\"id,pk,autoincrement\"`",
		"Ratio *float64 `bun:\"ratio\"`",
		"SeenAt time.Time `bun:\"seen_at\"`",
		"Payload json.RawMessage `bun:\"payload\"`",
		"Raw []byte `bun:\"raw\"`",
		"ParentID *int64 `bun:\"parent_id\"`",
		"ThingID int64 `bun:\"thing_id,pk\"`",
	} {
		if !strings.Contains(squash(string(src)), want) {
			t.Errorf("模型缺少 %q：\n%s", want, src)
		}
	}
}

// typeCheckKinds 把生成的 kinds 文件与 pkg/api/v1 里手写的非测试文件一起做类型检查，
// 抓住「生成了没人用的 import」这类只有编译才能发现的问题。
func typeCheckKinds(t *testing.T, generated []byte) {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "pkg", "api", "v1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "zz_generated") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	f, err := parser.ParseFile(fset, "zz_generated_kinds.go", generated, 0)
	if err != nil {
		t.Fatalf("生成物不是合法的 Go：%v\n%s", err, generated)
	}
	files = append(files, f)
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("v1", fset, files, nil); err != nil {
		t.Fatalf("生成物类型检查失败：%v\n%s", err, generated)
	}
}

// 唯一的时间列是 created_at（meta 列）时，Spec / Status 里没有 time.Time，不能生成 import "time"。
func TestGenerateKindsImportsOnlyWhatStructsUse(t *testing.T) {
	r := New()
	r.Add(Table{
		Name: "notes", Kind: "Note", KindClass: KindConfig,
		Columns: []Column{col("id", TypeSerial), col("title", TypeText), col("created_at", TypeTime)},
	})
	src, err := GenerateKinds(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `"time"`) {
		t.Fatalf("不该 import time：\n%s", src)
	}
	typeCheckKinds(t, src)
}

func TestGenerateKindsForDefaultRegistryTypeChecks(t *testing.T) {
	src, err := GenerateKinds(Default())
	if err != nil {
		t.Fatal(err)
	}
	typeCheckKinds(t, src)
}

func TestGenerateKinds(t *testing.T) {
	r := New()
	r.Add(Table{
		Name: "widgets", Kind: "Widget", KindClass: KindConfig,
		Columns: []Column{
			col("id", TypeSerial),
			col("name", TypeText),
			col("size", TypeInt).null(),
			col("seen", TypeTime).null().cls(ClassStatus),
			col("approved_by", TypeText).null().cls(ClassHuman),
			col("token", TypeText).cls(ClassMasterSelf).masked(),
			col("created_at", TypeTime),
		},
	})
	r.tables["widgets"].Columns[1].Immutable = true // name
	src, err := GenerateKinds(r)
	if err != nil {
		t.Fatal(err)
	}
	mustParse(t, "zz_generated_kinds.go", src)
	for _, want := range []string{
		"type WidgetSpec struct",
		"Name string `json:\"name\"`",
		"Size *int64 `json:\"size\"`",
		"type WidgetStatus struct",
		"Seen *time.Time `json:\"seen\"`",
		"ApprovedBy *string `json:\"approved_by\"`",
		`Class: ClassConfig`,
		`SpecFields: []string{"name", "size"}`,
		`StatusFields: []string{"seen", "approved_by", "token"}`,
		`MaskedFields: []string{"token"}`,
		`ImmutableFields: []string{"name"}`,
		`"seen": "status"`,
		`"approved_by": "human"`,
		`"resource_version": "meta"`,
	} {
		if !strings.Contains(squash(string(src)), want) {
			t.Errorf("kind 生成物缺少 %q：\n%s", want, src)
		}
	}
}

// squash 把 gofmt 的对齐空白压成单个空格，方便按片段比对。
func squash(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
