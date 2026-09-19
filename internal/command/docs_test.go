package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// master-command-table「命令 × scope 对照表由表生成」：生成物与表一致；改了表不重生成就红。
func TestCommandsDocUpToDate(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(DocsPath)))
	if err != nil {
		t.Fatalf("读不到 %s：%v（运行 go generate ./internal/command/）", DocsPath, err)
	}
	want := GenerateDocs(Catalog())
	if !bytes.Equal(onDisk, want) {
		t.Fatalf("%s 与命令表不一致，请运行 go generate ./internal/command/ 并提交", DocsPath)
	}
	extra := MustNew(append(catalogCommands(), read("demo", "ping"))...)
	if bytes.Equal(GenerateDocs(extra), onDisk) {
		t.Fatal("多一条命令后生成物应当不同")
	}
	if !strings.Contains(string(GenerateDocs(extra)), "`demo ping`") {
		t.Fatal("新命令应当出现在对照表里")
	}
}

func TestGenerateDocsShape(t *testing.T) {
	doc := string(GenerateDocs(Catalog()))
	for _, want := range []string{"| `whoami` | read | read | — | — | 否 | 否 | `GET /api/v1/whoami` |", "| `audit list` | read | read | — | — | 否 | 是 | `GET /api/v1/audit` |", "`__verify`（隐藏）", "`serve`"} {
		if !strings.Contains(doc, want) {
			t.Errorf("对照表缺 %q：\n%s", want, doc)
		}
	}
	if strings.Contains(strings.SplitN(doc, "## 本地命令", 2)[0], "`version`") {
		t.Error("本地命令不该出现在经主控那一段")
	}
}
