// Package backup 是业务层的整库备份与恢复（master-backup）：backup create / list / upload / download / restore 五条命令、
// 初始化向导的 setup restore、同一时刻只允许一次备份或恢复，以及恢复之后的收尾（作废并重发恢复码、last_restore、审计）。
// 打包、校验、换库换文件在基础设施层的 base/backup；恢复本身在下次启动时做（serve 调 base/backup.Startup）。
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	archive "github.com/satchel/satchel/internal/base/backup"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/command"
	coreaudit "github.com/satchel/satchel/internal/core/audit"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// maxUpload 是上传一份备份的上限（4 GiB；变量只为测试能调小）。
var maxUpload int64 = 4 << 30

// Deps 是本服务要的东西。StartJob、RequestStop、GenerateCodes 由装配根注入：业务层的模块之间不互相引用。
type Deps struct {
	DataDir  string
	DB       *bun.DB
	Driver   db.Driver
	PG       *archive.PGConn
	Version  string
	Users    *users.Repo
	Settings *coresettings.Repo
	Audit    *coreaudit.Repo
	Logger   *slog.Logger
	// StartJob 受理一个长任务（service/jobs 的 Start）：done 在 job 结束后一定调一次（含没能开始的情况）。
	StartJob func(ctx context.Context, inv *command.Invocation, run func(context.Context) (any, error), done func()) (any, error)
	// RequestStop 让 serve 优雅停止（发起恢复之后，由服务管理器拉起、在启动时换库）。
	RequestStop func()
	// GenerateCodes 生成一批恢复码的明文与哈希（service/auth 的 GenerateRecoveryCodes）。
	GenerateCodes func() (plain, hashes []string, err error)
}

// Service 持有依赖与「正在备份或恢复」的状态。
type Service struct {
	d   Deps
	now func() time.Time

	mu   sync.Mutex
	busy string // 正在进行的备份或恢复；空表示没有
}

// New 建服务。
func New(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{d: d, now: time.Now}
}

// Bindings 是五条 backup 命令的处理函数（setup restore 经 SetupRestore 交给 service/auth）。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"backup create": s.create, "backup list": s.list, "backup upload": s.upload,
		"backup download": s.download, "backup restore": s.restore,
	}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "备份与恢复只对管理员开放")
	}
	return nil
}

// begin 占住「正在备份或恢复」；已有一个在进行、或已有待恢复标记（主控正要重启去恢复）时是 conflict。
func (s *Service) begin(what string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy != "" {
		return v1.Newf(v1.CodeConflict, "已有一次%s在进行，同一时刻只能有一次备份或恢复", s.busy).WithNext("等它结束（job list 可以看）再做")
	}
	if m, err := archive.ReadMarker(s.d.DataDir); err != nil {
		return err
	} else if m != nil {
		return v1.New(v1.CodeConflict, "已经发起了一次恢复，主控正要重启去执行").WithNext("等主控重启完成")
	}
	s.busy = what
	return nil
}

func (s *Service) end() {
	s.mu.Lock()
	s.busy = ""
	s.mu.Unlock()
}

// Busy 报告此刻是否有备份或恢复在进行（backup_local 任务据此跳过）。
func (s *Service) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy != ""
}

func (s *Service) source() archive.Source {
	return archive.Source{DataDir: s.d.DataDir, DB: s.d.DB, Driver: s.d.Driver, PG: s.d.PG, Version: s.d.Version}
}

// CreateLocal 生成一份本机备份并按保留份数清理（backup create 的工作与 backup_local 任务共用）。
func (s *Service) CreateLocal(ctx context.Context) (archive.Info, error) {
	info, err := archive.Create(ctx, s.source(), archive.PrefixBackup)
	if err != nil {
		return info, err
	}
	s.prune(info.Name)
	return info, nil
}

// createJob 是 backup create 这个长任务的工作。
func (s *Service) createJob(ctx context.Context) (any, error) { return s.CreateLocal(ctx) }

