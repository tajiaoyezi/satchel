package schedule

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/taskruns"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func admin() context.Context { return v1.WithIdentity(context.Background(), v1.LocalAdmin("root")) }
func user() context.Context {
	return v1.WithIdentity(context.Background(), v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser})
}

func wantCode(t *testing.T, err error, code v1.Code) {
	t.Helper()
	var e *v1.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("想要错误码 %s，得到 %v", code, err)
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newService(bdb *bun.DB) (*Service, *clock) {
	c := &clock{t: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	s := New(core.New(bdb), nil)
	s.SetNow(c.now)
	return s, c
}

func runs(t *testing.T, s *Service, f core.Filter) []core.Run {
	t.Helper()
	out, err := s.repo.List(context.Background(), f, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// master-scheduler「成功与失败都有记录」：每小时的任务开始时插 running，结束时原地改。
func TestRecordHourlyTask(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s, c := newService(bdb)
		task := TaskInfo{Name: "audit_cleanup", Every: time.Hour}
		r := s.Begin(ctx, task)
		if got := runs(t, s, core.Filter{}); len(got) != 1 || got[0].Status != core.StatusRunning {
			t.Fatalf("开始时应当有一行 running：%+v", got)
		}
		c.t = c.t.Add(2 * time.Second)
		s.End(ctx, r, "删掉 12 条", nil)
		c.t = c.t.Add(time.Hour)
		s.End(ctx, s.Begin(ctx, task), "", errors.New("连不上"))
		got := runs(t, s, core.Filter{})
		if len(got) != 2 || got[1].Status != core.StatusOK || got[1].DurationMs != 2000 || got[1].Detail != "删掉 12 条" ||
			got[0].Status != core.StatusError || got[0].Detail != "连不上" {
			t.Fatalf("记录不对：%+v", got)
		}
	})
}

// master-scheduler「高频任务的成功节流」：每分钟一次、100 分钟，只有第 30 分钟那次失败 → ok 3 条（第 1、31、91 分钟）、error 1 条。
func TestRecordThrottle(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s, c := newService(bdb)
		task := TaskInfo{Name: "db_health", Every: time.Minute}
		start := c.t
		for minute := 1; minute <= 100; minute++ {
			c.t = start.Add(time.Duration(minute) * time.Minute)
			r := s.Begin(ctx, task)
			if r.id != 0 {
				t.Fatal("高频任务开始时不应当插 running 行")
			}
			var err error
			if minute == 30 {
				err = errors.New("quick_check 失败")
			}
			s.End(ctx, r, "ok", err)
		}
		oks := runs(t, s, core.Filter{Status: core.StatusOK})
		errs := runs(t, s, core.Filter{Status: core.StatusError})
		var minutes []int
		for i := len(oks) - 1; i >= 0; i-- {
			minutes = append(minutes, int(oks[i].StartedAt.Sub(start)/time.Minute))
		}
		if len(errs) != 1 || len(minutes) != 3 || minutes[0] != 1 || minutes[1] != 31 || minutes[2] != 91 {
			t.Fatalf("ok 应当在第 1、31、91 分钟，error 1 条：ok %v，error %d", minutes, len(errs))
		}
	})
}

// master-scheduler「上次中断的记录」与按保留期删。
func TestInterruptedAndPrune(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s, c := newService(bdb)
		s.Begin(ctx, TaskInfo{Name: "audit_cleanup", Every: time.Hour})
		if err := s.MarkInterrupted(ctx); err != nil {
			t.Fatal(err)
		}
		got := runs(t, s, core.Filter{})
		if len(got) != 1 || got[0].Status != core.StatusError || got[0].Detail != interruptedDetail {
			t.Fatalf("中断的记录不对：%+v", got)
		}
		c.t = c.t.Add(RunRetention + time.Minute)
		if n, err := s.PruneRuns(ctx); err != nil || n != 1 {
			t.Fatalf("应当删掉 1 条，得到 %d %v", n, err)
		}
	})
}

func call(t *testing.T, ctx context.Context, s *Service, path string, flags map[string]any) (*command.PageResult, error) {
	t.Helper()
	if flags == nil {
		flags = map[string]any{}
	}
	in := &command.Invocation{Path: strings.Fields(path), Flags: flags, Page: &command.Page{Limit: 50}}
	out, err := s.Bindings()[path](ctx, in)
	if err != nil {
		return nil, err
	}
	return out.(*command.PageResult), nil
}

