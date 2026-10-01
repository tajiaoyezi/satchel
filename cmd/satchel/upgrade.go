package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	archive "github.com/satchel/satchel/internal/base/backup"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/selfupdate"
	"github.com/satchel/satchel/internal/command"
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	"github.com/satchel/satchel/internal/service/update"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 自升级在 exec 之后的收尾（master-self-update「健康检查与成功收尾」「失败回退」，design 第 7 条）：serve 启动时按数据目录里的
// 升级标记，与本二进制的版本比，决定是照常启动并做健康检查、回退，还是给回退后的旧版本收尾。

// maxUpgradeStarts 是新版本最多启动几次：超过还没走到健康检查通过，就回退。
const maxUpgradeStarts = 3

// healthTimeout 是新版本开始监听之后，等健康检查通过的上限；commitRetry 与 commitRetryMax 是健康检查通过之后收尾失败时
// 重试的起始间隔与最长间隔（每次翻倍）。变量只为测试能调小。
var (
	healthTimeout  = 30 * time.Second
	commitRetry    = 5 * time.Second
	commitRetryMax = 5 * time.Minute
)

// commitUpgrade 是健康检查通过之后的收尾；变量只为测试能注入失败。
var commitUpgrade = (*upgradeStart).commit

// execBinary 用同样的参数与给定的环境变量 exec path 上的二进制（PID 不变）。变量只为测试能换掉。
var execBinary = func(path string, env []string) error { return syscall.Exec(path, os.Args, env) }

// upgradeStart 是启动时读到的升级标记与该怎么处理。
type upgradeStart struct {
	dataDir string
	logger  *slog.Logger
	m       *selfupdate.Marker
	// isNew：本二进制是标记里的新版本、阶段是 switching：照常启动，失败就回退，监听后做健康检查。
	// finishOld：本二进制是标记里的旧版本、阶段是 switching 或 rolling_back：启动之后给这个 job 写 failed、删标记。
	// committed：阶段是 committed（健康检查上次已经通过），不论本二进制是哪个版本：启动之后把 done 写上、删标记，不计次、不回退、不换库。
	isNew, finishOld, committed bool
}

// prepareUpgrade 在打开库之前读升级标记。execPath 非空时不再往下启动，直接 exec 它（回退）。
func prepareUpgrade(dataDir, version string, logger *slog.Logger) (up *upgradeStart, execPath string, err error) {
	m, err := selfupdate.ReadMarker(dataDir)
	if err != nil || m == nil {
		return nil, "", err
	}
	up = &upgradeStart{dataDir: dataDir, logger: logger, m: m}
	cur := selfupdate.Normalize(version)
	switch {
	case cur == m.ToVersion && m.Phase == selfupdate.PhaseSwitching:
		m.Attempts++
		if err := selfupdate.WriteMarker(dataDir, m); err != nil {
			return nil, "", err
		}
		if m.Attempts > maxUpgradeStarts {
			path, err := up.rollback(fmt.Sprintf("新版本 %s 连续 %d 次没能启动到健康检查通过", m.ToVersion, maxUpgradeStarts))
			return nil, path, err
		}
		logger.Info("升级之后的第一次启动：照常启动，监听之后做健康检查", "from", m.FromVersion, "to", m.ToVersion, "attempt", m.Attempts)
		up.isNew = true
		return up, "", nil
	case m.Phase == selfupdate.PhaseCommitted:
		// 健康检查已经通过：升级是成功的，只补完收尾。跑的不是新版本说明有人在那之后手工换了二进制，不是回退，库也不动。
		if cur != m.ToVersion {
			logger.Warn("升级已经成功，但现在跑的不是新版本（健康检查通过之后有人换了二进制）；库是升级之后的，不会换回",
				"version", cur, "from", m.FromVersion, "to", m.ToVersion)
		}
		up.committed = true
		return up, "", nil
	case cur == m.ToVersion && m.Phase == selfupdate.PhaseRollingBack:
		path, err := up.rollback(m.Error) // 回退中途崩溃：接着做
		return nil, path, err
	case cur == m.FromVersion:
		up.finishOld = true
		return up, "", nil
	}
	logger.Error("升级标记里的版本与本二进制对不上，不猜怎么回退，删掉标记照常启动",
		"version", cur, "from", m.FromVersion, "to", m.ToVersion, "phase", m.Phase)
	return nil, "", selfupdate.RemoveMarker(dataDir)
}