// prune 按保留份数清理，保护刚放进去的那份，以及待恢复标记里点名的那份（master-backup「本机备份目录与保留」）。
func (s *Service) prune(keep string) {
	protect := []string{keep}
	if m, err := archive.ReadMarker(s.d.DataDir); err == nil && m != nil {
		protect = append(protect, m.Backup)
	}
	if err := archive.Prune(s.d.DataDir, archive.Keep, protect...); err != nil {
		s.d.Logger.Warn("清理旧备份失败", "error", err)
	}
}

// LatestAge 是 backups/ 里最新一份备份距今多久；没有备份时 ok 为 false。
func (s *Service) LatestAge() (time.Duration, bool, error) {
	list, err := archive.List(s.d.DataDir)
	if err != nil || len(list) == 0 {
		return 0, false, err
	}
	fi, err := os.Stat(filepath.Join(archive.Dir(s.d.DataDir), list[0].Name))
	if err != nil {
		return 0, false, err
	}
	return s.now().Sub(fi.ModTime()), true, nil
}

// TryCreateLocal 是 backup_local 任务的一次运行：已有备份或恢复在进行时跳过（skipped 为真），否则生成一份。
func (s *Service) TryCreateLocal(ctx context.Context) (info archive.Info, skipped bool, err error) {
	if err := s.begin("备份"); err != nil {
		return info, true, nil
	}
	defer s.end()
	info, err = s.CreateLocal(ctx)
	return info, false, err
}

func (s *Service) create(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := s.begin("备份"); err != nil {
		return nil, err
	}
	job, err := s.d.StartJob(ctx, inv, s.createJob, s.end)
	if err != nil {
		s.end()
	}
	return job, err
}

func (s *Service) list(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	if err := page.Normalize(); err != nil {
		return nil, err
	}
	offset, err := command.DecodeOffsetCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	all, err := archive.List(s.d.DataDir)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: []any{}, Total: len(all)}
	for i := offset; i < len(all) && i < offset+page.Limit; i++ {
		res.Items = append(res.Items, all[i])
	}
	if offset+page.Limit < len(all) {
		res.NextCursor = command.EncodeOffsetCursor(offset + page.Limit)
	}
	return res, nil
}

// receive 把上传的请求体边收边写到 backups/ 里的临时文件，超过 MaxUpload 删掉并 bad_request；收完校验，通过就改名成
// uploaded-<时间>.zip、按保留份数清理（保护这一份），返回它的信息。
func (s *Service) receive(inv *command.Invocation) (archive.Info, error) {
	if inv.Body == nil {
		return archive.Info{}, v1.New(v1.CodeBadRequest, "没有收到备份文件")
	}
	dir := archive.Dir(s.d.DataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return archive.Info{}, v1.Wrap(v1.CodeInternal, "建备份目录失败", err)
	}
	tmp, err := os.CreateTemp(dir, ".upload-*.zip")
	if err != nil {
		return archive.Info{}, v1.Wrap(v1.CodeInternal, "建上传的临时文件失败", err)
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(inv.Body, maxUpload+1))
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return archive.Info{}, v1.Wrap(v1.CodeBadRequest, "接收备份文件失败", err)
	}
	if n > maxUpload {
		return archive.Info{}, v1.Newf(v1.CodeBadRequest, "备份文件超过 %d 字节的上限（4 GiB）", maxUpload)
	}
	m, err := archive.Validate(tmp.Name(), s.d.Driver)
	if err != nil {
		return archive.Info{}, err
	}
	name := archive.NewName(dir, archive.PrefixUploaded, s.now())
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return archive.Info{}, v1.Wrap(v1.CodeInternal, "保存上传的备份失败", err)
	}
	if err := archive.SyncDir(dir); err != nil {
		return archive.Info{}, v1.Wrap(v1.CodeInternal, "保存上传的备份失败", err)
	}
	s.prune(name)
	created := m.CreatedAt
	return archive.Info{Name: name, Size: n, CreatedAt: &created, Driver: m.Driver}, nil
}

