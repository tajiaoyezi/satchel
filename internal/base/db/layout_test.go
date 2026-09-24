package db

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// otherPrograms 是恰好与数据目录里的名字同名、指的却是别的程序的文件：mcp init 要改 Hermes 自己的 config.yaml，
// 这个名字在 runtime_hermes.go 里定义一次（按文件与名字精确放行，不放过同一文件里别的名字）。
var otherPrograms = map[string]string{
	filepath.Join("internal", "projection", "cli", "runtime_hermes.go"): ConfigYAMLFile,
}

// storage-dual-database「名字只有一处」：数据目录里的子目录与文件名只在定义常量的这一处出现，别处不许写死字面量。
// 扫整个仓库的 Go 源码（含测试），只放过本文件与 config.go（以及上面 otherPrograms 里的同名文件）。database.json 与 satchel.db
// 不在清单里：它们在错误文案的断言里以字面量出现是合理的（点名文件的 reason）。
func TestLayoutNamesDefinedOnce(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	names := []string{SubscribesDir, RuleTemplatesDir, PublicDir, MasterKeyFile, SocketFile, ConfigYAMLFile}
	re := regexp.MustCompile(`"(` + strings.Join(escapeAll(names), "|") + `)"`)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "config.go") || strings.HasSuffix(path, "layout_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		matches := re.FindAllStringSubmatch(string(data), -1)
		if name, ok := otherPrograms[rel]; ok && len(matches) == 1 && matches[0][1] == name {
			return nil
		}
		for _, m := range matches {
			t.Errorf("%s 写死了数据目录里的名字 %q，应当用 db 包的常量", path, m[1])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func escapeAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = regexp.QuoteMeta(n)
	}
	return out
}
