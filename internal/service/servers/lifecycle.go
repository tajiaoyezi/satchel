package servers

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/release"
	"github.com/satchel/satchel/pkg/xrpc"
)

const (
	defaultUpdatePollInterval = 2 * time.Second
	defaultUpdatePollTimeout  = 90 * time.Second
	defaultUpdateRPCTimeout   = 60 * time.Second
	lifecycleParallel         = 8
)

// 节点发行版默认发布源；测试二进制可通过 -ldflags -X 注入测试地址，严禁环境变量覆盖。
var (
	AgentCDNBase    = release.DefaultCDNBase
	AgentGitHubRepo = "tajiaoyezi/satchel-agent"
	AgentGitHubAPI  = release.DefaultGitHubAPI
	AgentGitHubWeb  = release.DefaultGitHubWeb
	AgentGHProxy    = release.DefaultGHProxy
)

func defaultAgentReleaseSources() release.Sources {
	return release.Sources{
		CDN:     AgentCDNBase,
		Project: "satchel-agent",
		Repo:    AgentGitHubRepo,
		API:     AgentGitHubAPI,
		Web:     AgentGitHubWeb,
		Proxy:   AgentGHProxy,
	}
}

// SetMasterVersion 注入当前主控版本号，用于比较节点新版本要求的最底主控版本。
func (s *Service) SetMasterVersion(v string) { s.masterVersion = strings.TrimSpace(v) }

// SetReleaseSources 覆盖节点发布源配置（测试用于指向伪造的三源服务端）。
func (s *Service) SetReleaseSources(src release.Sources) { s.releaseSources = src }

// SetStartJob 注入关联 server_id 的长任务启动器（装配根把 jobsSvc.StartForServer 接进来）。
func (s *Service) SetStartJob(fn func(ctx context.Context, serverID int64, inv *command.Invocation, run func(ctx context.Context) (any, error), done func()) (any, error)) {
	s.startJob = fn
}

// SetUpdatePollTimings 覆盖远程升级后轮询确认窗口，仅用于测试缩短等待。
func (s *Service) SetUpdatePollTimings(interval, timeout time.Duration) {
	if interval > 0 {
		s.pollInterval = interval
	}
	if timeout > 0 {
		s.pollTimeout = timeout
	}
}

type runtimeTracker interface {
	Runtime(serverID int64) (agentconn.NodeRuntime, bool)
	RecordRuntime(serverID int64, rt agentconn.NodeRuntime)
}

type serverDisconnector interface {
	Disconnect(ctx context.Context, serverID int64) error
}

type masterSyncBlocker interface {
	SetMasterSyncBlocked(serverID int64, blocked bool)
}

func (s *Service) effectiveSources() release.Sources {
	if s.releaseSources.Repo == "" && s.releaseSources.CDN == "" && s.releaseSources.API == "" && s.releaseSources.Web == "" && s.releaseSources.Proxy == "" {
		return defaultAgentReleaseSources()
	}
	return s.releaseSources
}

func (s *Service) masterVer() string {
	if s.masterVersion == "" {
		return agentconn.DevVersion
	}
	return s.masterVersion
}

func (s *Service) tryLockUpdates(ids []int64) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.updating == nil {
		s.updating = map[int64]bool{}
	}
	for _, id := range ids {
		if s.updating[id] {
			return false
		}
	}
	for _, id := range ids {
		s.updating[id] = true
	}
	return true
}

func (s *Service) unlockUpdates(ids []int64) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	for _, id := range ids {
		delete(s.updating, id)
	}
}

func parseChannelFlag(inv *command.Invocation) (string, error) {
	ch := strings.TrimSpace(inv.String("channel", release.ChannelStable))
	if ch != release.ChannelStable && ch != release.ChannelPrerelease {
		return "", v1.Newf(v1.CodeBadRequest, "渠道只能是 stable 或 prerelease，得到 %q", ch)
	}
	return ch, nil
}

func (s *Service) fetchLatestAgent(ctx context.Context, channel string) (*release.Release, release.CDNStatus, bool, error) {
	st, err := s.settings.Load(ctx)
	if err != nil {
		return nil, release.CDNStatus{}, false, err
	}
	cdnEnabled, _ := st.Values["update_cdn_enabled"].(bool)
	rel, cdn, err := release.Latest(ctx, s.effectiveSources(), channel, cdnEnabled)
	if err != nil {
		return nil, cdn, cdnEnabled, v1.Wrap(v1.CodeUnavailable, "取不到 satchel-agent 最新版本："+err.Error(), err).
			WithNext("检查主控是否能访问发布源，或切换 update_cdn_enabled 后再试")
	}
	return rel, cdn, cdnEnabled, nil
}

