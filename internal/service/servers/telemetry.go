package servers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

const (
	defaultSpeedPersistInterval = 15 * time.Second
	defaultSpeedCollectInterval = 3 * time.Second
	speedPullTimeout            = 4 * time.Second
)

type speedSample struct {
	uploadSpeed   int64
	downloadSpeed int64
	updatedAt     time.Time
	lastPersisted time.Time
}

type sysTrafficMem struct {
	rxCycle       int64
	txCycle       int64
	lastSeenRx    int64
	lastSeenTx    int64
	bootTimeUnix  int64
	updatedAt     time.Time
	lastPersisted time.Time
}

type telemetryState struct {
	speedMu              sync.Mutex
	speeds               map[int64]speedSample
	sysTraffic           map[int64]*sysTrafficMem
	speedPersistInterval time.Duration

	pollMu       sync.Mutex
	pollInterval time.Duration
	pollWake     chan struct{}
	pollCancel   context.CancelFunc
	pollWG       sync.WaitGroup
}

type pullSpeedCaller interface {
	PullSpeed(ctx context.Context, serverID int64, timeout time.Duration) (xrpc.SpeedReport, string, bool, error)
}

type configBroadcaster interface {
	BroadcastConfig(ctx context.Context, serverIDs []int64, cfg xrpc.AgentConfig)
}

// SystemInfoResult 是 server system-info 的输出。
type SystemInfoResult struct {
	Server string `json:"server"`
	xrpc.SystemInfo
}

// NICsResult 是 server nics 的输出。
type NICsResult struct {
	Server string         `json:"server"`
	NICs   []xrpc.NICInfo `json:"nics"`
}

// SpeedView 是 server speed 的单台速率视图。
type SpeedView struct {
	Server        string     `json:"server"`
	ServerID      int64      `json:"server_id"`
	Status        string     `json:"status"`
	UploadSpeed   int64      `json:"upload_speed"`
	DownloadSpeed int64      `json:"download_speed"`
	UpdatedAt     *time.Time `json:"updated_at"`
}

// SpeedListResult 是不带服务器名执行 server speed 时的汇总输出。
type SpeedListResult struct {
	Items []SpeedView `json:"items"`
	Total int         `json:"total"`
}

// DashboardServer 是 GET /api/v1/ws/dashboard 推送帧里的单台服务器摘要（绝不含任何凭据字段）。
type DashboardServer struct {
	ID                    int64      `json:"id"`
	Name                  string     `json:"name"`
	ConnectionMode        string     `json:"connection_mode"`
	Status                string     `json:"status"`
	LastHeartbeat         *time.Time `json:"last_heartbeat"`
	IPAddress             *string    `json:"ip_address"`
	IPAddressV6           string     `json:"ip_address_v6"`
	CoreRunning           bool       `json:"core_running"`
	CoreVersion           string     `json:"core_version"`
	CurrentUploadSpeed    int64      `json:"current_upload_speed"`
	CurrentDownloadSpeed  int64      `json:"current_download_speed"`
	SpeedUpdatedAt        *time.Time `json:"speed_updated_at"`
	TrafficSource         string     `json:"traffic_source"`
	TrafficStatsMode      string     `json:"traffic_stats_mode"`
	IncludeInTrafficStats bool       `json:"include_in_traffic_stats"`
	SystemRxCycle         int64      `json:"system_rx_cycle"`
	SystemTxCycle         int64      `json:"system_tx_cycle"`
	SameHostAsMaster      bool       `json:"same_host_as_master"`
}

// DashboardSummary 是 GET /api/v1/ws/dashboard 推送帧里的全盘汇总计数与实时总网速。
type DashboardSummary struct {
	TotalServers       int   `json:"total_servers"`
	ConnectedServers   int   `json:"connected_servers"`
	OfflineServers     int   `json:"offline_servers"`
	PendingServers     int   `json:"pending_servers"`
	TotalUploadSpeed   int64 `json:"total_upload_speed"`
	TotalDownloadSpeed int64 `json:"total_download_speed"`
}

