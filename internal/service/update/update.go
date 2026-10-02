// Package update 是业务层的主控自升级（master-self-update）：update check 与 update apply 两条命令。
// 升级是一个长任务：成对下载验签 → 试跑新二进制 → 暂停写入 → 升级前备份 → 写升级标记 → 留 .bak 并原子替换 → 让 serve 优雅停止后 exec
// 新二进制。exec 之后的健康检查、成功收尾与失败回退在 cmd/satchel 的启动流程里（按数据目录里的升级标记）。机制在 base/selfupdate。
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/selfupdate"
	"github.com/satchel/satchel/internal/command"
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Deps 是本服务要的东西。Lock、Unlock、CreateBackup、StartJob、RequestExec 由装配根注入：业务层的模块之间不互相引用。
type Deps struct {
	DataDir string
	// Version 是正在运行的二进制的版本（buildinfo.Version）。
	Version  string
	Settings *coresettings.Repo
	Sources  selfupdate.Sources
	// Verify 验一对二进制与签名（产品里是 pkg/release.VerifyFile：正在运行的二进制是信任锚）。
	Verify func(bin, sig string) error
	// Root 是判断 Docker 用的根目录（产品里是 "/"）；GOOS、GOARCH 是这台主控的平台。
	Root         string
	GOOS, GOARCH string
	// Executable 返回目标路径：正在运行的二进制解开符号链接后的路径。
	Executable func() (string, error)
	// RunVersion 运行 path 上的二进制的 version --json，返回它报的版本号。为空时用 exec。
	RunVersion func(ctx context.Context, path string) (string, error)
	// Lock、Unlock 是 service/backup 的「同一时刻只有一次」。
	Lock   func(what string) error
	Unlock func()
	// CreateBackup 生成一份 before-upgrade-<时间>.zip，返回它的名字（调用方已占住锁）。
	CreateBackup func(ctx context.Context) (string, error)
	Gate         *db.WriteGate
	StartJob     func(ctx context.Context, inv *command.Invocation, run func(ctx context.Context) (any, error), done func()) (any, error)
	// RequestExec 让 serve 优雅停止，然后 exec path 上的二进制。
	RequestExec func(path string)
	Logger      *slog.Logger
}

// Service 是自升级的业务。
type Service struct {
	d   Deps
	now func() time.Time
}

// New 建服务。
func New(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.RunVersion == nil {
		d.RunVersion = runVersion
	}
	return &Service{d: d, now: time.Now}
}

// Bindings 是本服务提供的命令处理函数。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"update check": s.check,
		"update apply": s.apply,
	}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "自升级只对管理员开放")
	}
	return nil
}

// CheckResult 是 update check 的输出（master-self-update「检查更新」）。
type CheckResult struct {
	CurrentVersion string               `json:"current_version"`
	LatestVersion  string               `json:"latest_version"`
	HasUpdate      bool                 `json:"has_update"`
	Channel        string               `json:"channel"`
	Prerelease     bool                 `json:"prerelease"`
	ReleaseURL     string               `json:"release_url"`
	Notes          string               `json:"notes"`
	Source         string               `json:"source"`
	CDN            selfupdate.CDNStatus `json:"cdn"`
	CanApply       bool                 `json:"can_apply"`
	Reason         string               `json:"reason,omitempty"`
}

// IsDev 报告 v 是不是开发版构建的版本号：不合法，或是直接 go build 时的默认值那种 -dev 结尾的。开发版不能自升级。
func IsDev(v string) bool { return !selfupdate.Valid(v) || strings.HasSuffix(v, "-dev") }

// blocked 返回这台主控不能自升级的原因（能升时为 nil）：不是 Linux、在 Docker 里、开发版构建。
func (s *Service) blocked() *v1.Error {
	switch {
	case s.d.GOOS != "linux":
		return v1.Newf(v1.CodeConflict, "主控只在 Linux 上运行，%s 上不能自升级", s.d.GOOS)
	case selfupdate.InDocker(s.d.Root):
		return v1.New(v1.CodeConflict, "Docker 部署不能在容器里替换主控二进制，升级要换镜像 tag").
			WithNext("在 compose 文件所在的目录把 .env 里 SATCHEL_IMAGE 的 tag 改成新版本号（latest 不用改），再 docker compose pull && docker compose up -d")
	case IsDev(s.d.Version):
		return v1.Newf(v1.CodeConflict, "当前是开发版构建（%s），不能自升级", s.d.Version).
			WithNext("用 install.sh 装一个正式发布的版本")
	}
	return nil
}

func channelOf(inv *command.Invocation) (string, error) {
	ch := inv.String("channel", selfupdate.ChannelStable)
	if ch != selfupdate.ChannelStable && ch != selfupdate.ChannelPrerelease {
		return "", v1.Newf(v1.CodeBadRequest, "渠道只能是 stable 或 prerelease，得到 %q", ch)
	}
	return ch, nil
}

