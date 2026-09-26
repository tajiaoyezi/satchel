package logs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/logging"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func admin() context.Context { return v1.WithIdentity(context.Background(), v1.LocalAdmin("root")) }
func user() context.Context {
	return v1.WithIdentity(context.Background(), v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser})
}

func codeOf(err error) v1.Code {
	var e *v1.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func inv(path string, flags map[string]any, limit int, cursor string) *command.Invocation {
	if flags == nil {
		flags = map[string]any{}
	}
	return &command.Invocation{Path: strings.Fields(path), Flags: flags, Page: &command.Page{Limit: limit, Cursor: cursor}}
}

// setup 建数据目录并往当前日志文件里写 lines。
func setup(t *testing.T, lines []string) *Service {
	t.Helper()
	dir := t.TempDir()
	if err := db.EnsureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if lines != nil {
		content := strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, db.LogsDir, db.LogFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return New(dir)
}

func run(t *testing.T, ctx context.Context, s *Service, in *command.Invocation) (*command.PageResult, error) {
	t.Helper()
	out, err := s.Bindings()[strings.Join(in.Path, " ")](ctx, in)
	if err != nil {
		return nil, err
	}
	return out.(*command.PageResult), nil
}

// master-logs「按级别过滤」：取这一级及更高的，从新到旧；拆不开的行没有级别，不算。
func TestListLevel(t *testing.T) {
	s := setup(t, []string{
		`time=t1 level=INFO msg=a`,
		`time=t2 level=WARN msg=b k=v`,
		`garbage line`,
		`time=t3 level=ERROR msg=c`,
	})
	res, err := run(t, admin(), s, inv("logs list", map[string]any{"level": "warn"}, 50, ""))
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 || len(res.Items) != 2 || res.Items[0].(logging.Entry).Msg != "c" || res.Items[1].(logging.Entry).Attrs["k"] != "v" {
		t.Fatalf("得到 %+v", res)
	}
	res, _ = run(t, admin(), s, inv("logs list", nil, 50, ""))
	if res.Total != 4 || res.Items[1].(logging.Entry).Raw != "garbage line" {
		t.Fatalf("不带过滤应当四行都在，得到 %+v", res)
	}
	if _, err := run(t, admin(), s, inv("logs list", map[string]any{"level": "fatal"}, 50, "")); codeOf(err) != v1.CodeBadRequest {
		t.Fatalf("非法级别应当 bad_request，得到 %v", err)
	}
}

// master-logs「翻页」：120 行分三页，不重不漏。
func TestListPaging(t *testing.T) {
	var lines []string
	for i := 0; i < 130; i++ {
		msg := fmt.Sprintf("m%03d", i)
		if i%13 == 0 {
			msg = "skip" // 10 行不含 m，被 --grep 过滤掉
		}
		lines = append(lines, fmt.Sprintf("time=t level=INFO msg=%s", msg))
	}
	s := setup(t, lines)
	seen := map[string]bool{}
	cursor := ""
	var sizes []int
	for {
		res, err := run(t, admin(), s, inv("logs list", map[string]any{"grep": "msg=m"}, 50, cursor))
		if err != nil {
			t.Fatal(err)
		}
		if res.Total != 120 {
			t.Fatalf("total 应当是 120，得到 %d", res.Total)
		}
		sizes = append(sizes, len(res.Items))
		for _, it := range res.Items {
			seen[it.(logging.Entry).Msg] = true
		}
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	if fmt.Sprint(sizes) != "[50 50 20]" || len(seen) != 120 {
		t.Fatalf("三页应当是 50、50、20 且不重不漏，得到 %v、%d 个不同的行", sizes, len(seen))
	}
}

// master-logs「不许出日志目录」与 --file 看旧文件。
func TestListFile(t *testing.T) {
	s := setup(t, []string{`time=t level=INFO msg=now`})
	old := filepath.Join(s.dir, "satchel-2026-09-01T00-00-00.000.log")
	os.WriteFile(old, []byte("time=t level=INFO msg=old\n"), 0o600)
	for _, name := range []string{"../satchel.db", "..", "other.log", "satchel-x.txt"} {
		if _, err := run(t, admin(), s, inv("logs list", map[string]any{"file": name}, 50, "")); codeOf(err) != v1.CodeNotFound {
			t.Fatalf("--file %q 应当 not_found，得到 %v", name, err)
		}
	}
	res, err := run(t, admin(), s, inv("logs list", map[string]any{"file": filepath.Base(old)}, 50, ""))
	if err != nil || res.Total != 1 || res.Items[0].(logging.Entry).Msg != "old" {
		t.Fatalf("应当读到旧文件，得到 %+v %v", res, err)
	}
}

func TestListNoFile(t *testing.T) {
	s := setup(t, nil)
	res, err := run(t, admin(), s, inv("logs list", nil, 50, ""))
	if err != nil || res.Total != 0 || len(res.Items) != 0 {
		t.Fatalf("文件不存在时应当是空列表，得到 %+v %v", res, err)
	}
}

func TestFilesAndForbidden(t *testing.T) {
	s := setup(t, []string{`time=t level=INFO msg=now`})
	res, err := run(t, admin(), s, inv("logs files list", nil, 50, ""))
	if err != nil || res.Total != 1 || res.Items[0].(logging.File).Name != db.LogFile || !res.Items[0].(logging.File).Active {
		t.Fatalf("得到 %+v %v", res, err)
	}
	for _, path := range []string{"logs list", "logs files list"} {
		if _, err := run(t, user(), s, inv(path, nil, 50, "")); codeOf(err) != v1.CodeForbidden {
			t.Fatalf("%s 普通用户应当 forbidden，得到 %v", path, err)
		}
	}
}