// LookupIDByName 按服务器名称精确查找 server_id（注入 service/jobs 供 job list --server-name 使用）。
func (s *Service) LookupIDByName(ctx context.Context, name string) (int64, bool, error) {
	srv, err := s.repo.ByName(ctx, strings.TrimSpace(name))
	if errors.Is(err, core.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return srv.Metadata.ID, true, nil
}

// IngestSpeed 汇入来自 WS speed 帧、加密 HTTP speed RPC 或 pull 口 speed.get 的实时速率与系统网卡累计快照：
// 内存即时更新，落盘按同机 15 秒节流（首帧立写），系统网卡周期流量严格按重启基线防首见突刺与防倒退。
func (s *Service) IngestSpeed(ctx context.Context, serverID int64, rep xrpc.SpeedReport, now time.Time) {
	if serverID <= 0 {
		return
	}
	up := rep.UploadSpeed
	if up < 0 {
		up = 0
	}
	down := rep.DownloadSpeed
	if down < 0 {
		down = 0
	}
	nowUTC := now.UTC().Truncate(time.Microsecond)
	sampleAt := nowUTC
	if !rep.At.IsZero() {
		sampleAt = rep.At.UTC().Truncate(time.Microsecond)
	}

	lock := s.trafficLock(serverID)
	lock.Lock()
	defer lock.Unlock()

	s.speedMu.Lock()
	if s.speeds == nil {
		s.speeds = map[int64]speedSample{}
	}
	prev := s.speeds[serverID]
	persistInterval := s.speedPersistInterval
	needPersistSpeed := prev.lastPersisted.IsZero() || persistInterval <= 0 || nowUTC.Sub(prev.lastPersisted) >= persistInterval
	next := speedSample{
		uploadSpeed:   up,
		downloadSpeed: down,
		updatedAt:     sampleAt,
		lastPersisted: prev.lastPersisted,
	}
	if needPersistSpeed {
		next.lastPersisted = nowUTC
	}
	s.speeds[serverID] = next
	s.speedMu.Unlock()

	if needPersistSpeed {
		if err := s.repo.UpdateSpeed(ctx, serverID, up, down, sampleAt); err != nil {
			s.logger.Warn("落盘服务器实时网速失败", "server_id", serverID, "err", err)
			s.speedMu.Lock()
			if cur, ok := s.speeds[serverID]; ok && cur.lastPersisted.Equal(nowUTC) {
				cur.lastPersisted = prev.lastPersisted
				s.speeds[serverID] = cur
			}
			s.speedMu.Unlock()
		}
	}

	if rep.SystemBootTimeUnix > 0 {
		s.ingestSystemTrafficLocked(ctx, serverID, rep.SystemRxBytes, rep.SystemTxBytes, rep.SystemBootTimeUnix, sampleAt, nowUTC, persistInterval)
	}
}

func (s *Service) ingestSystemTrafficLocked(ctx context.Context, serverID, rx, tx, boot int64, sampleAt, nowUTC time.Time, persistInterval time.Duration) {
	if rx < 0 {
		rx = 0
	}
	if tx < 0 {
		tx = 0
	}

	s.speedMu.Lock()
	if s.sysTraffic == nil {
		s.sysTraffic = map[int64]*sysTrafficMem{}
	}
	st := s.sysTraffic[serverID]
	s.speedMu.Unlock()

	if st == nil {
		srv, err := s.repo.ByID(ctx, serverID)
		if err != nil {
			return
		}
		st = &sysTrafficMem{
			rxCycle:      srv.Status.SystemRxCycle,
			txCycle:      srv.Status.SystemTxCycle,
			lastSeenRx:   srv.Status.SystemLastSeenRx,
			lastSeenTx:   srv.Status.SystemLastSeenTx,
			bootTimeUnix: srv.Status.SystemBootTimeUnix,
		}
		if srv.Status.SystemTrafficUpdatedAt != nil {
			st.updatedAt = *srv.Status.SystemTrafficUpdatedAt
		}
	}

	forcePersist := false
	switch {
	case st.bootTimeUnix == 0 && st.lastSeenRx == 0 && st.lastSeenTx == 0:
		// 首次建基线：记录当前原始计数，不把历史开机流量一次性算进本周期。
		st.lastSeenRx = rx
		st.lastSeenTx = tx
		st.bootTimeUnix = boot
		st.updatedAt = sampleAt
		forcePersist = true
	case st.bootTimeUnix != 0 && boot != st.bootTimeUnix:
		// 节点主机重启：计数器从零重新增长，累加本次上报值。
		st.rxCycle += rx
		st.txCycle += tx
		st.lastSeenRx = rx
		st.lastSeenTx = tx
		st.bootTimeUnix = boot
		st.updatedAt = sampleAt
		forcePersist = true
	default:
		// 同次开机周期：仅累加非负差值；某方向发生计数器回退时该方向本次计 0 并更新基线。
		if rx >= st.lastSeenRx {
			st.rxCycle += rx - st.lastSeenRx
		}
		if tx >= st.lastSeenTx {
			st.txCycle += tx - st.lastSeenTx
		}
		st.lastSeenRx = rx
		st.lastSeenTx = tx
		st.bootTimeUnix = boot
		st.updatedAt = sampleAt
	}

	needPersist := forcePersist || st.lastPersisted.IsZero() || persistInterval <= 0 || nowUTC.Sub(st.lastPersisted) >= persistInterval
	prevPersisted := st.lastPersisted
	if needPersist {
		st.lastPersisted = nowUTC
	}

	s.speedMu.Lock()
	s.sysTraffic[serverID] = st
	s.speedMu.Unlock()

	if needPersist {
		err := s.repo.UpdateSystemTraffic(ctx, serverID, core.SystemTrafficUpdate{
			RxCycle:      st.rxCycle,
			TxCycle:      st.txCycle,
			LastSeenRx:   st.lastSeenRx,
			LastSeenTx:   st.lastSeenTx,
			BootTimeUnix: st.bootTimeUnix,
			UpdatedAt:    st.updatedAt,
		})
		if err != nil {
			s.logger.Warn("落盘服务器系统网卡流量基线失败", "server_id", serverID, "err", err)
			s.speedMu.Lock()
			st.lastPersisted = prevPersisted
			s.speedMu.Unlock()
		}
	}
}

// ClearSpeed 在节点 WS 断线或离线扫盘后清零主控内存实时网速缓存。
func (s *Service) ClearSpeed(serverID int64, at time.Time) {
	if serverID <= 0 {
		return
	}
	t := at.UTC().Truncate(time.Microsecond)
	s.speedMu.Lock()
	if s.speeds == nil {
		s.speeds = map[int64]speedSample{}
	}
	s.speeds[serverID] = speedSample{uploadSpeed: 0, downloadSpeed: 0, updatedAt: t, lastPersisted: t}
	s.speedMu.Unlock()
}

func (s *Service) effectiveSpeed(srv *core.Server) (int64, int64, *time.Time) {
	s.speedMu.Lock()
	sp, ok := s.speeds[srv.Metadata.ID]
	s.speedMu.Unlock()
	if ok {
		if srv.Status.Status == core.StatusOffline {
			at := sp.updatedAt
			return 0, 0, &at
		}
		at := sp.updatedAt
		return sp.uploadSpeed, sp.downloadSpeed, &at
	}
	if srv.Status.Status == core.StatusOffline {
		return 0, 0, srv.Status.SpeedUpdatedAt
	}
	return srv.Status.CurrentUploadSpeed, srv.Status.CurrentDownloadSpeed, srv.Status.SpeedUpdatedAt
}

func (s *Service) effectiveSystemTraffic(srv *core.Server) (int64, int64, *time.Time) {
	s.speedMu.Lock()
	st := s.sysTraffic[srv.Metadata.ID]
	s.speedMu.Unlock()
	if st != nil {
		at := st.updatedAt
		return st.rxCycle, st.txCycle, &at
	}
	return srv.Status.SystemRxCycle, srv.Status.SystemTxCycle, srv.Status.SystemTrafficUpdatedAt
}

// StartSpeedCollector 启动轻量独立网速轮询循环（不占 scheduler 内置定时任务名额），按 speed_collect_interval 拉取 pull / 回落节点网速。
func (s *Service) StartSpeedCollector(ctx context.Context, interval time.Duration) {
	if interval < time.Second {
		interval = defaultSpeedCollectInterval
	}
	s.pollMu.Lock()
	if s.pollCancel != nil {
		s.pollMu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.pollCancel = cancel
	s.pollInterval = interval
	s.pollWake = make(chan struct{}, 1)
	s.pollWG.Add(1)
	s.pollMu.Unlock()

	go func() {
		defer s.pollWG.Done()
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-s.pollWake:
				s.pollMu.Lock()
				iv := s.pollInterval
				s.pollMu.Unlock()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(iv)
			case <-timer.C:
				s.CollectSpeedOnce(runCtx, time.Now())
				s.pollMu.Lock()
				iv := s.pollInterval
				s.pollMu.Unlock()
				timer.Reset(iv)
			}
		}
	}()
}

