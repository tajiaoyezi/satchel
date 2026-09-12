package schema

import (
	"go/parser"
	"go/token"
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
			col("created_at", TypeTime),
		},
	})
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
		`StatusFields: []string{"seen", "approved_by"}`,
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
