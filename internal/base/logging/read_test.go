package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/satchel/satchel/internal/base/db"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), db.LogFile)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTail(t *testing.T) {
	// 跨块：每行约 100 字节，2000 行超过一块（64 KiB）。
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "line-%04d %s\n", i, strings.Repeat("y", 90))
	}
	path := writeFile(t, b.String()+"half-written")
	for _, n := range []int{1, 700, 1999, 2000, 5000} {
		got, err := Tail(path, n)
		if err != nil {
			t.Fatal(err)
		}
		want := n
		if want > 2000 {
			want = 2000
		}
		if len(got) != want {
			t.Fatalf("n=%d 应当得到 %d 行，得到 %d", n, want, len(got))
		}
		if !strings.HasPrefix(got[len(got)-1], "line-1999 ") || !strings.HasPrefix(got[0], fmt.Sprintf("line-%04d ", 2000-want)) {
			t.Fatalf("n=%d 首尾不对：%q … %q", n, got[0][:10], got[len(got)-1][:10])
		}
	}
	if got, err := Tail(writeFile(t, ""), 10); err != nil || len(got) != 0 {
		t.Fatalf("空文件应当没有行，得到 %v %v", got, err)
	}
	if got, err := Tail(writeFile(t, "only-half"), 10); err != nil || len(got) != 0 {
		t.Fatalf("只有半行时应当没有行，得到 %v %v", got, err)
	}
	if _, err := Tail(filepath.Join(t.TempDir(), "none.log"), 10); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("文件不存在应当是 ErrNotExist，得到 %v", err)
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		line string
		want Entry
	}{
		{`time=2026-09-25T12:00:00.000+08:00 level=INFO msg=主控已启动 listen=0.0.0.0:12889`,
			Entry{Time: "2026-09-25T12:00:00.000+08:00", Level: "INFO", Msg: "主控已启动", Attrs: map[string]string{"listen": "0.0.0.0:12889"}}},
		{`time=t level=ERROR msg="审计写入失败 a=b" error="say \"hi\"" empty=""`,
			Entry{Time: "t", Level: "ERROR", Msg: "审计写入失败 a=b", Attrs: map[string]string{"error": `say "hi"`, "empty": ""}}},
		{`time=t level=WARN msg=x`, Entry{Time: "t", Level: "WARN", Msg: "x"}},
		{`panic: runtime error`, Entry{Raw: `panic: runtime error`}},
		{`level=INFO msg=x`, Entry{Raw: `level=INFO msg=x`}},
		{`time=t level=INFO msg="unterminated`, Entry{Raw: `time=t level=INFO msg="unterminated`}},
	}
	for _, c := range cases {
		if got := Parse(c.line); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q)\n得到 %+v\n应当 %+v", c.line, got, c.want)
		}
	}
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for name, age := range map[string]time.Duration{
		db.LogFile: 3 * time.Hour, "satchel-2026-09-01T00-00-00.000.log": time.Hour,
		"satchel-2026-08-01T00-00-00.000.log": 2 * time.Hour, "other.log": 0, "satchel-x.txt": 0,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	os.Mkdir(filepath.Join(dir, "satchel-dir.log"), 0o700)
	files, err := Files(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	want := []string{db.LogFile, "satchel-2026-09-01T00-00-00.000.log", "satchel-2026-08-01T00-00-00.000.log"}
	if !reflect.DeepEqual(names, want) || !files[0].Active || files[1].Active || files[0].Size != 1 {
		t.Fatalf("得到 %+v", files)
	}
	if files, err := Files(filepath.Join(dir, "none")); err != nil || files != nil {
		t.Fatalf("目录不存在应当是空列表，得到 %v %v", files, err)
	}
}