func (s *Service) isReleaseCompatible(rel *release.Release) (bool, string) {
	if rel.ControlProtocol > xrpc.ControlProtocol {
		return false, fmt.Sprintf("目标版本 %s 控制协议号为 %d（当前主控仅支持 %d），请先升级主控", rel.Version, rel.ControlProtocol, xrpc.ControlProtocol)
	}
	masterV := s.masterVer()
	if rel.MinMasterVersion != "" && !release.IsDev(masterV) {
		if !release.Valid(rel.MinMasterVersion) || release.Compare(masterV, rel.MinMasterVersion) < 0 {
			return false, fmt.Sprintf("目标版本 %s 要求主控至少为 %s（当前主控为 %s），请先升级主控", rel.Version, rel.MinMasterVersion, release.Normalize(masterV))
		}
	}
	return true, ""
}

func (s *Service) recordRuntime(id int64, info xrpc.AgentInfo) {
	conns := s.getConns()
	if conns == nil {
		return
	}
	if rt, ok := conns.(runtimeTracker); ok {
		rt.RecordRuntime(id, agentconn.NodeRuntime{
			AgentVersion:     info.AgentVersion,
			InDocker:         info.InDocker,
			MachineID:        info.MachineID,
			MasterURL:        info.MasterURL,
			MinMasterVersion: info.MinMasterVersion,
			ExecRunning:      info.ExecRunning,
		})
	}
}

func (s *Service) inspectNodeInfo(ctx context.Context, srv *core.Server) (agentconn.NodeRuntime, bool, error) {
	var cached agentconn.NodeRuntime
	conns := s.getConns()
	if conns != nil {
		if tracker, ok := conns.(runtimeTracker); ok {
			if c, exists := tracker.Runtime(srv.Metadata.ID); exists {
				cached = c
			}
		}
	}
	if conns == nil {
		return cached, false, v1.Newf(v1.CodeUnavailable, "服务器 %q 现在没有连接", srv.Spec.Name)
	}
	var info xrpc.AgentInfo
	err := s.call(ctx, srv, xrpc.MethodAgentInfo, nil, &info, callTimeout)
	if err != nil {
		return cached, false, err
	}
	rt := agentconn.NodeRuntime{
		AgentVersion:     info.AgentVersion,
		InDocker:         info.InDocker,
		MachineID:        info.MachineID,
		MasterURL:        info.MasterURL,
		MinMasterVersion: info.MinMasterVersion,
		ExecRunning:      info.ExecRunning,
	}
	s.recordRuntime(srv.Metadata.ID, info)
	return rt, true, nil
}

// AgentUpdateCheckItem 是 server update check 单台服务器的版本兼容性检查结果。
type AgentUpdateCheckItem struct {
	Server         string            `json:"server"`
	Channel        string            `json:"channel"`
	CurrentVersion string            `json:"current_version"`
	LatestVersion  string            `json:"latest_version"`
	HasUpdate      bool              `json:"has_update"`
	Source         string            `json:"source"`
	CDN            release.CDNStatus `json:"cdn"`
	InDocker       bool              `json:"in_docker"`
	Compatible     bool              `json:"compatible"`
	CanApply       bool              `json:"can_apply"`
	Reachable      bool              `json:"reachable"`
	ExecRunning    bool              `json:"exec_running,omitempty"`
	Reason         string            `json:"reason,omitempty"`
}

// AgentUpdateCheckList 是 server update check 省略 <name> 时返回的多台汇总结果。
type AgentUpdateCheckList struct {
	Channel       string                 `json:"channel"`
	LatestVersion string                 `json:"latest_version"`
	Source        string                 `json:"source"`
	CDN           release.CDNStatus      `json:"cdn"`
	Items         []AgentUpdateCheckItem `json:"items"`
	Total         int                    `json:"total"`
}

