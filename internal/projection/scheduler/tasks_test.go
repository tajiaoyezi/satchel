package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	coreaudit "github.com/satchel/satchel/internal/core/audit"
	coresecurity "github.com/satchel/satchel/internal/core/security"
	"github.com/satchel/satchel/internal/core/sessions"
	"github.com/satchel/satchel/internal/core/taskruns"
	"github.com/satchel/satchel/internal/core/users"
	"github.com/satchel/satchel/internal/service/audit"
	"github.com/satchel/satchel/internal/service/auth"
	"github.com/satchel/satchel/internal/service/schedule"
	"github.com/satchel/satchel/internal/service/security"
)

// master-scheduler「PostgreSQL 下没有检查点任务」与「列出任务」：SQLite 下八个，PostgreSQL 下没有 db_checkpoint；每个都能跑一次。
func TestTasksPerDriver(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		tasks := Tasks(Deps{
			DB:       bdb,
			Auth:     auth.New(users.New(bdb, st), sessions.New(bdb)),
			Audit:    audit.New(coreaudit.New(bdb, st)),
			Security: security.New(coresecurity.New(bdb), nil),
			Schedule: schedule.New(taskruns.New(bdb), nil),
		})
		var names []string
		for _, task := range tasks {
			names = append(names, task.Info.Name)
			if task.Info.Summary == "" || task.Info.Every <= 0 {
				t.Fatalf("%s 缺说明或间隔", task.Info.Name)
			}
			if detail, err := task.Run(context.Background()); err != nil || detail == "" {
				t.Fatalf("%s 跑一次应当成功并给一句结果：%q %v", task.Info.Name, detail, err)
			}
		}
		want := []string{"session_cleanup", "audit_cleanup", "security_event_cleanup", "task_run_cleanup", "ban_sweep", "login_limit_sweep"}
		if db.DialectOf(bdb) == schema.SQLite {
			want = append(want, "db_checkpoint")
		}
		want = append(want, "db_health")
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("任务清单应当是 %v，得到 %v", want, names)
		}
	})
}

// db_health：好变坏记 error、坏变好记 info，状态不变时不重复记。
func TestHealthCheckTransitions(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var fail error
	run := healthCheck(func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("每次检查应当带时限")
		}
		return fail
	}, logger)
	steps := []error{nil, errors.New("disk i/o error"), errors.New("disk i/o error"), nil, nil}
	for _, e := range steps {
		fail = e
		if _, err := run(context.Background()); (err != nil) != (e != nil) {
			t.Fatalf("结果应当跟着检查走：%v", err)
		}
	}
	// 主控停止打断的那一次：不记日志、不改状态，接下来的成功不会记「已恢复」（审查第 5 条）。
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	fail = context.Canceled
	if _, err := run(stopped); err == nil {
		t.Fatal("被打断的那次应当返回错误")
	}
	fail = nil
	run(context.Background())
	out := logs.String()
	if strings.Count(out, "数据库健康检查失败") != 1 || strings.Count(out, "level=ERROR") != 1 ||
		strings.Count(out, "数据库健康检查已恢复") != 1 || strings.Count(out, "level=INFO") != 1 {
		t.Fatalf("应当各记一次：%s", out)
	}
}