// master-scheduler「查看任务与运行记录」：两条命令的输出与过滤、普通用户 forbidden。
func TestCommands(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s, _ := newService(bdb)
		hourly := TaskInfo{Name: "audit_cleanup", Summary: "删审计", Every: time.Hour}
		health := TaskInfo{Name: "db_health", Summary: "查库", Every: time.Minute}
		s.SetTasks([]TaskInfo{health, hourly, {Name: "session_cleanup", Every: time.Hour}})
		s.End(ctx, s.Begin(ctx, hourly), "a", nil)
		s.End(ctx, s.Begin(ctx, hourly), "b", nil)
		s.End(ctx, s.Begin(ctx, health), "", errors.New("坏了"))

		res, err := call(t, admin(), s, "schedule list", nil)
		if err != nil || res.Total != 3 {
			t.Fatalf("schedule list：%+v %v", res, err)
		}
		first, third := res.Items[0].(TaskView), res.Items[2].(TaskView)
		if first.Name != "audit_cleanup" || first.Interval != "1h0m0s" || first.LastStatus != core.StatusOK || first.LastStartedAt == nil ||
			third.Name != "session_cleanup" || third.LastStartedAt != nil || third.LastStatus != "" {
			t.Fatalf("schedule list 的项不对：%+v", res.Items)
		}

		res, err = call(t, admin(), s, "schedule runs list", map[string]any{"status": "error"})
		if err != nil || res.Total != 1 || res.Items[0].(core.Run).Task != "db_health" {
			t.Fatalf("按状态过滤：%+v %v", res, err)
		}
		res, err = call(t, admin(), s, "schedule runs list", map[string]any{"task": "audit_cleanup"})
		if err != nil || res.Total != 2 {
			t.Fatalf("按任务过滤：%+v %v", res, err)
		}
		_, err = call(t, admin(), s, "schedule runs list", map[string]any{"status": "done"})
		wantCode(t, err, v1.CodeBadRequest)
		for _, path := range []string{"schedule list", "schedule runs list"} {
			_, err := call(t, user(), s, path, nil)
			wantCode(t, err, v1.CodeForbidden)
		}
	})
}

// SQLite 的写锁被别的事务占着超过时限：插了 running 的行结束时改不了，Flush（serve 关库之前调）再写一次；
// 高频任务的成功没写进库时不算「上一条写下的 ok」，锁放开后的下一次成功照样写（审查第 1、6 条）。
func TestWriteFailureRetried(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		if db.DialectOf(bdb) != schema.SQLite {
			t.Skip("PostgreSQL 没有单写者锁")
		}
		writeFailureRetried(t, bdb)
	})
}

func writeFailureRetried(t *testing.T, bdb *bun.DB) {
	ctx := context.Background()
	s, c := newService(bdb)
	hourly := s.Begin(ctx, TaskInfo{Name: "audit_cleanup", Every: time.Hour})
	quiet := TaskInfo{Name: "db_health", Every: time.Minute}
	tx, err := bdb.BeginTx(ctx, nil) // _txlock=immediate：一开事务就占住写锁
	if err != nil {
		t.Fatal(err)
	}
	s.End(ctx, hourly, "删掉 0 条", nil)
	s.End(ctx, s.Begin(ctx, quiet), "健康", nil)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := runs(t, s, core.Filter{}); len(got) != 1 || got[0].Status != core.StatusRunning {
		t.Fatalf("锁住期间应当只有那行 running：%+v", got)
	}
	s.Flush(ctx)
	c.t = c.t.Add(time.Minute)
	s.End(ctx, s.Begin(ctx, quiet), "健康", nil)
	got := runs(t, s, core.Filter{})
	if len(got) != 2 || got[1].Task != "audit_cleanup" || got[1].Status != core.StatusOK || got[0].Task != "db_health" || got[0].Status != core.StatusOK {
		t.Fatalf("Flush 后 running 行应当是 ok，锁放开后高频任务的成功应当写上：%+v", got)
	}
}