func (s *Service) checkOneServer(ctx context.Context, srv *core.Server, rel *release.Release, cdn release.CDNStatus, channel string) AgentUpdateCheckItem {
	rt, reachable, _ := s.inspectNodeInfo(ctx, srv)
	curVer := strings.TrimSpace(rt.AgentVersion)
	if release.Valid(curVer) {
		curVer = release.Normalize(curVer)
	}
	compatible, compatReason := s.isReleaseCompatible(rel)
	hasUpdate := curVer != "" && !release.IsDev(curVer) && release.Compare(rel.Version, curVer) > 0

	item := AgentUpdateCheckItem{
		Server:         srv.Spec.Name,
		Channel:        channel,
		CurrentVersion: curVer,
		LatestVersion:  rel.Version,
		HasUpdate:      hasUpdate,
		Source:         rel.Source,
		CDN:            cdn,
		InDocker:       rt.InDocker,
		Compatible:     compatible,
		CanApply:       true,
		Reachable:      reachable,
		ExecRunning:    rt.ExecRunning,
	}
	switch {
	case !compatible:
		item.CanApply, item.Reason = false, compatReason
	case rt.InDocker:
		item.CanApply, item.Reason = false, "Docker 容器内不支持自更新，请更换镜像 tag 后重建容器"
	case !reachable:
		item.CanApply, item.Reason = false, "节点当前不可达，暂不能下发升级"
	case curVer == "":
		item.CanApply, item.Reason = false, "节点未上报有效版本号"
	case release.IsDev(curVer):
		item.CanApply, item.Reason = false, fmt.Sprintf("当前节点为开发版构建（%s），不能自更新", curVer)
	case rt.ExecRunning:
		item.CanApply, item.Reason = false, "节点正在执行远程命令（exec），暂不能更新"
	case !hasUpdate:
		item.CanApply, item.Reason = false, "节点已是最新版本"
	}
	return item
}

func (s *Service) listAllServers(ctx context.Context) ([]*core.Server, error) {
	var all []*core.Server
	var afterID int64
	for {
		page, err := s.repo.List(ctx, afterID, 500)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			return all, nil
		}
		afterID = page[len(page)-1].Metadata.ID
	}
}

func (s *Service) updateCheck(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	channel, err := parseChannelFlag(inv)
	if err != nil {
		return nil, err
	}
	rel, cdn, _, err := s.fetchLatestAgent(ctx, channel)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(inv.Arg(0))
	if name != "" {
		srv, err := s.byName(ctx, name)
		if err != nil {
			return nil, err
		}
		item := s.checkOneServer(ctx, srv, rel, cdn, channel)
		return &item, nil
	}

	rows, err := s.listAllServers(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]AgentUpdateCheckItem, len(rows))
	var wg sync.WaitGroup
	sem := make(chan struct{}, lifecycleParallel)
	for i, srv := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			items[i] = s.checkOneServer(ctx, srv, rel, cdn, channel)
		}()
	}
	wg.Wait()
	return &AgentUpdateCheckList{
		Channel:       channel,
		LatestVersion: rel.Version,
		Source:        rel.Source,
		CDN:           cdn,
		Items:         items,
		Total:         len(items),
	}, nil
}

