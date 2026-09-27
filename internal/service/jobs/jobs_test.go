package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/jobs"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func admin() context.Context { return v1.WithIdentity(context.Background(), v1.LocalAdmin("root")) }
func user() context.Context {
	return v1.WithIdentity(context.Background(), v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser})
}

func newService(bdb *bun.DB) *Service {
	digest := func(_ *command.Command, inv *command.Invocation) string { return `{"args":[],"flags":{}}` }
	return New(core.New(bdb, store.New(bdb, schema.Default())), command.Catalog(), digest, nil)
}

func inv() *command.Invocation {
	return &command.Invocation{Path: []string{"backup", "create"}, Flags: map[string]any{}}
}

// wait 等 job 结束。
func wait(t *testing.T, s *Service, jobID string) *core.Job {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		j, err := s.repo.Get(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Finished() {
			return j
		}
	}
	t.Fatalf("job %s 没在限时内结束", jobID)
	return nil
}

func TestStartOutcomes(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := newService(bdb)
		defer s.Stop(context.Background())
		var seen v1.Identity
		ok, err := s.Start(admin(), inv(), func(ctx context.Context) (any, error) {
			seen = v1.IdentityFrom(ctx)
			return map[string]string{"name": "b.zip"}, nil
		}, nil)
		if err != nil || !strings.HasPrefix(ok.JobID, "job-") || len(ok.JobID) != 20 || ok.Status != core.StatusQueued {
			t.Fatalf("受理：%+v %v", ok, err)
		}
		j := wait(t, s, ok.JobID)
		if j.Status != core.StatusDone || *j.ExitCode != 0 || *j.Output != `{"name":"b.zip"}` || seen.Actor != "root" {
			t.Fatalf("成功的 job：%+v 身份 %+v", j, seen)
		}
		var args map[string]any
		if json.Unmarshal(j.Args, &args); args["actor"] != "root" || args["digest"] == nil {
			t.Fatalf("args 应当记调用者与摘要：%s", j.Args)
		}

		failed, _ := s.Start(admin(), inv(), func(context.Context) (any, error) {
			return nil, v1.New(v1.CodeUnavailable, "找不到 pg_dump")
		}, nil)
		j = wait(t, s, failed.JobID)
		var e v1.Error
		if j.Status != core.StatusFailed || *j.ExitCode != 1 || json.Unmarshal([]byte(*j.Output), &e) != nil || e.Code != v1.CodeUnavailable {
			t.Fatalf("失败的 job：%+v", j)
		}

		panicked, _ := s.Start(admin(), inv(), func(context.Context) (any, error) { panic("炸了") }, nil)
		if j = wait(t, s, panicked.JobID); j.Status != core.StatusFailed || !strings.Contains(*j.Output, "panic") {
			t.Fatalf("panic 应当记成失败：%+v", j)
		}

		big, _ := s.Start(admin(), inv(), func(context.Context) (any, error) { return strings.Repeat("x", OutputLimit*2), nil }, nil)
		if j = wait(t, s, big.JobID); !j.OutputTruncated || len(*j.Output) != OutputLimit {
			t.Fatalf("输出应当截断到 %d：%d %v", OutputLimit, len(*j.Output), j.OutputTruncated)
		}
	})
}

// Stop 等正在跑的 job 返回；run 看到的 ctx 被取消，结局照样写上。
func TestStopWaits(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := newService(bdb)
		started := make(chan struct{})
		j, _ := s.Start(admin(), inv(), func(ctx context.Context) (any, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}, nil)
		<-started
		s.Stop(context.Background())
		got, _ := s.repo.Get(context.Background(), j.JobID)
		if got.Status != core.StatusFailed {
			t.Fatalf("停止后结局应当写上：%+v", got)
		}
	})
}

func call(t *testing.T, ctx context.Context, s *Service, path string, args []string, flags map[string]any) (any, error) {
	t.Helper()
	if flags == nil {
		flags = map[string]any{}
	}
	in := &command.Invocation{Path: strings.Fields(path), Args: args, Flags: flags}
	if path == "job list" {
		in.Page = &command.Page{Limit: 50}
	}
	return s.Bindings()[path](ctx, in)
}

func TestCommands(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := newService(bdb)
		defer s.Stop(context.Background())
		done, _ := s.Start(admin(), inv(), func(context.Context) (any, error) { return "ok", nil }, nil)
		failed, _ := s.Start(admin(), inv(), func(context.Context) (any, error) { return nil, errors.New("x") }, nil)
		wait(t, s, done.JobID)
		wait(t, s, failed.JobID)
		out, err := call(t, admin(), s, "job get", []string{done.JobID}, nil)
		if err != nil || out.(*core.Job).Status != core.StatusDone {
			t.Fatalf("job get：%+v %v", out, err)
		}
		if _, err := call(t, admin(), s, "job get", []string{"job-nosuch"}, nil); v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("没有的 job 应当 not_found，得到 %v", err)
		}
		out, err = call(t, admin(), s, "job list", nil, map[string]any{"status": "failed"})
		if res := out.(*command.PageResult); err != nil || res.Total != 1 || res.Items[0].(*core.Job).JobID != failed.JobID {
			t.Fatalf("按状态过滤：%+v %v", out, err)
		}
		if _, err := call(t, admin(), s, "job list", nil, map[string]any{"status": "bogus"}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("非法状态应当 bad_request，得到 %v", err)
		}
		for _, path := range []string{"job get", "job list"} {
			if _, err := call(t, user(), s, path, []string{done.JobID}, nil); v1.AsError(err).Code != v1.CodeForbidden {
				t.Fatalf("%s 普通用户应当 forbidden，得到 %v", path, err)
			}
		}
		if err := s.MarkInterrupted(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// 改 running 失败时（这一行已经不是 queued）：不跑 run、按失败结束，done 照样调一次（审查第 5 条）。
func TestDoneWhenCannotStart(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s := newService(bdb)
		defer s.Stop(context.Background())
		job, err := s.repo.Insert(context.Background(), "job-00000000000000aa", "backup create", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.repo.SetRunning(context.Background(), job.ID, time.Now()); err != nil { // 让 SetRunning 的前置条件不满足
			t.Fatal(err)
		}
		ran, done := false, 0
		s.wg.Add(1)
		func() {
			defer s.wg.Done()
			defer func() { done++ }()
			s.execute(admin(), job, func(context.Context) (any, error) { ran = true; return nil, nil })
		}()
		if ran || done != 1 {
			t.Fatalf("改不成 running 时不应当跑 run，done 应当调一次：ran=%v done=%d", ran, done)
		}
	})
}