// upload 也占住「正在备份或恢复」：上传末尾的保留清理不能和一次正在进行的恢复、备份交错（审查第 4 条）。
func (s *Service) upload(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := s.begin("上传"); err != nil {
		return nil, err
	}
	defer s.end()
	return s.receive(inv)
}

func (s *Service) download(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	path, err := archive.Path(s.d.DataDir, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, v1.Wrap(v1.CodeNotFound, "备份 "+inv.Arg(0)+" 不在了", err)
	}
	return &command.File{Name: inv.Arg(0), ContentType: "application/zip", Size: fi.Size(),
		Open: func() (io.ReadCloser, error) { return os.Open(path) }}, nil
}

// RestartResult 是发起恢复的输出。
type RestartResult struct {
	Restarting bool   `json:"restarting"`
	Backup     string `json:"backup"`
}

// schedule 校验备份、写待恢复标记、让 serve 优雅停止（回应照常发完）。调用方已经 begin 过；成功后不再 end：进程要退出了。
func (s *Service) schedule(ctx context.Context, name, source string) (*RestartResult, error) {
	path, err := archive.Path(s.d.DataDir, name)
	if err != nil {
		return nil, err
	}
	if _, err := archive.Validate(path, s.d.Driver); err != nil {
		return nil, err
	}
	m := &archive.Marker{Backup: name, Source: source, Actor: v1.IdentityFrom(ctx).Actor, RequestedAt: s.now().UTC(), Phase: archive.PhasePending}
	if err := archive.WriteMarker(s.d.DataDir, m); err != nil {
		return nil, err
	}
	s.d.Logger.Warn("已发起从备份恢复，主控现在停止，重启后执行", "backup", name, "source", source, "actor", m.Actor)
	if s.d.RequestStop != nil {
		s.d.RequestStop()
	}
	return &RestartResult{Restarting: true, Backup: name}, nil
}

func (s *Service) restore(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := s.begin("恢复"); err != nil {
		return nil, err
	}
	res, err := s.schedule(ctx, inv.Arg(0), archive.SourceManual)
	if err != nil {
		s.end()
	}
	return res, err
}

// SetupRestore 是初始化向导的恢复（service/auth 的 setup restore 在确认库里没有用户之后调）：收下上传的备份、校验、
// 写 source 为 setup 的待恢复标记、让 serve 停止。收文件可能要很久：收完之后、写标记之前再数一次用户，这期间有人建了
// 管理员就作废这次上传（审查第 3 条；收的过程中 setup init 也会被 SetupGuard 挡住）。失败时删掉已经收下的文件。
func (s *Service) SetupRestore(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := s.begin("恢复"); err != nil {
		return nil, err
	}
	info, err := s.receive(inv)
	if err == nil {
		var n int
		if n, err = s.d.Users.Count(ctx); err == nil && n > 0 {
			err = v1.New(v1.CodeConflict, "收文件期间主控已经初始化（库里有了用户），初始化向导不能再恢复备份").
				WithNext("管理员用 backup upload 与 backup restore 恢复（要当场验证）")
		}
		if err == nil {
			var res *RestartResult
			if res, err = s.schedule(ctx, info.Name, archive.SourceSetup); err == nil {
				return res, nil
			}
		}
		os.Remove(filepath.Join(archive.Dir(s.d.DataDir), info.Name))
	}
	s.end()
	return nil, err
}

// SetupGuard 给 setup init 用：初始化向导正在恢复备份（收文件中，或已发起、主控正要重启）时是 conflict。
func (s *Service) SetupGuard() error {
	s.mu.Lock()
	busy := s.busy
	s.mu.Unlock()
	if busy == "恢复" {
		return v1.New(v1.CodeConflict, "初始化向导正在从备份恢复，不能同时建管理员").WithNext("等恢复完成（主控会重启）")
	}
	if m, err := archive.ReadMarker(s.d.DataDir); err != nil {
		return err
	} else if m != nil {
		return v1.New(v1.CodeConflict, "已经发起了一次恢复，主控正要重启去执行").WithNext("等主控重启完成")
	}
	return nil
}