// SetSpeedCollectInterval 在 settings set / rollback 变更 speed_collect_interval 时热更新轮询周期。
func (s *Service) SetSpeedCollectInterval(interval time.Duration) {
	if interval < time.Second {
		interval = defaultSpeedCollectInterval
	}
	s.pollMu.Lock()
	changed := s.pollInterval != interval
	s.pollInterval = interval
	wake := s.pollWake
	s.pollMu.Unlock()
	if changed && wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// StopSpeedCollector 停止后台轻量网速轮询循环并等待退出。
func (s *Service) StopSpeedCollector() {
	s.pollMu.Lock()
	cancel := s.pollCancel
	s.pollCancel = nil
	s.pollMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.pollWG.Wait()
}

// CollectSpeedOnce 对当前处于 pull 或 auto(fallback_to_pull) 且无活跃 WS 的服务器执行一次并发有界的 speed.get 拉取。
func (s *Service) CollectSpeedOnce(ctx context.Context, now time.Time) {
	conns := s.getConns()
	puller, ok := conns.(pullSpeedCaller)
	if !ok || conns == nil {
		return
	}
	targets, err := s.repo.PollTargets(ctx)
	if err != nil || len(targets) == 0 {
		return
	}
	activeWS := map[int64]bool{}
	for _, id := range conns.ConnectedIDs() {
		activeWS[id] = true
	}
	sem := make(chan struct{}, collectParallel)
	var wg sync.WaitGroup
	for _, srv := range targets {
		if activeWS[srv.Metadata.ID] {
			continue
		}
		if srv.Status.Status == core.StatusOffline && !srv.Status.FallbackToPull && srv.Spec.ConnectionMode != core.ModePull {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(id int64) {
			defer wg.Done()
			defer func() { <-sem }()
			rep, _, _, err := puller.PullSpeed(ctx, id, speedPullTimeout)
			if err != nil {
				return
			}
			s.IngestSpeed(ctx, id, rep, now)
		}(srv.Metadata.ID)
	}
	wg.Wait()
}

// BroadcastAgentConfig 向活跃 WS 与可达管理口节点推送最新运行配置（agent_log_enabled 与采集/心跳间隔）。
func (s *Service) BroadcastAgentConfig(ctx context.Context, cfg xrpc.AgentConfig) {
	conns := s.getConns()
	broadcaster, ok := conns.(configBroadcaster)
	if !ok || conns == nil {
		return
	}
	rows, err := s.repo.ListAll(ctx)
	if err != nil {
		s.logger.Warn("广播节点运行配置前列出服务器失败", "err", err)
		return
	}
	ids := make([]int64, 0, len(rows))
	for _, srv := range rows {
		if srv.Spec.ConnectionMode != core.ModeWebsocket && srv.Status.Status == core.StatusConnected {
			ids = append(ids, srv.Metadata.ID)
		}
	}
	broadcaster.BroadcastConfig(ctx, ids, cfg)
}

// DashboardSnapshot 构建管理员仪表盘实时快照（绝不含三把服务器密钥）。
func (s *Service) DashboardSnapshot(ctx context.Context) ([]DashboardServer, DashboardSummary, error) {
	rows, err := s.repo.ListAll(ctx)
	if err != nil {
		return nil, DashboardSummary{}, err
	}
	items := make([]DashboardServer, 0, len(rows))
	summary := DashboardSummary{TotalServers: len(rows)}
	for _, srv := range rows {
		up, down, speedAt := s.effectiveSpeed(srv)
		rxCycle, txCycle, _ := s.effectiveSystemTraffic(srv)
		switch srv.Status.Status {
		case core.StatusConnected:
			summary.ConnectedServers++
			summary.TotalUploadSpeed += up
			summary.TotalDownloadSpeed += down
		case core.StatusOffline:
			summary.OfflineServers++
		default:
			summary.PendingServers++
		}
		items = append(items, DashboardServer{
			ID:                    srv.Metadata.ID,
			Name:                  srv.Spec.Name,
			ConnectionMode:        srv.Spec.ConnectionMode,
			Status:                srv.Status.Status,
			LastHeartbeat:         srv.Status.LastHeartbeat,
			IPAddress:             srv.Status.IPAddress,
			IPAddressV6:           srv.Status.IPAddressV6,
			CoreRunning:           srv.Status.CoreRunning,
			CoreVersion:           srv.Status.CoreVersion,
			CurrentUploadSpeed:    up,
			CurrentDownloadSpeed:  down,
			SpeedUpdatedAt:        speedAt,
			TrafficSource:         srv.Spec.TrafficSource,
			TrafficStatsMode:      srv.Spec.TrafficStatsMode,
			IncludeInTrafficStats: srv.Spec.IncludeInTrafficStats,
			SystemRxCycle:         rxCycle,
			SystemTxCycle:         txCycle,
			SameHostAsMaster:      srv.Status.SameHostAsMaster,
		})
	}
	return items, summary, nil
}

// systemInfo 是 server system-info <name>：经统一通道采集节点系统与硬件只读信息。
func (s *Service) systemInfo(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	var info xrpc.SystemInfo
	if err := s.call(ctx, srv, xrpc.MethodSystemInfo, nil, &info, callTimeout); err != nil {
		return nil, err
	}
	return &SystemInfoResult{Server: srv.Spec.Name, SystemInfo: info}, nil
}

// nics 是 server nics <name>：经统一通道列出节点 UP 非回环网卡与可绑定地址。
func (s *Service) nics(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	var res xrpc.SystemNICs
	if err := s.call(ctx, srv, xrpc.MethodSystemNICs, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	if res.NICs == nil {
		res.NICs = []xrpc.NICInfo{}
	}
	return &NICsResult{Server: srv.Spec.Name, NICs: res.NICs}, nil
}

// speed 是 server speed [<name>]：直接读主控内存/库实时网速，不触发远端 RPC。
func (s *Service) speed(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if name := strings.TrimSpace(inv.Arg(0)); name != "" {
		srv, err := s.byName(ctx, name)
		if err != nil {
			return nil, err
		}
		up, down, at := s.effectiveSpeed(srv)
		return &SpeedView{
			Server:        srv.Spec.Name,
			ServerID:      srv.Metadata.ID,
			Status:        srv.Status.Status,
			UploadSpeed:   up,
			DownloadSpeed: down,
			UpdatedAt:     at,
		}, nil
	}
	rows, err := s.repo.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]SpeedView, 0, len(rows))
	for _, srv := range rows {
		up, down, at := s.effectiveSpeed(srv)
		items = append(items, SpeedView{
			Server:        srv.Spec.Name,
			ServerID:      srv.Metadata.ID,
			Status:        srv.Status.Status,
			UploadSpeed:   up,
			DownloadSpeed: down,
			UpdatedAt:     at,
		})
	}
	return &SpeedListResult{Items: items, Total: len(items)}, nil
}

// trafficStatsSet 是 server traffic-stats set <name>：按乐观并发更新流量统计口径。
func (s *Service) trafficStatsSet(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	rv := inv.Int("resource-version", 0)
	if rv <= 0 {
		return nil, v1.New(v1.CodeBadRequest, "缺少 --resource-version").WithNext("先用 server get " + srv.Spec.Name + " 看 metadata.resourceVersion")
	}
	var patch core.TrafficStatsPatch
	if mode := strings.TrimSpace(inv.String("mode", "")); mode != "" {
		if !core.ValidTrafficStatsMode(mode) {
			return nil, v1.Newf(v1.CodeBadRequest, "--mode 不认识 %q", mode).WithNext("写 both、upload、download、max 之一")
		}
		patch.Mode = &mode
	}
	if src := strings.TrimSpace(inv.String("source", "")); src != "" {
		if !core.ValidTrafficSource(src) {
			return nil, v1.Newf(v1.CodeBadRequest, "--source 不认识 %q", src).WithNext("写 core 或 system")
		}
		patch.Source = &src
	}
	if inc := strings.TrimSpace(inv.String("include", "")); inc != "" {
		switch inc {
		case "true":
			b := true
			patch.Include = &b
		case "false":
			b := false
			patch.Include = &b
		default:
			return nil, v1.Newf(v1.CodeBadRequest, "--include 只能是 true 或 false，收到 %q", inc)
		}
	}
	if patch.Mode == nil && patch.Source == nil && patch.Include == nil {
		return nil, v1.New(v1.CodeBadRequest, "至少需要提供 --mode、--source 或 --include 之一")
	}
	return s.repo.UpdateTrafficStats(ctx, srv.Metadata.ID, int64(rv), patch)
}

var validLogServices = map[string]bool{"agent": true, "core": true, "nginx": true}
var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

// logsList 是 server logs list <name>：经统一通道只读分页查看节点日志行。
func (s *Service) logsList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
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
	service := strings.TrimSpace(inv.String("service", "agent"))
	if service == "" {
		service = "agent"
	}
	if !validLogServices[service] {
		return nil, v1.Newf(v1.CodeBadRequest, "--service 不认识 %q", service).WithNext("写 agent、core、nginx 之一")
	}
	level := strings.TrimSpace(inv.String("level", ""))
	if level != "" && !validLogLevels[level] {
		return nil, v1.Newf(v1.CodeBadRequest, "--level 不认识 %q", level).WithNext("写 debug、info、warn、error 之一")
	}
	grep := inv.String("grep", "")
	file := strings.TrimSpace(inv.String("file", ""))
	if file != "" {
		if strings.ContainsAny(file, `/\`) || strings.Contains(file, "..") || file == "." {
			return nil, v1.Newf(v1.CodeNotFound, "没有日志文件 %q", file).WithNext("先用 server logs files " + srv.Spec.Name + " 看有哪些文件")
		}
		if service == "nginx" {
			return nil, v1.New(v1.CodeBadRequest, "--file 仅支持 agent 或 core 日志，不支持 nginx")
		}
	}
	var res xrpc.AgentLogsResult
	params := xrpc.AgentLogsParams{
		Service: service,
		Level:   level,
		Grep:    grep,
		File:    file,
		Limit:   page.Limit,
		Offset:  offset,
	}
	if err := s.call(ctx, srv, xrpc.MethodAgentLogs, params, &res, callTimeout); err != nil {
		return nil, err
	}
	out := &command.PageResult{Items: make([]any, 0, len(res.Items)), Total: res.Total}
	for _, item := range res.Items {
		out.Items = append(out.Items, item)
	}
	if offset+len(res.Items) < res.Total && len(res.Items) > 0 {
		out.NextCursor = command.EncodeOffsetCursor(offset + len(res.Items))
	}
	return out, nil
}

// logsFiles 是 server logs files <name>：经统一通道只读分页列出节点日志目录内的白名单文件。
func (s *Service) logsFiles(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
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
	var res xrpc.AgentLogFilesResult
	if err := s.call(ctx, srv, xrpc.MethodAgentLogFiles, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	out := &command.PageResult{Items: make([]any, 0, len(res.Files)), Total: len(res.Files)}
	for i := offset; i < len(res.Files) && i < offset+page.Limit; i++ {
		out.Items = append(out.Items, res.Files[i])
	}
	if offset+page.Limit < len(res.Files) {
		out.NextCursor = command.EncodeOffsetCursor(offset + page.Limit)
	}
	return out, nil
}
