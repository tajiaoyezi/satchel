package db

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// storage-dual-database「名字只有一处」：数据目录里的子目录与文件名只在定义常量的这一处出现，别处不许写死字面量。
// 扫整个仓库的 Go 源码（含测试），只放过本文件与 config.go。database.json 与 satchel.db 不在清单里：
// 它们在错误文案的断言里以字面量出现是合理的（点名文件的 reason）。
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
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
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