// rollback 做回退的第 1 到 3 步：标记改成 rolling_back 并写原因；写 upgrade_rollback 的待恢复标记指向升级前备份；
// 把 .bak 放回目标路径。返回要 exec 的路径（目标路径上的旧二进制）。任何一步之后崩溃，重启都还认得出是在回退。
func (u *upgradeStart) rollback(reason string) (string, error) {
	m := u.m
	u.logger.Error("自升级失败，回退到旧版本与升级前的备份", "from", m.FromVersion, "to", m.ToVersion, "reason", reason, "backup", m.Backup)
	m.Phase, m.Error = selfupdate.PhaseRollingBack, reason
	if err := selfupdate.WriteMarker(u.dataDir, m); err != nil {
		return "", err
	}
	pending, err := archive.ReadMarker(u.dataDir)
	if err != nil {
		return "", err
	}
	// 已有这次回退写下、还没执行的待恢复标记（回退中途崩溃后接着做）就不重写。别的待恢复标记都覆盖掉：例如新版本启动时库损坏、
	// 自动恢复留下的（已是 restored）——不覆盖的话，旧版本看不到 pending，就不会换回升级前的库。
	if pending == nil || pending.Source != archive.SourceUpgradeRollback || pending.Backup != m.Backup || pending.Phase != archive.PhasePending {
		if err := archive.WriteMarker(u.dataDir, &archive.Marker{Backup: m.Backup, Source: archive.SourceUpgradeRollback, Actor: m.Actor,
			RequestedAt: time.Now().UTC(), Phase: archive.PhasePending}); err != nil {
			return "", err
		}
	}
	if err := selfupdate.RestorePrevious(m.Target); err != nil {
		return "", v1.Wrap(v1.CodeInternal, "回退时放回旧二进制失败", err).
			WithNext("手工把 " + m.Previous + " 改回 " + m.Target + " 后重启主控")
	}
	return m.Target, nil
}

// failStart 是启动顺序里某一步失败时的处理：本二进制是升级后的新版本时做回退并 exec 旧二进制，否则原样返回错误。
// 失败是因为收到了停止信号（ctx 取消，例如 systemctl stop / restart 或关机）时不算新版本启动失败：不回退，这次启动也不计次。
func (u *upgradeStart) failStart(ctx context.Context, err error, logFile io.Closer, lock *os.File) error {
	if u == nil || !u.isNew {
		return err
	}
	if ctx.Err() != nil {
		u.uncountIfStopped(ctx)
		return err
	}
	path, rerr := u.rollback("新版本启动失败：" + v1.AsError(err).Reason)
	if rerr != nil {
		return rerr
	}
	return execNow(path, logFile, lock)
}

// uncountIfStopped：新版本这次启动在健康检查通过之前就收到了停止信号（ctx 取消）时，把这次从启动次数里减掉——停止不是崩溃。
// 按磁盘上的标记判断，健康检查已经通过（标记已删或已是 committed）、或已经开始回退时不动。
func (u *upgradeStart) uncountIfStopped(ctx context.Context) {
	if u == nil || !u.isNew || ctx.Err() == nil {
		return
	}
	m, err := selfupdate.ReadMarker(u.dataDir)
	if err != nil || m == nil || m.Phase != selfupdate.PhaseSwitching || m.Attempts <= 0 {
		return
	}
	m.Attempts--
	if err := selfupdate.WriteMarker(u.dataDir, m); err != nil {
		u.logger.Error("停止时把这次启动从次数里减掉失败", "error", err)
	}
}

// withUpgradeHint：数据目录里有升级标记时，启动失败的错误加上提示——库可能已经被新版本迁移过（旧二进制会报结构不一致），
// 不要按结构不一致的提示删库，按升级标记与 README 处理。新版本自己启动失败时通常已经回退并 exec 了（err 为 nil），
// 没回退成（收到停止信号、回退本身失败）时同样加上。
func (u *upgradeStart) withUpgradeHint(err error) error {
	if err == nil || u == nil {
		return err
	}
	e := *v1.AsError(err)
	hint := fmt.Sprintf("数据目录里有升级标记（%s → %s，阶段 %s）：库可能已被新版本迁移过，不要删库；按 README「升级主控与更新 CDN」一节处理（升级前的备份是 backups/%s）。",
		u.m.FromVersion, u.m.ToVersion, u.m.Phase, u.m.Backup)
	if e.Code == v1.CodeSchemaMismatch || e.Next == "" {
		e.Next = hint // 结构不一致原来的提示是开发期的「删掉库重新迁移」，这里绝不能照做
	} else {
		e.Next = hint + e.Next
	}
	return &e
}