// ServerUpdateTargetResult 记录单台或批量远程升级中一台节点的最终结果与三态口径。
type ServerUpdateTargetResult struct {
	Server        string `json:"server"`
	FromVersion   string `json:"from_version,omitempty"`
	ToVersion     string `json:"to_version,omitempty"`
	Status        string `json:"status"` // done | failed | skipped | aborted
	Source        string `json:"source,omitempty"`
	DiskState     string `json:"disk_state,omitempty"`
	Retryable     *bool  `json:"retryable,omitempty"`
	BatchContinue *bool  `json:"batch_continue,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

func boolPtr(v bool) *bool { return &v }

func setFailureOutcome(out *ServerUpdateTargetResult, diskState string, retryable, batchContinue bool, reason string) {
	out.Status = "failed"
	out.DiskState = diskState
	out.Retryable = boolPtr(retryable)
	out.BatchContinue = boolPtr(batchContinue)
	out.Reason = reason
}

// BatchUpdateResult 是 server update batch 成功或完成时输出的结果清单。
type BatchUpdateResult struct {
	Version   string                     `json:"version"`
	Channel   string                     `json:"channel"`
	Total     int                        `json:"total"`
	Succeeded int                        `json:"succeeded"`
	Failed    int                        `json:"failed"`
	Aborted   int                        `json:"aborted"`
	Results   []ServerUpdateTargetResult `json:"results"`
}

type preparedUpdateTarget struct {
	srv     *core.Server
	fromVer string
}

func (s *Service) prepareUpdateTarget(ctx context.Context, srv *core.Server, wantVer string, rel *release.Release) (preparedUpdateTarget, error) {
	rt, reachable, callErr := s.inspectNodeInfo(ctx, srv)
	if !reachable {
		if callErr != nil {
			return preparedUpdateTarget{}, callErr
		}
		return preparedUpdateTarget{}, v1.Newf(v1.CodeUnavailable, "服务器 %q 当前不可达，不能发起升级", srv.Spec.Name)
	}
	if rt.InDocker {
		return preparedUpdateTarget{}, v1.Newf(v1.CodeConflict, "服务器 %q 运行在 Docker 容器内，不支持远程自更新", srv.Spec.Name).
			WithNext("在节点宿主机更换 satchel-agent 容器镜像标签后重建容器")
	}
	if rt.ExecRunning {
		return preparedUpdateTarget{}, v1.Newf(v1.CodeConflict, "服务器 %q 正在执行远程命令（exec），暂不能升级", srv.Spec.Name)
	}
	curVer := strings.TrimSpace(rt.AgentVersion)
	if curVer == "" || release.IsDev(curVer) {
		return preparedUpdateTarget{}, v1.Newf(v1.CodeConflict, "服务器 %q 当前版本 %q 不支持远程自更新", srv.Spec.Name, curVer)
	}
	curVer = release.Normalize(curVer)
	if wantVer == "" || !release.Valid(wantVer) {
		return preparedUpdateTarget{}, v1.Newf(v1.CodeBadRequest, "目标版本号 %q 不是合法的语义化版本", wantVer)
	}
	targetNorm := release.Normalize(wantVer)
	if rel.Version != targetNorm {
		return preparedUpdateTarget{}, v1.Newf(v1.CodeConflict, "当前渠道最新可用版本为 %s，与请求目标版本 %s 不一致", rel.Version, targetNorm)
	}
	if release.Compare(rel.Version, curVer) <= 0 {
		return preparedUpdateTarget{}, v1.Newf(v1.CodeConflict, "服务器 %q 当前版本（%s）不低于目标版本（%s）", srv.Spec.Name, curVer, rel.Version)
	}
	if ok, reason := s.isReleaseCompatible(rel); !ok {
		return preparedUpdateTarget{}, v1.New(v1.CodeConflict, reason)
	}
	return preparedUpdateTarget{srv: srv, fromVer: curVer}, nil
}

func (s *Service) runSingleServerUpdate(ctx context.Context, target preparedUpdateTarget, rel *release.Release, channel string, cdnEnabled bool) ServerUpdateTargetResult {
	srv := target.srv
	out := ServerUpdateTargetResult{
		Server:      srv.Spec.Name,
		FromVersion: target.fromVer,
		ToVersion:   rel.Version,
	}
	conns := s.getConns()
	if conns == nil {
		setFailureOutcome(&out, xrpc.DiskStateOld, true, true, "节点连接管理器未就绪")
		return out
	}

	var ack xrpc.AgentUpdateAck
	rpcCtx, cancelRPC := context.WithTimeout(ctx, s.updateTimeout)
	raw, err := conns.Call(rpcCtx, srv.Metadata.ID, xrpc.MethodAgentUpdate, xrpc.AgentUpdateParams{
		Version:                 rel.Version,
		Channel:                 channel,
		UseCDN:                  cdnEnabled,
		ExpectedControlProtocol: rel.ControlProtocol,
	}, s.updateTimeout)
	cancelRPC()

	if err == nil {
		if uerr := json.Unmarshal(raw, &ack); uerr != nil {
			setFailureOutcome(&out, xrpc.DiskStateNewUnconfirmed, false, false, fmt.Sprintf("解析节点 agent.update 回执失败：%v", uerr))
			return out
		}
		out.Source = ack.Source
		if !ack.Accepted {
			ds := cmp.Or(ack.DiskState, xrpc.DiskStateOld)
			setFailureOutcome(&out, ds, ack.Retryable || ds != xrpc.DiskStateNewUnconfirmed, ack.BatchContinue || ds != xrpc.DiskStateNewUnconfirmed, cmp.Or(ack.Message, "节点拒绝或中止更新，旧版本仍在磁盘上"))
			return out
		}
	} else {
		var xe *xrpc.Error
		if errors.As(err, &xe) {
			// 节点明确拒绝（如空间不足、三源均失败、不支持该方法），尚未动磁盘二进制。
			setFailureOutcome(&out, xrpc.DiskStateOld, xe.Code != xrpc.CodeUnknownMethod, true, xe.Message)
			return out
		}
		// 若传输层中途断开且节点可能已进入 exec，继续走重连验证流程判断真实状态。
	}

	deadline := time.Now().Add(s.pollTimeout)
	var lastErr error
	var lastInfo xrpc.AgentInfo
	var gotInfo bool
	for {
		if ctx.Err() != nil {
			setFailureOutcome(&out, xrpc.DiskStateNewUnconfirmed, false, false, "升级确认被取消："+ctx.Err().Error())
			return out
		}
		var info xrpc.AgentInfo
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		callErr := s.call(pctx, srv, xrpc.MethodAgentInfo, nil, &info, 5*time.Second)
		pcancel()
		if callErr == nil {
			lastInfo = info
			gotInfo = true
			s.recordRuntime(srv.Metadata.ID, info)
			norm := release.Normalize(info.AgentVersion)
			if norm == rel.Version {
				out.Status = "done"
				out.ToVersion = norm
				return out
			}
			// 若节点已经重新连通、但尚未完成重启切换，继续等待直到目标版本上线或超时。
		} else {
			lastErr = callErr
		}

		if time.Now().After(deadline) {
			break
		}
		timer := time.NewTimer(s.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}

	if gotInfo && lastInfo.AgentVersion != "" {
		setFailureOutcome(&out, xrpc.DiskStateOldAfterSwap, true, true, fmt.Sprintf("升级确认窗内节点仍报告旧版本 %s（期望 %s）", release.Normalize(lastInfo.AgentVersion), rel.Version))
		return out
	}
	if !ack.Accepted && err != nil {
		// 连 ack 都没收到，且通道在确认窗内也没有恢复。
		setFailureOutcome(&out, xrpc.DiskStateNewUnconfirmed, false, false, fmt.Sprintf("调用 agent.update 断开（%v）且节点未在确认窗内恢复心跳", err))
		return out
	}
	setFailureOutcome(&out, xrpc.DiskStateNewUnconfirmed, false, false, fmt.Sprintf("节点替换后未在 %s 确认窗内恢复并确认新版本：%v", s.pollTimeout, lastErr))
	return out
}

func (s *Service) updateApply(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.startJob == nil {
		return nil, v1.New(v1.CodeInternal, "长任务服务未装配，不能受理 server update apply")
	}
	name := strings.TrimSpace(inv.Arg(0))
	wantVer := strings.TrimSpace(inv.Arg(1))
	if name == "" || wantVer == "" {
		return nil, v1.New(v1.CodeBadRequest, "需要提供服务器名字与目标版本号")
	}
	channel, err := parseChannelFlag(inv)
	if err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, name)
	if err != nil {
		return nil, err
	}
	rel, _, cdnEnabled, err := s.fetchLatestAgent(ctx, channel)
	if err != nil {
		return nil, err
	}
	target, err := s.prepareUpdateTarget(ctx, srv, wantVer, rel)
	if err != nil {
		return nil, err
	}
	ids := []int64{srv.Metadata.ID}
	if !s.tryLockUpdates(ids) {
		return nil, v1.Newf(v1.CodeConflict, "服务器 %q 正在执行升级或卸载，请等待完成", srv.Spec.Name)
	}
	job, jerr := s.startJob(ctx, srv.Metadata.ID, inv, func(jobCtx context.Context) (any, error) {
		res := s.runSingleServerUpdate(jobCtx, target, rel, channel, cdnEnabled)
		if res.Status == "done" {
			return res, nil
		}
		errCode := v1.CodeUnavailable
		if res.DiskState == xrpc.DiskStateOld {
			errCode = v1.CodeConflict
		}
		return nil, v1.Newf(errCode, "服务器 %q 升级至 %s 失败：%s", srv.Spec.Name, rel.Version, res.Reason).
			WithState("server", res.Server).
			WithState("from_version", res.FromVersion).
			WithState("to_version", res.ToVersion).
			WithState("disk_state", res.DiskState).
			WithState("retryable", res.Retryable != nil && *res.Retryable).
			WithState("batch_continue", res.BatchContinue != nil && *res.BatchContinue)
	}, func() {
		s.unlockUpdates(ids)
	})
	if jerr != nil {
		s.unlockUpdates(ids)
		return nil, jerr
	}
	return job, nil
}

func (s *Service) updateBatch(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if !v1.IdentityFrom(ctx).HasDanger(v1.DangerRestart) {
		return nil, v1.New(v1.CodeForbidden, "批量升级节点需要同时具备 restart 危险类权限")
	}
	if s.startJob == nil {
		return nil, v1.New(v1.CodeInternal, "长任务服务未装配，不能受理 server update batch")
	}
	wantVer := strings.TrimSpace(inv.Arg(0))
	if wantVer == "" {
		return nil, v1.New(v1.CodeBadRequest, "需要提供目标版本号")
	}
	channel, err := parseChannelFlag(inv)
	if err != nil {
		return nil, err
	}
	serversCSV := strings.TrimSpace(inv.String("servers", ""))
	allFlag := inv.Bool("all")
	if (serversCSV == "" && !allFlag) || (serversCSV != "" && allFlag) {
		return nil, v1.New(v1.CodeBadRequest, "--servers 与 --all 必须且只能选一个")
	}

	var rawServers []*core.Server
	if allFlag {
		rawServers, err = s.listAllServers(ctx)
		if err != nil {
			return nil, err
		}
		if len(rawServers) == 0 {
			return nil, v1.New(v1.CodeConflict, "当前没有已登记的服务器可批量升级")
		}
	} else {
		seen := map[string]bool{}
		for _, part := range strings.Split(serversCSV, ",") {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			srv, err := s.byName(ctx, name)
			if err != nil {
				return nil, err
			}
			rawServers = append(rawServers, srv)
		}
		if len(rawServers) == 0 {
			return nil, v1.New(v1.CodeBadRequest, "--servers 不能为空")
		}
	}

	expectedConfirm := strconv.Itoa(len(rawServers))
	if inv.Confirm != expectedConfirm {
		return nil, v1.Newf(v1.CodeConfirmRequired, "批量升级 %d 台服务器，请用 --confirm %s 确认目标数量", len(rawServers), expectedConfirm).
			WithState("kind", "count").
			WithState("expected", expectedConfirm).
			WithState("given", inv.Confirm)
	}

	rel, _, cdnEnabled, err := s.fetchLatestAgent(ctx, channel)
	if err != nil {
		return nil, err
	}
	targets := make([]preparedUpdateTarget, 0, len(rawServers))
	ids := make([]int64, 0, len(rawServers))
	for _, srv := range rawServers {
		pt, err := s.prepareUpdateTarget(ctx, srv, wantVer, rel)
		if err != nil {
			return nil, err
		}
		targets = append(targets, pt)
		ids = append(ids, srv.Metadata.ID)
	}
	if !s.tryLockUpdates(ids) {
		return nil, v1.New(v1.CodeConflict, "目标中有服务器正在执行升级或卸载，请稍后再试")
	}

	job, jerr := s.startJob(ctx, 0, inv, func(jobCtx context.Context) (any, error) {
		out := BatchUpdateResult{
			Version: rel.Version,
			Channel: channel,
			Total:   len(targets),
			Results: make([]ServerUpdateTargetResult, 0, len(targets)),
		}
		stoppedIdx := -1
		for i, pt := range targets {
			res := s.runSingleServerUpdate(jobCtx, pt, rel, channel, cdnEnabled)
			out.Results = append(out.Results, res)
			if res.Status == "done" {
				out.Succeeded++
				continue
			}
			out.Failed++
			if res.BatchContinue != nil && !*res.BatchContinue {
				stoppedIdx = i
				for j := i + 1; j < len(targets); j++ {
					out.Aborted++
					out.Results = append(out.Results, ServerUpdateTargetResult{
						Server:      targets[j].srv.Spec.Name,
						FromVersion: targets[j].fromVer,
						ToVersion:   rel.Version,
						Status:      "aborted",
						Reason:      fmt.Sprintf("前面节点 %s 出现高危确认失败（%s），批量升级已中止", pt.srv.Spec.Name, res.DiskState),
					})
				}
				break
			}
		}
		if out.Failed == 0 {
			return out, nil
		}
		msg := fmt.Sprintf("批量升级完成 %d/%d 台，失败 %d 台", out.Succeeded, out.Total, out.Failed)
		if stoppedIdx >= 0 {
			msg = fmt.Sprintf("节点 %s 出现高危确认失败，已中止后续 %d 台（成功 %d 台）", targets[stoppedIdx].srv.Spec.Name, out.Aborted, out.Succeeded)
		}
		return nil, v1.New(v1.CodeConflict, msg).
			WithState("version", out.Version).
			WithState("channel", out.Channel).
			WithState("total", out.Total).
			WithState("succeeded", out.Succeeded).
			WithState("failed", out.Failed).
			WithState("aborted", out.Aborted).
			WithState("results", out.Results)
	}, func() {
		s.unlockUpdates(ids)
	})
	if jerr != nil {
		s.unlockUpdates(ids)
		return nil, jerr
	}
	return job, nil
}

// UninstallResult 是 server uninstall 成功后返回的结构化结果。
type UninstallResult struct {
	Server      string    `json:"server"`
	Uninstalled bool      `json:"uninstalled"`
	ScheduledAt time.Time `json:"scheduled_at"`
	Spawner     string    `json:"spawner,omitempty"`
	Status      string    `json:"status"`
}

func (s *Service) uninstall(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	rt, reachable, callErr := s.inspectNodeInfo(ctx, srv)
	if !reachable {
		if callErr != nil {
			return nil, callErr
		}
		return nil, v1.Newf(v1.CodeUnavailable, "服务器 %q 当前不可达，不能执行远程卸载", srv.Spec.Name)
	}
	if rt.InDocker {
		return nil, v1.Newf(v1.CodeConflict, "服务器 %q 运行在 Docker 容器内，请通过停止并移除容器来卸载", srv.Spec.Name)
	}
	if rt.ExecRunning {
		return nil, v1.Newf(v1.CodeConflict, "服务器 %q 正在执行远程命令（exec），暂不能卸载", srv.Spec.Name)
	}
	ids := []int64{srv.Metadata.ID}
	if !s.tryLockUpdates(ids) {
		return nil, v1.Newf(v1.CodeConflict, "服务器 %q 正在执行升级或卸载，请等待完成", srv.Spec.Name)
	}
	defer s.unlockUpdates(ids)

	var ack xrpc.AgentUninstallAck
	if err := s.call(ctx, srv, xrpc.MethodAgentUninstall, xrpc.AgentUninstallParams{DetachDelayMS: 1000}, &ack, callTimeout); err != nil {
		return nil, err
	}
	if conns := s.getConns(); conns != nil {
		if disc, ok := conns.(serverDisconnector); ok {
			if derr := disc.Disconnect(ctx, srv.Metadata.ID); derr != nil {
				s.logger.Warn("卸载后断开节点连接或标离线失败", "server", srv.Spec.Name, "err", derr)
			}
		} else {
			_, _ = s.repo.MarkOffline(ctx, srv.Metadata.ID, time.Now().UTC().Truncate(time.Microsecond))
		}
	}
	return &UninstallResult{
		Server:      srv.Spec.Name,
		Uninstalled: true,
		ScheduledAt: ack.ScheduledAt,
		Spawner:     ack.Spawner,
		Status:      core.StatusOffline,
	}, nil
}

func (s *Service) switchListenPort(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	_, hasListen := inv.Flags["listen-port"]
	_, hasPull := inv.Flags["pull-port"]
	if !hasListen && !hasPull {
		return nil, v1.New(v1.CodeBadRequest, "至少要提供 --listen-port 或 --pull-port 之一")
	}
	var rpcParams xrpc.SwitchPortsParams
	if hasListen {
		lp := inv.Int("listen-port", 0)
		if lp < 1 || lp > 65535 {
			return nil, v1.Newf(v1.CodeBadRequest, "--listen-port 必须在 1..65535 之间，得到 %d", lp)
		}
		rpcParams.ListenPort = lp
	}
	if hasPull {
		pp := inv.Int("pull-port", 0)
		if pp < 1 || pp > 65535 {
			return nil, v1.Newf(v1.CodeBadRequest, "--pull-port 必须在 1..65535 之间，得到 %d", pp)
		}
		rpcParams.PullPort = pp
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	newListen := srv.Spec.ListenPort
	if hasListen {
		newListen = int64(rpcParams.ListenPort)
	}
	newPull := srv.Spec.PullPort
	if hasPull {
		newPull = int64(rpcParams.PullPort)
	}
	effListen := newListen
	if effListen == 0 {
		effListen = core.DefaultListenPort
	}
	effPull := newPull
	if effPull == 0 {
		effPull = core.DefaultPullPort
	}
	if effListen == effPull {
		return nil, v1.Newf(v1.CodeBadRequest, "管理监听端口（%d）与轮询只读端口（%d）不能相同", effListen, effPull)
	}
	if s.getConns() == nil {
		return nil, v1.Newf(v1.CodeUnavailable, "服务器 %q 现在没有连接", srv.Spec.Name)
	}
	var ack xrpc.SwitchPortsAck
	if err := s.call(ctx, srv, xrpc.MethodAgentSwitchPorts, rpcParams, &ack, callTimeout); err != nil {
		return nil, err
	}
	updated, err := s.repo.UpdatePorts(ctx, srv.Metadata.ID, srv.Metadata.ResourceVersion, newListen, newPull)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// MasterProbeOutput 是 server probe-master-url 的结果。
type MasterProbeOutput struct {
	Server    string `json:"server"`
	Target    string `json:"target"`
	URL       string `json:"url"`
	Reachable bool   `json:"reachable"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

func (s *Service) probeMasterURL(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	target := strings.TrimSpace(inv.String("target", "master"))
	if target != "master" && target != "recovery" {
		return nil, v1.Newf(v1.CodeBadRequest, "--target 只允许 master 或 recovery，得到 %q", target)
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	st, err := s.settings.Load(ctx)
	if err != nil {
		return nil, err
	}
	probeURL := strings.TrimSpace(st.Gates().MasterURL)
	if target == "recovery" {
		raw, _ := st.Values["master_recovery_url"].(string)
		probeURL = strings.TrimSpace(raw)
	}
	u, perr := url.Parse(probeURL)
	if probeURL == "" || perr != nil || u.Scheme != "https" || u.Host == "" {
		return nil, v1.Newf(v1.CodeConflict, "系统设置中的 %s 地址（%q）未配置为合法 https origin", target, probeURL)
	}
	if s.getConns() == nil {
		return nil, v1.Newf(v1.CodeUnavailable, "服务器 %q 现在没有连接", srv.Spec.Name)
	}
	var res xrpc.ProbeMasterResult
	if err := s.call(ctx, srv, xrpc.MethodAgentProbeMaster, xrpc.ProbeMasterParams{URL: probeURL, TimeoutMS: 5000}, &res, callTimeout); err != nil {
		return nil, err
	}
	return &MasterProbeOutput{
		Server:    srv.Spec.Name,
		Target:    target,
		URL:       probeURL,
		Reachable: res.Reachable,
		LatencyMS: res.LatencyMS,
		Error:     res.Error,
	}, nil
}

// MigrationNodeProbe 记录每台节点对候选 master_url 的预检与迁移可达性分类。
type MigrationNodeProbe struct {
	Server    string `json:"server"`
	DBStatus  string `json:"db_status"`
	State     string `json:"state"` // ready | unreachable | offline
	LatencyMS int64  `json:"latency_ms,omitempty"`
	SameHost  bool   `json:"same_host_as_master"`
	Pending   bool   `json:"pending,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ProbeAllNodesMasterURL 并发指示当前所有可达节点对 candidateURL 执行真握手探测（只读，不改任何配置）。
func (s *Service) ProbeAllNodesMasterURL(ctx context.Context, candidateURL string) ([]MigrationNodeProbe, error) {
	rows, err := s.listAllServers(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b *core.Server) int { return cmp.Compare(a.Metadata.ID, b.Metadata.ID) })
	out := make([]MigrationNodeProbe, len(rows))
	var wg sync.WaitGroup
	sem := make(chan struct{}, lifecycleParallel)
	for i, srv := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			item := MigrationNodeProbe{
				Server:   srv.Spec.Name,
				DBStatus: srv.Status.Status,
				SameHost: srv.Status.SameHostAsMaster,
				Pending:  srv.Status.Status == core.StatusPending,
			}
			if s.getConns() == nil {
				item.State = "offline"
				item.Error = "节点未连接"
				out[i] = item
				return
			}
			var res xrpc.ProbeMasterResult
			pctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			err := s.call(pctx, srv, xrpc.MethodAgentProbeMaster, xrpc.ProbeMasterParams{URL: candidateURL, TimeoutMS: 5000}, &res, 5*time.Second)
			cancel()
			if err != nil {
				if e := v1.AsError(err); e != nil && e.Code == v1.CodeUnavailable && srv.Status.Status != core.StatusConnected {
					item.State = "offline"
					item.Error = e.Reason
				} else {
					item.State = "unreachable"
					if e != nil {
						item.Error = e.Reason
					} else {
						item.Error = err.Error()
					}
				}
				out[i] = item
				return
			}
			item.LatencyMS = res.LatencyMS
			if !res.Reachable {
				item.State = "unreachable"
				item.Error = cmp.Or(res.Error, "节点握手候选地址失败")
				out[i] = item
				return
			}
			item.State = "ready"
			out[i] = item
		}()
	}
	wg.Wait()
	return out, nil
}

// PushNodesMasterURL 并发向指定名称的可达节点推送 agent.update_master_url（迁移提交前调用）。
func (s *Service) PushNodesMasterURL(ctx context.Context, targetURL string, serverNames []string) error {
	if len(serverNames) == 0 {
		return nil
	}
	conns := s.getConns()
	if conns == nil {
		return v1.New(v1.CodeUnavailable, "节点连接管理器未就绪")
	}
	srvs := make([]*core.Server, 0, len(serverNames))
	for _, name := range serverNames {
		srv, err := s.byName(ctx, name)
		if err != nil {
			return err
		}
		srvs = append(srvs, srv)
	}
	if blocker, ok := conns.(masterSyncBlocker); ok {
		for _, srv := range srvs {
			blocker.SetMasterSyncBlocked(srv.Metadata.ID, true)
		}
		defer func() {
			for _, srv := range srvs {
				blocker.SetMasterSyncBlocked(srv.Metadata.ID, false)
			}
		}()
	}

	var mu sync.Mutex
	var failed []string
	var wg sync.WaitGroup
	sem := make(chan struct{}, lifecycleParallel)
	for _, srv := range srvs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			var ack xrpc.UpdateMasterAck
			pctx, cancel := context.WithTimeout(ctx, callTimeout)
			err := s.call(pctx, srv, xrpc.MethodAgentUpdateMaster, xrpc.UpdateMasterParams{MasterURL: targetURL}, &ack, callTimeout)
			cancel()
			if err != nil {
				mu.Lock()
				failed = append(failed, fmt.Sprintf("%s (%v)", srv.Spec.Name, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(failed) > 0 {
		slices.Sort(failed)
		return v1.Newf(v1.CodeConflict, "向部分在线节点下发新 master_url 失败：%s", strings.Join(failed, "; ")).
			WithState("failed", failed)
	}
	return nil
}