// latest 按系统设置里的 CDN 开关取所选渠道的最新版本；取不到时是 unavailable，reason 带各处的原因。
func (s *Service) latest(ctx context.Context, channel string) (*selfupdate.Release, selfupdate.CDNStatus, error) {
	st, err := s.d.Settings.Load(ctx)
	if err != nil {
		return nil, selfupdate.CDNStatus{}, err
	}
	enabled, _ := st.Values["update_cdn_enabled"].(bool)
	rel, cdn, err := selfupdate.Latest(ctx, s.d.Sources, channel, enabled)
	if err != nil {
		return nil, cdn, v1.Wrap(v1.CodeUnavailable, "取不到最新版本："+err.Error(), err).WithNext("检查主控能不能访问 GitHub，稍后再试")
	}
	return rel, cdn, nil
}

func (s *Service) check(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	channel, err := channelOf(inv)
	if err != nil {
		return nil, err
	}
	rel, cdn, err := s.latest(ctx, channel)
	if err != nil {
		return nil, err
	}
	res := &CheckResult{CurrentVersion: selfupdate.Normalize(s.d.Version), LatestVersion: rel.Version, Channel: channel, Prerelease: rel.Prerelease,
		ReleaseURL: rel.URL, Notes: rel.Notes, Source: rel.Source, CDN: cdn, CanApply: true}
	res.HasUpdate = !IsDev(s.d.Version) && selfupdate.Compare(rel.Version, s.d.Version) > 0
	if e := s.blocked(); e != nil {
		res.CanApply, res.Reason = false, e.Reason
	}
	return res, nil
}

// install 是替换这一步；变量只为测试能注入「改名成功、目录落盘失败」。
var install = selfupdate.Install

// drainTimeout 是暂停写入之后等已经进门的请求走完的上限。变量只为测试能调小。
var drainTimeout = 30 * time.Second

// Progress 是 update apply 的 job 的 progress（master-self-update「应用升级的步骤」）。
type Progress struct {
	Phase  string `json:"phase"`
	Source string `json:"source,omitempty"`
	Bytes  int64  `json:"bytes"`
	Total  int64  `json:"total"`
}

// Result 是升级成功时 job 的 output（由 exec 之后的新进程写）。
type Result struct {
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	Backup      string `json:"backup"`
}

// apply 受理之前按 master-self-update「应用升级的前提」逐条检查，任一条不过都不受理。
func (s *Service) apply(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	channel, err := channelOf(inv)
	if err != nil {
		return nil, err
	}
	if e := s.blocked(); e != nil {
		return nil, e
	}
	// 先拿锁再联网取版本：取版本最慢要几十秒，拿不到锁（已有备份、恢复、迁移或升级）就该立刻被拒，而不是带着「已进门」的登记
	// 挂在网络请求上，拖住那一次迁移或升级的 Drain。
	if err := s.d.Lock("升级"); err != nil {
		return nil, err
	}
	accepted := false
	defer func() {
		if !accepted {
			s.d.Unlock()
		}
	}()
	rel, cdn, err := s.latest(ctx, channel)
	if err != nil {
		return nil, err
	}
	if selfupdate.Compare(rel.Version, s.d.Version) <= 0 {
		return nil, v1.Newf(v1.CodeConflict, "已是最新版本：当前 %s，%s 渠道的最新是 %s", selfupdate.Normalize(s.d.Version), channel, rel.Version)
	}
	if want := selfupdate.Normalize(inv.Arg(0)); want != rel.Version {
		return nil, v1.Newf(v1.CodeConflict, "%s 渠道现在的最新版本是 %s，不是 %s", channel, rel.Version, want).
			WithNext("重新运行 satchel update check，按它报的版本号升级")
	}
	target, err := s.d.Executable()
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "找不到正在运行的主控二进制", err)
	}
	actor := v1.IdentityFrom(ctx).Actor
	job, err := s.d.StartJob(ctx, inv, func(ctx context.Context) (any, error) {
		return s.run(ctx, rel, cdn.Enabled, channel, target, actor)
	}, s.d.Unlock)
	accepted = err == nil // 受理了就由 job 结束时的 done 回调放锁
	return job, err
}