// execNow 关掉日志文件，exec path 上的二进制，数据目录的锁跟着过去；只有 exec 失败时才返回（以非 0 退出，由服务管理器拉起
// 目标路径上的二进制）。
func execNow(path string, logFile io.Closer, lock *os.File) error {
	logFile.Close()
	if err := execBinary(path, execEnv(lock)); err != nil {
		return v1.Wrap(v1.CodeInternal, "exec "+path+" 失败", err)
	}
	return nil
}

// settle 写这个 job 的结局；它已经不是 running（上一次写上了、之后才崩溃），或者这一行已经不在（超过保留期被清理）时，
// 当作已经写好。also 是除 running 之外还允许改写的状态（见 commit）。
func (u *upgradeStart) settle(ctx context.Context, a *app, result any, runErr error, also ...string) error {
	err := a.jobs.Settle(ctx, u.m.JobID, result, runErr, also...)
	if err != nil && v1.AsError(err).Code != v1.CodeConflict && !errors.Is(err, corejobs.ErrNotFound) {
		return err
	}
	return nil
}

// finish 在开始监听之前收尾：旧版本（回退已按待恢复标记换回旧库）给这个 job 写 failed；新版本上次健康检查已通过的写 done。
// 结局写上之后才删升级标记：写不上时标记留着、返回错误，下次启动再做一遍。
func (u *upgradeStart) finish(ctx context.Context, a *app) error {
	if u == nil || !(u.finishOld || u.committed) {
		return nil
	}
	m := u.m
	if u.committed {
		// 与 commit 相同：job 可能已被旧进程写成 failed（放回旧二进制也失败），升级其实成功了，改写成 done。
		if err := u.settle(ctx, a, update.Result{FromVersion: m.FromVersion, ToVersion: m.ToVersion, Backup: m.Backup}, nil, corejobs.StatusFailed); err != nil {
			return err
		}
		u.logger.Info("自升级成功（上次启动已通过健康检查，这次补完收尾）", "from", m.FromVersion, "to", m.ToVersion)
		return selfupdate.RemoveMarker(u.dataDir)
	}
	reason := m.Error
	if m.Phase == selfupdate.PhaseSwitching {
		reason = "升级没有完成切换（在切换之前中断，或替换之后又放回了旧二进制）"
	}
	e := v1.New(v1.CodeInternal, "升级失败，已回到 "+m.FromVersion+"："+reason).
		WithState("from_version", m.FromVersion).WithState("to_version", m.ToVersion).
		WithNext("看主控日志找原因；修好之后可以再 satchel update check")
	if err := u.settle(ctx, a, nil, e); err != nil {
		return err
	}
	u.logger.Warn("自升级没有成功，主控仍是旧版本", "from", m.FromVersion, "to", m.ToVersion, "reason", reason)
	return selfupdate.RemoveMarker(u.dataDir)
}

