package schema

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestGeneratedFilesUpToDate(t *testing.T) {
	outputs, err := Outputs(Default())
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(outputs))
	for p := range outputs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		onDisk, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("读不到生成物 %s：%v（运行 go generate ./internal/base/schema/）", rel, err)
			continue
		}
		if !bytes.Equal(onDisk, outputs[rel]) {
			t.Errorf("%s 与注册表不一致，请运行 go generate ./internal/base/schema/ 并提交", rel)
		}
	}
}

var (
	sqliteTypeWords   = map[string]bool{"INTEGER": true, "REAL": true, "BOOLEAN": true, "TEXT": true, "BLOB": true}
	postgresTypeWords = map[string]bool{"BIGSERIAL": true, "BIGINT": true, "DOUBLE PRECISION": true, "BOOLEAN": true, "TIMESTAMPTZ": true, "JSONB": true, "TEXT": true, "BYTEA": true}
	columnLine        = regexp.MustCompile(`^  ([a-z_]+) ([A-Z]+(?: PRECISION)?)`)
)

// 生成的 DDL 里，每个列定义的类型词只能是 design 第 3 条写死的那几个。
func TestDDLUsesOnlyFixedTypeWords(t *testing.T) {
	cases := []struct {
		dialect Dialect
		allowed map[string]bool
	}{{SQLite, sqliteTypeWords}, {Postgres, postgresTypeWords}}
	for _, tc := range cases {
		t.Run(tc.dialect.String(), func(t *testing.T) {
			ddl := DDL(Default(), tc.dialect)
			columns := 0
			for _, line := range strings.Split(ddl, "\n") {
				if strings.HasPrefix(line, "  PRIMARY KEY") || strings.HasPrefix(line, "  FOREIGN KEY") {
					continue
				}
				m := columnLine.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				columns++
				if !tc.allowed[m[2]] {
					t.Errorf("列 %s 用了不在清单里的类型词 %q：%s", m[1], m[2], strings.TrimSpace(line))
				}
			}
			if columns == 0 {
				t.Fatal("没有识别出任何列定义，正则可能坏了")
			}
			for _, forbidden := range []string{`\bSERIAL\b`, `\bINT\b`, `DATETIME`, `\bTIMESTAMP\b`, `\bJSON\b`, `VARCHAR`} {
				if regexp.MustCompile(forbidden).MatchString(ddl) {
					t.Errorf("DDL 里出现了禁止的类型词 %s", forbidden)
				}
			}
		})
	}
}
