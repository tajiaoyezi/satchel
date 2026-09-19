package audit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/audit"
	svc "github.com/satchel/satchel/internal/service/audit"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func table(t *testing.T) *command.Table {
	t.Helper()
	tbl, err := command.New(
		&command.Command{Path: []string{"whoami"}, Summary: "s", Class: command.ClassRead},
		&command.Command{Path: []string{"explain"}, Summary: "s", Class: command.ClassRead, Offline: true},
		&command.Command{Path: []string{"version"}, Summary: "s", Class: command.ClassLocal},
		&command.Command{Path: []string{"demo", "login"}, Summary: "s", Class: command.ClassAction,
			Flags: []command.Flag{{Name: "secret", Type: command.TypeString, Secret: true}, {Name: "note", Type: command.TypeString}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

type memRecorder struct {
	entries []svc.Entry
	err     error
}

func (m *memRecorder) Record(_ context.Context, e svc.Entry) error {
	if m.err != nil {
		return m.err
	}
	m.entries = append(m.entries, e)
	return nil
}

func next(result any, err error) command.Runner {
	return command.RunnerFunc(func(context.Context, *command.Invocation) (any, error) { return result, err })
}

func TestRecordsAfterRun(t *testing.T) {
	rec := &memRecorder{}
	r := Wrap(rec, table(t), nil, next(map[string]any{"ok": true}, nil))
	admin := v1.WithIdentity(context.Background(), v1.LocalAdmin("root"))
	if _, err := r.Run(admin, &command.Invocation{Path: []string{"whoami"}, Flags: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	failing := Wrap(rec, table(t), nil, next(nil, v1.New(v1.CodeForbidden, "no")))
	if _, err := failing.Run(admin, &command.Invocation{Path: []string{"whoami"}}); err == nil {
		t.Fatal("错误应当原样返回")
	}
	if len(rec.entries) != 2 || rec.entries[0].Result != "ok" || rec.entries[1].Result != "forbidden" || rec.entries[0].Actor != "root" || rec.entries[0].ActorKind != v1.ActorLocalAdmin || rec.entries[0].At.IsZero() {
		t.Fatalf("应当两条：成功与 forbidden：%+v", rec.entries)
	}
	// anonymous、离线命令、本地命令都不记。
	_, _ = r.Run(context.Background(), &command.Invocation{Path: []string{"whoami"}})
	_, _ = r.Run(admin, &command.Invocation{Path: []string{"explain"}, Args: []string{"Task"}})
	_, _ = r.Run(admin, &command.Invocation{Path: []string{"version"}})
	if len(rec.entries) != 2 {
		t.Fatalf("anonymous / explain / version 不该记，得到 %d 条", len(rec.entries))
	}
}

func TestDigestMasksAndTruncates(t *testing.T) {
	cmd, _ := table(t).Lookup("demo login")
	inv := &command.Invocation{Path: []string{"demo", "login"}, Args: []string{"alice"}, Flags: map[string]any{"secret": "hunter2", "note": "hi", "d": 90 * time.Second}}
	d := Digest(cmd, inv)
	if strings.Contains(d, "hunter2") || !strings.Contains(d, `"secret":"***"`) || !strings.Contains(d, `"args":["alice"]`) || !strings.Contains(d, `"d":"1m30s"`) {
		t.Fatalf("摘要应当打码、含位置参数与时长：%s", d)
	}
	long := &command.Invocation{Path: []string{"demo", "login"}, Flags: map[string]any{"note": strings.Repeat("字", 5000)}}
	d = Digest(cmd, long)
	if len(d) > DigestLimit || !strings.HasSuffix(d, ellipsis) || !strings.HasPrefix(d, `{"args":[],"flags":{"note":"`) {
		t.Fatalf("超长应当截到 %d 字节以内、以 … 结尾、开头仍可辨：%d %q", DigestLimit, len(d), d[:40])
	}
	if Digest(nil, &command.Invocation{Path: []string{"x"}}) != `{"args":[],"flags":{}}` {
		t.Fatal("空调用的摘要形状")
	}
}

// master-audit-log「写入失败不改变命令结果」：换成真库并把表弄坏。
func TestWriteFailureKeepsResult(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := svc.New(core.New(bdb, store.New(bdb, schema.Default())))
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		r := Wrap(s, table(t), logger, next(map[string]any{"ok": true}, nil))
		admin := v1.WithIdentity(context.Background(), v1.LocalAdmin("root"))
		if _, err := r.Run(admin, &command.Invocation{Path: []string{"whoami"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := bdb.ExecContext(context.Background(), "ALTER TABLE audit_logs RENAME TO audit_logs_broken"); err != nil {
			t.Fatal(err)
		}
		got, err := r.Run(admin, &command.Invocation{Path: []string{"whoami"}})
		if err != nil || got == nil {
			t.Fatalf("审计写失败时命令结果应当照常返回：%v %v", got, err)
		}
		if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "whoami") || !strings.Contains(logs.String(), "审计记录写入失败") {
			t.Fatalf("应当有一条 error 级别、含 whoami 的日志：\n%s", logs.String())
		}
	})
}
