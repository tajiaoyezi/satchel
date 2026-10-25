package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	coreaudit "github.com/satchel/satchel/internal/core/audit"
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	coresecurity "github.com/satchel/satchel/internal/core/security"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	"github.com/satchel/satchel/internal/core/sessions"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	"github.com/satchel/satchel/internal/core/taskruns"
	"github.com/satchel/satchel/internal/core/users"
	"github.com/satchel/satchel/internal/service/audit"
	"github.com/satchel/satchel/internal/service/auth"
	svcbackup "github.com/satchel/satchel/internal/service/backup"
	"github.com/satchel/satchel/internal/service/jobs"
	"github.com/satchel/satchel/internal/service/schedule"
	"github.com/satchel/satchel/internal/service/security"
	"github.com/satchel/satchel/internal/service/servers"
)

// master-scheduler「PostgreSQL 下没有检查点任务」与「列出任务」：SQLite 下八个，PostgreSQL 下没有 db_checkpoint；每个都能跑一次。
func TestTasksPerDriver(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		st := store.New(bdb, schema.Default())
		dataDir := t.TempDir()
		db.EnsureDataDir(dataDir)
		// backups/ 里放一份刚生成的（假）备份：backup_local 这一轮跳过，不用真的导出（PostgreSQL 的导出要 pg_dump）。
		os.WriteFile(filepath.Join(dataDir, db.BackupsDir, "satchel-backup-x.zip"), []byte("x"), 0o600)
		settingsRepo := coresettings.New(bdb, st, schema.Default())
		if err := settingsRepo.EnsureSingleton(context.Background()); err != nil {
			t.Fatal(err)
		}
		srvs := servers.New(coreservers.New(bdb, st), settingsRepo, nil, nil)
		srvs.SetConns(noConns{})
		tasks := Tasks(Deps{
			DB:       bdb,
			Auth:     auth.New(users.New(bdb, st), sessions.New(bdb)),
			Audit:    audit.New(coreaudit.New(bdb, st)),
			Security: security.New(coresecurity.New(bdb), nil),
			Schedule: schedule.New(taskruns.New(bdb), nil),
			Backup:   svcbackup.New(svcbackup.Deps{DataDir: dataDir, DB: bdb}),
			Jobs:     jobs.New(corejobs.New(bdb, st), command.Catalog(), nil, nil),
			Servers:  srvs,
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
		want := []string{"session_cleanup", "audit_cleanup", "security_event_cleanup", "task_run_cleanup", "ban_sweep", "login_limit_sweep", "backup_local", "job_cleanup", "server_offline_sweep", "server_traffic_collect"}
		if db.DialectOf(bdb) == schema.SQLite {
			want = append(want, "db_checkpoint")
		}
		want = append(want, "db_health")
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("任务清单应当是 %v，得到 %v", want, names)
		}
	})
}

// noConns 是没有任何节点连着的连接管理。
type noConns struct{}

func (noConns) Call(context.Context, int64, string, any, time.Duration) (json.RawMessage, error) {
	return nil, agentconn.ErrOffline
}

func (noConns) ConnectedIDs() []int64 { return nil }

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

// backup_local 的跳过：最新一份不到 20 小时跳过；没有备份时生成一份（SQLite）。
func TestBackupLocal(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	db.EnsureDataDir(dataDir)
	b := svcbackup.New(svcbackup.Deps{DataDir: dataDir, DB: bdb, Driver: db.DriverSQLite, Version: "test"})
	run := backupLocal(b)
	detail, err := run(context.Background())
	if err != nil || !strings.HasPrefix(detail, "生成 satchel-backup-") {
		t.Fatalf("没有备份时应当生成一份：%q %v", detail, err)
	}
	if detail, err = run(context.Background()); err != nil || !strings.Contains(detail, "跳过") {
		t.Fatalf("刚生成过应当跳过：%q %v", detail, err)
	}
}

// master-scheduler「心跳过期的服务器被标离线」：心跳早于离线阈值的那台变离线，阈值之内的那台还在线，detail 写标了几台。
func TestServerOfflineSweep(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		repo := coreservers.New(bdb, store.New(bdb, schema.Default()))
		ctx := context.Background()
		stale, _ := repo.Create(ctx, "stale", "websocket")
		fresh, _ := repo.Create(ctx, "fresh", "websocket")
		now := time.Now().UTC().Truncate(time.Microsecond)
		_ = repo.MarkOnline(ctx, stale.Metadata.ID, "", false, time.Time{}, now.Add(-2*time.Minute))
		_ = repo.MarkOnline(ctx, fresh.Metadata.ID, "", false, time.Time{}, now.Add(-10*time.Second))
		run := servers.New(repo, nil, nil, nil).SweepOffline(90*time.Second, func() time.Time { return now })
		detail, err := run(ctx)
		if err != nil || detail != "标为离线 1 台" {
			t.Fatalf("应当标为离线 1 台：%q %v", detail, err)
		}
		if s, _ := repo.ByName(ctx, "stale"); s.Status.Status != coreservers.StatusOffline || s.Status.OfflineSince == nil {
			t.Fatalf("心跳过期的应当离线：%+v", s.Status)
		}
		if s, _ := repo.ByName(ctx, "fresh"); s.Status.Status != coreservers.StatusConnected {
			t.Fatalf("阈值之内的应当还在线：%+v", s.Status)
		}
	})
}