// LastRestore 是运行态 key last_restore 的内容（master-backup「恢复之后的收尾」）。
type LastRestore struct {
	Source            string    `json:"source"`
	Backup            string    `json:"backup"`
	At                time.Time `json:"at"`
	Result            string    `json:"result"`
	Error             string    `json:"error,omitempty"`
	RecoveryCodesFile string    `json:"recovery_codes_file,omitempty"`
}

// Finalize 是恢复之后的收尾（serve 迁移之后、开始监听之前调；m 是 base/backup.Startup 返回的标记，nil 时什么都不做）：
// restored 时在一个事务里作废全部恢复码、给开了两步验证的账号换新、写 last_restore、插审计；提交后写恢复码明文文件，
// 最后删标记。中途崩溃时标记还在，下次从头再来。failed 时只写 last_restore 与一条失败的审计，再删标记。
func (s *Service) Finalize(ctx context.Context, m *archive.Marker) error {
	if m == nil || (m.Phase != archive.PhaseRestored && m.Phase != archive.PhaseFailed) {
		return nil
	}
	now := s.now().UTC()
	last := LastRestore{Source: m.Source, Backup: m.Backup, At: now, Result: "ok"}
	actor := m.Actor
	if actor == "" {
		actor = "system"
	}
	var codes map[string][]string
	var codesFile string
	if m.Phase == archive.PhaseRestored {
		codesFile = filepath.Join(s.d.DataDir, db.RecoveryCodesDir, "recovery-codes-"+now.Format("20060102T150405Z")+".txt")
		last.RecoveryCodesFile = codesFile
	} else {
		last.Result, last.Error = "failed", m.Error
	}
	err := s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if m.Phase == archive.PhaseRestored {
			var err error
			if codes, err = s.d.Users.ResetRecoveryCodes(ctx, tx, s.d.GenerateCodes); err != nil {
				return err
			}
		}
		raw, _ := json.Marshal(last)
		if err := s.d.Settings.WriteStatus(ctx, tx, map[string]any{"last_restore": json.RawMessage(raw)}); err != nil {
			return err
		}
		digest, _ := json.Marshal(map[string]any{"args": []string{m.Backup}, "flags": map[string]string{"source": m.Source}})
		return s.d.Audit.InsertTx(ctx, tx, coreaudit.Record{At: now, Actor: actor, ActorKind: v1.ActorSystem, Command: "backup restore",
			ArgsDigest: string(digest), Result: map[string]string{"ok": "ok", "failed": "error"}[last.Result]})
	})
	if err != nil {
		return err
	}
	if m.Phase == archive.PhaseRestored {
		if err := writeCodes(codesFile, codes); err != nil {
			return err
		}
		s.d.Logger.Warn("已从备份恢复：全部恢复码作废，开了两步验证的账号的新恢复码在这个文件里，用完请删掉", "file", codesFile, "backup", m.Backup)
	}
	return archive.RemoveMarker(s.d.DataDir)
}

// writeCodes 写恢复码明文文件（0600，每个账号一行：账号名与它的几枚码），先写临时文件再改名。
func writeCodes(path string, codes map[string][]string) error {
	names := make([]string, 0, len(codes))
	for n := range codes {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# 从备份恢复之后的新恢复码（旧码全部作废）。每个账号一行，每枚码只能用一次；用完请删掉这个文件。\n")
	for _, n := range names {
		fmt.Fprintf(&b, "%s: %s\n", n, strings.Join(codes[n], " "))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return v1.Wrap(v1.CodeInternal, "建恢复码目录失败", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".recovery-codes-*")
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "写恢复码文件失败", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err == nil {
		// 明文是唯一的一份：目录项落盘之后才能删标记，断电也不会出现「库里换了、文件没了」。
		err = archive.SyncDir(filepath.Dir(path))
	}
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "写恢复码文件失败", err)
	}
	return nil
}