// run 是升级这个长任务的工作（master-self-update「应用升级的步骤」）。成功时返回 corejobs.ErrHandedOff：job 保持 running，
// 由 exec 之后的新进程（或回退之后的旧进程）写结局。第 1 到 6 步失败时把已经动过的东西撤回，写入照常。
func (s *Service) run(ctx context.Context, rel *selfupdate.Release, useCDN bool, channel, target, actor string) (res any, err error) {
	report := func(p Progress) { corejobs.Report(ctx, p) }
	jobID := corejobs.CurrentID(ctx)
	from := selfupdate.Normalize(s.d.Version)

	report(Progress{Phase: "downloading"})
	art, err := selfupdate.Download(ctx, s.d.Sources, rel, useCDN, s.d.GOARCH, filepath.Dir(target), s.d.Verify,
		func(source string, n, total int64) {
			report(Progress{Phase: "downloading", Source: source, Bytes: n, Total: total})
		})
	if err != nil {
		return nil, v1.Wrap(v1.CodeUnavailable, "下载或验签失败："+err.Error(), err).WithNext("稍后重试；主控上的二进制没有被改动")
	}
	defer art.Remove() // 替换成功后临时文件已经改名走了，这里删不到什么
	report(Progress{Phase: "verifying", Source: art.Source})
	if err := os.Chmod(art.Binary, 0o700); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "把下载的二进制设为可执行失败", err)
	}
	got, err := s.d.RunVersion(ctx, art.Binary)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "试跑下载的二进制失败", err)
	}
	if selfupdate.Normalize(got) != rel.Version {
		return nil, v1.Newf(v1.CodeInternal, "下载的二进制报的版本是 %s，不是 %s", got, rel.Version)
	}

	// 从这里起暂停写入，直到 exec：升级前备份之后不再收写入，回退换回这份备份时就不会丢东西（design 第 5 条）。
	s.d.Gate.Suspend(fmt.Sprintf("正在把主控从 %s 升级到 %s，这期间主控只能查长任务", from, rel.Version),
		"用 satchel job get "+jobID+" 看升级进度；升级完成后主控会原地重启")
	installed, marked := false, false
	defer func() {
		if err == nil || errors.Is(err, corejobs.ErrHandedOff) {
			return
		}
		if installed {
			if rerr := selfupdate.RestorePrevious(target); rerr != nil {
				// 目标路径上可能还是新二进制（放回的改名没做成），也可能已经是旧的（改名做成了、目录落盘失败），断电后是哪个也不确定：
				// 留着升级标记（下次启动按它处理：新版本做健康检查、不过就回退；旧版本收尾），写入继续暂停，不再收可能被回退丢掉的写入。
				s.d.Logger.Error("升级失败后放回旧二进制也失败，保留升级标记、写入保持暂停，请重启主控", "target", target, "error", rerr)
				s.d.Gate.Suspend("主控升级失败、放回旧二进制也失败，这期间主控只能查长任务", "重启主控：它会按升级标记处理")
				err = v1.Wrap(v1.CodeInternal, v1.AsError(err).Reason+"；放回旧二进制也失败，目标路径上可能是新版本也可能是旧版本，升级标记已保留", rerr).
					WithNext("重启主控：起来的是新版本就按升级标记做健康检查、不过就自动回退，是旧版本就只收尾；之后以 satchel update check 报的 current_version（主控正在跑的版本）为准")
				return
			}
		}
		if marked {
			if rerr := selfupdate.RemoveMarker(s.d.DataDir); rerr != nil {
				s.d.Logger.Error("升级失败后删除升级标记失败", "error", rerr)
			}
		}
		s.d.Gate.Resume()
	}()
	// 暂停之前已经进门的命令可能还没提交：等它们都走完再备份，否则它们的写入在备份之后、回退时会丢。
	dctx, cancel := context.WithTimeout(ctx, drainTimeout)
	err = s.d.Gate.Drain(dctx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // 主控在停止
		}
		return nil, v1.Wrap(v1.CodeUnavailable, fmt.Sprintf("暂停写入之前进门的请求 %s 内没有走完", drainTimeout), err).WithNext("稍后重试")
	}

	report(Progress{Phase: "backing_up"})
	backup, err := s.d.CreateBackup(ctx)
	if err != nil {
		return nil, err
	}
	m := &selfupdate.Marker{JobID: jobID, FromVersion: from, ToVersion: rel.Version, Channel: channel, Backup: backup,
		Target: target, Previous: selfupdate.PreviousPath(target), Actor: actor, RequestedAt: s.now().UTC(), Phase: selfupdate.PhaseSwitching}
	marked = true // 写标记时改名成功、目录落盘失败，标记也已经在了：失败时照样删
	if err = selfupdate.WriteMarker(s.d.DataDir, m); err != nil {
		return nil, err
	}

	report(Progress{Phase: "switching"})
	if err = selfupdate.SavePrevious(target); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "保留上一版二进制失败", err)
	}
	installed, err = install(art.Binary, target)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "替换主控二进制失败", err)
	}

	report(Progress{Phase: "restarting"})
	s.d.Logger.Info("主控二进制已替换，优雅停止后原地重启", "from", from, "to", rel.Version, "backup", backup)
	s.d.RequestExec(target)
	return nil, corejobs.ErrHandedOff
}

// runVersion 运行 path version --json，取 version 字段。
func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version", "--json").Output()
	if err != nil {
		return "", err
	}
	var info struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("version --json 的输出不是 JSON：%w", err)
	}
	return info.Version, nil
}