// watchHealth 在新版本开始监听之后做健康检查（waitHealthy）。通过就收尾（commit）：先把标记改成 committed（之后再崩溃也不会
// 回退），再写 done、删标记；收尾某一步失败就隔 commitRetry 重试，直到做完或进程停止，免得标记长期留着挡住备份与再次升级。
// 健康检查不过就回退（优雅停止后 exec 旧二进制）。回退本身失败时暂停写入并以错误停下：服务管理器会拉起，下次启动按标记接着处理，
// 这期间不再收会被回退丢掉的写入。
func (u *upgradeStart) watchHealth(ctx context.Context, a *app, tcp net.Addr) {
	if u == nil || !u.isNew {
		return
	}
	m := u.m
	if err := waitHealthy(ctx, filepath.Join(u.dataDir, db.SocketFile), tcp, healthTimeout); err != nil {
		if ctx.Err() != nil {
			return // 已经在停止：下次启动按标记接着做
		}
		path, rerr := u.rollback("新版本的健康检查不过：" + err.Error())
		if rerr != nil {
			u.logger.Error("回退失败，暂停写入并停止；下次启动按升级标记接着处理", "phase", m.Phase, "error", rerr)
			a.writeGate.Suspend("主控升级失败、回退没有做完，这期间主控只能查长任务", "看主控日志；重启主控会按升级标记接着处理")
			re := v1.AsError(rerr)
			cause := errors.Unwrap(rerr)
			if cause == nil {
				cause = rerr
			}
			next := re.Next
			if next == "" {
				next = "重启主控，它会按升级标记接着处理"
			}
			a.failStop(v1.Wrap(v1.CodeInternal, "自升级的健康检查不过，回退也失败："+re.Reason, cause).WithNext(next))
			return
		}
		a.requestExec(path)
		return
	}
	wait := commitRetry
	for {
		err := commitUpgrade(u, ctx, a)
		if err == nil {
			u.logger.Info("自升级成功", "from", m.FromVersion, "to", m.ToVersion, "backup", m.Backup)
			return
		}
		if ctx.Err() != nil {
			return // 在停止：下次启动按标记接着收尾
		}
		u.logger.Error("新版本已通过健康检查，收尾失败，稍后重试", "phase", m.Phase, "job_id", m.JobID, "retry_in", wait, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, commitRetryMax)
	}
}

// commit 是健康检查通过之后的收尾：标记改成 committed → 放开写入 → 写 done → 删标记。每一步都可以重做。
// 写入在新版本启动时就暂停了（serveWith），committed 落盘之后不会再回退，这才放开。
// job 也可能已经被旧进程写成了 failed：替换之后放回旧二进制也失败时（service/update），旧进程先写了 failed、留着升级标记，
// 重启之后起来的是新版本并通过了健康检查——升级其实成功了，按升级标记把它改写成 done。
func (u *upgradeStart) commit(ctx context.Context, a *app) error {
	m := u.m
	if m.Phase != selfupdate.PhaseCommitted {
		next := *m
		next.Phase = selfupdate.PhaseCommitted
		if err := selfupdate.WriteMarker(u.dataDir, &next); err != nil {
			return err
		}
		m.Phase = selfupdate.PhaseCommitted
	}
	a.writeGate.Resume()
	if err := u.settle(ctx, a, update.Result{FromVersion: m.FromVersion, ToVersion: m.ToVersion, Backup: m.Backup}, nil, corejobs.StatusFailed); err != nil {
		return err
	}
	return selfupdate.RemoveMarker(u.dataDir)
}

// waitHealthy 是新版本的健康检查，timeout 内两步都过才算健康：
//  1. 经数据目录里的 unix socket 请求 healthz，要 200。走 socket 是因为 TCP 上的请求要过门（关闭公网访问、反代登记、主控域名），
//     门的 403 不说明新版本不健康；socket 是本机管理员的入口，门放行。
//  2. 再向 TCP 监听（监听全部地址时用 127.0.0.1）请求一次 healthz：只要拿到 HTTP 回应、状态码低于 500 就算通（门的 403 也算），
//     证明 TCP 这条路（门、来源解析、监听）没有 panic 或挂死；连不上、断开或 5xx 才算不通。
func waitHealthy(ctx context.Context, sock string, tcp net.Addr, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	viaSocket := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	viaTCP := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}} // 不走 HTTP_PROXY：请求的是本机的监听
	socketURL := "http://satchel" + command.APIPrefix + "healthz"
	tcpURL := "http://" + loopbackOf(tcp) + command.APIPrefix + "healthz"
	socketOK := false
	var last error
	for {
		url, client := socketURL, viaSocket
		if socketOK {
			url, client = tcpURL, viaTCP
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			switch {
			case !socketOK && resp.StatusCode == http.StatusOK:
				socketOK = true
				continue
			case socketOK && resp.StatusCode < http.StatusInternalServerError:
				return nil
			}
			err = fmt.Errorf("%s 返回 HTTP %d", url, resp.StatusCode)
		}
		last = err
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("%s 内健康检查没有通过", timeout), last)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// loopbackOf 是 TCP 监听地址在本机上的访问地址：监听全部地址（0.0.0.0、[::]）时用 127.0.0.1。
func loopbackOf(tcp net.Addr) string {
	addr, ok := tcp.(*net.TCPAddr)
	if !ok {
		return tcp.String()
	}
	host := addr.IP.String()
	if addr.IP == nil || addr.IP.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(addr.Port))
}
