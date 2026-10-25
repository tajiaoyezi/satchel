package servers

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// Conns 是调节点要用的连接管理（agentconn.Manager 满足它）。
type Conns interface {
	Call(ctx context.Context, serverID int64, method string, params any, timeout time.Duration) (json.RawMessage, error)
	ConnectedIDs() []int64
}

// SetConns 在建好连接管理之后把它设回来（连接管理又要用本服务给的 Directory，只能后设）。
func (s *Service) SetConns(c Conns) { s.conns = c }

const (
	// callTimeout 是命令与拉取每次调节点的超时（m2-03 design 第 5 条）。
	callTimeout = 10 * time.Second
	// collectParallel 是一次拉取最多同时向几台节点要快照。
	collectParallel = 8
	// bitsPerMbps：命令里的 --mbps 是 Mbps，通道上是比特每秒。
	bitsPerMbps = 1_000_000
)

// call 向一台服务器发请求，把节点与连接的错误换成四字段错误（m2-03 design 第 5 条，node-limiter「这几条命令的前提与错误」）。
func (s *Service) call(ctx context.Context, srv *core.Server, method string, params, result any) error {
	raw, err := s.conns.Call(ctx, srv.Metadata.ID, method, params, callTimeout)
	if err != nil {
		return nodeError(srv.Spec.Name, method, err)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return v1.Wrap(v1.CodeInternal, fmt.Sprintf("节点对 %s 的回复解不开", method), err)
	}
	return nil
}

func nodeError(name, method string, err error) error {
	var xe *xrpc.Error
	switch {
	case errors.Is(err, agentconn.ErrOffline):
		return v1.Newf(v1.CodeUnavailable, "服务器 %q 现在没有连接", name).
			WithNext(fmt.Sprintf("用 server get %s 看在线状态，等它连上再试", name))
	case errors.Is(err, xrpc.ErrTimeout):
		return v1.Newf(v1.CodeUnavailable, "服务器 %q 在 %s 内没有回复 %s", name, callTimeout, method).
			WithNext(fmt.Sprintf("用 server get %s 看在线状态，稍后再试", name))
	case errors.As(err, &xe) && xe.Code == xrpc.CodeUnknownMethod:
		return v1.Newf(v1.CodeConflict, "服务器 %q 上的 satchel-agent 不认识 %s：它比主控旧", name, method).
			WithNext("升级这台节点的 satchel-agent")
	case errors.As(err, &xe) && xe.Code == xrpc.CodeBadParams:
		return v1.Newf(v1.CodeBadRequest, "服务器 %q 拒绝了 %s：%s", name, method, xe.Message)
	case errors.As(err, &xe):
		return v1.Newf(v1.CodeInternal, "服务器 %q 执行 %s 失败：%s", name, method, xe.Message)
	default:
		// 发不出去、连接中途断开（xrpc.ErrClosed）等。
		return v1.Newf(v1.CodeUnavailable, "向服务器 %q 发 %s 失败：%v", name, method, err).
			WithNext(fmt.Sprintf("用 server get %s 看在线状态，稍后再试", name))
	}
}

// required 取一个必须给的字符串 flag。
func required(inv *command.Invocation, flag string) (string, error) {
	v := strings.TrimSpace(inv.String(flag, ""))
	if v == "" {
		return "", v1.Newf(v1.CodeBadRequest, "缺少 --%s", flag)
	}
	return v, nil
}

// LimitSource 是一份已设的限速来源。
type LimitSource struct {
	Source   string     `json:"source"`
	BPS      int64      `json:"bps"`
	Mbps     float64    `json:"mbps"`
	Deadline *time.Time `json:"deadline,omitempty"`
}

// LimitResult 是 server limit set / clear 的结果：这份凭据改完之后已设的来源（没设的不出现）与生效值。
type LimitResult struct {
	Server        string        `json:"server"`
	Inbound       string        `json:"inbound"`
	Credential    string        `json:"credential"`
	Sources       []LimitSource `json:"sources"`
	Limited       bool          `json:"limited"`
	EffectiveBPS  int64         `json:"effective_bps"`
	EffectiveMbps float64       `json:"effective_mbps"`
}

func limitResult(server string, st xrpc.LimiterState) *LimitResult {
	r := &LimitResult{Server: server, Inbound: st.Inbound, Credential: st.Credential, Sources: []LimitSource{},
		Limited: st.Limited, EffectiveBPS: st.EffectiveBPS, EffectiveMbps: float64(st.EffectiveBPS) / bitsPerMbps}
	for _, src := range st.Sources {
		r.Sources = append(r.Sources, LimitSource{Source: src.Source, BPS: src.BPS, Mbps: float64(src.BPS) / bitsPerMbps, Deadline: src.Deadline})
	}
	return r
}

// limitTarget 取限速命令共同的部分：服务器、入站与凭据。
func (s *Service) limitTarget(ctx context.Context, inv *command.Invocation) (*core.Server, string, string, error) {
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, "", "", err
	}
	inbound, err := required(inv, "inbound")
	if err != nil {
		return nil, "", "", err
	}
	credential, err := required(inv, "credential")
	if err != nil {
		return nil, "", "", err
	}
	return srv, inbound, credential, nil
}

// limitSet 设「主控临时」这一份来源（node-limiter「主控临时限速的设与撤」）。
func (s *Service) limitSet(ctx context.Context, inv *command.Invocation) (any, error) {
	srv, inbound, credential, err := s.limitTarget(ctx, inv)
	if err != nil {
		return nil, err
	}
	mbps := inv.Int("mbps", -1)
	if mbps < 0 {
		return nil, v1.New(v1.CodeBadRequest, "--mbps 必须给、是不小于 0 的整数（单位 Mbps，0 表示这一份显式不限速）")
	}
	if int64(mbps) > math.MaxInt64/bitsPerMbps {
		return nil, v1.Newf(v1.CodeBadRequest, "--mbps %d 太大", mbps)
	}
	expiresIn := inv.Duration("expires-in", 0)
	if expiresIn < 0 {
		return nil, v1.New(v1.CodeBadRequest, "--expires-in 不能是负数（不给或为 0 表示一直有效）")
	}
	params := xrpc.LimiterSetParams{Inbound: inbound, Credential: credential, Source: xrpc.SourceMaster, BPS: int64(mbps) * bitsPerMbps}
	if expiresIn > 0 {
		d := time.Now().Add(expiresIn).UTC()
		params.Deadline = &d
	}
	var st xrpc.LimiterState
	if err := s.call(ctx, srv, xrpc.MethodLimiterSet, params, &st); err != nil {
		return nil, err
	}
	return limitResult(srv.Spec.Name, st), nil
}

// limitClear 撤掉「主控临时」这一份来源。
func (s *Service) limitClear(ctx context.Context, inv *command.Invocation) (any, error) {
	srv, inbound, credential, err := s.limitTarget(ctx, inv)
	if err != nil {
		return nil, err
	}
	var st xrpc.LimiterState
	params := xrpc.LimiterUnsetParams{Inbound: inbound, Credential: credential, Source: xrpc.SourceMaster}
	if err := s.call(ctx, srv, xrpc.MethodLimiterUnset, params, &st); err != nil {
		return nil, err
	}
	return limitResult(srv.Spec.Name, st), nil
}

// KickResult 是 server kick 的结果。
type KickResult struct {
	Server     string `json:"server"`
	Credential string `json:"credential"`
	Inbound    string `json:"inbound,omitempty"`
	Kicked     int    `json:"kicked"`
}

// kick 踢线：不给入站时踢这个凭据名在所有入站上的连接（node-limiter「踢线与连接清单」）。
func (s *Service) kick(ctx context.Context, inv *command.Invocation) (any, error) {
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	credential, err := required(inv, "credential")
	if err != nil {
		return nil, err
	}
	params := xrpc.ConnectionsKickParams{Credential: credential, Inbound: strings.TrimSpace(inv.String("inbound", ""))}
	var res xrpc.ConnectionsKicked
	if err := s.call(ctx, srv, xrpc.MethodConnectionsKick, params, &res); err != nil {
		return nil, err
	}
	return &KickResult{Server: srv.Spec.Name, Credential: credential, Inbound: params.Inbound, Kicked: res.Kicked}, nil
}

// connections 列这份凭据在这个入站上的活动连接，按连接 id 分页。
func (s *Service) connections(ctx context.Context, inv *command.Invocation) (any, error) {
	srv, inbound, credential, err := s.limitTarget(ctx, inv)
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
	afterID, err := command.DecodeIDCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	var list xrpc.ConnectionsList
	if err := s.call(ctx, srv, xrpc.MethodConnectionsList, xrpc.ConnectionsListParams{Inbound: inbound, Credential: credential}, &list); err != nil {
		return nil, err
	}
	conns := list.Connections
	slices.SortFunc(conns, func(a, b xrpc.Connection) int { return cmp.Compare(a.ID, b.ID) })
	res := &command.PageResult{Items: []any{}, Total: len(conns)}
	var lastID uint64
	for _, c := range conns {
		if int64(c.ID) <= afterID {
			continue
		}
		if len(res.Items) == page.Limit {
			res.NextCursor = command.EncodeIDCursor(int64(lastID))
			break
		}
		res.Items, lastID = append(res.Items, c), c.ID
	}
	return res, nil
}

// TrafficView 是 server traffic 的结果：主控内存里的累计与库里的内核状态（node-traffic「看累计」）。
type TrafficView struct {
	Server        string         `json:"server"`
	Since         *time.Time     `json:"since"`
	CollectedAt   *time.Time     `json:"collected_at"`
	CoreRunning   bool           `json:"core_running"`
	CoreVersion   string         `json:"core_version"`
	CoreBootTime  *time.Time     `json:"core_boot_time"`
	CoreBootCount int64          `json:"core_boot_count"`
	Traffic       []TrafficTotal `json:"traffic"`
}

// traffic 读内存里的累计，不经通道、不要求服务器在线。
func (s *Service) traffic(ctx context.Context, inv *command.Invocation) (any, error) {
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	st := srv.Status
	v := &TrafficView{Server: srv.Spec.Name, CoreRunning: st.CoreRunning, CoreVersion: st.CoreVersion, CoreBootTime: st.CoreBootTime,
		CoreBootCount: st.CoreBootCount, Traffic: []TrafficTotal{}}
	s.mu.Lock()
	if a := s.accs[srv.Metadata.ID]; a != nil {
		since, at := a.since, a.at
		v.Since, v.CollectedAt, v.Traffic = &since, &at, a.totals()
	}
	s.mu.Unlock()
	return v, nil
}

// trafficState 是拉取与累计在内存里的状态：每台服务器的累计与上次拉取的时间。内置任务的一次运行等全部拉取结束才返回，
// 下一次运行不会和它重叠，所以同一台服务器不会有两个拉取同时在跑（两份快照乱序收进累计会让累计倒退）。
type trafficState struct {
	mu       sync.Mutex
	accs     map[int64]*accumulator
	lastPull map[int64]time.Time
}

// CollectTraffic 是内置任务 server_traffic_collect（node-traffic「定时拉取计数快照」）：每次运行对已连接、从没拉过或距上次
// 拉取满 traffic_collect_interval 的服务器要一次快照，收进累计、写内核状态；一台失败不影响别的，到了间隔再试（失败也记下
// 拉取时间，坏掉的节点不会每 10 秒报一次）。now 由调用方给，测试里拨钟。
func (s *Service) CollectTraffic(now func() time.Time) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		st, err := s.settings.Load(ctx)
		if err != nil {
			return "", err
		}
		interval := st.TrafficCollectInterval()
		at := now()
		var due []int64
		s.mu.Lock()
		for _, id := range s.conns.ConnectedIDs() {
			if last, ok := s.lastPull[id]; !ok || at.Sub(last) >= interval {
				due = append(due, id)
				s.lastPull[id] = at
			}
		}
		s.mu.Unlock()
		var (
			wg           sync.WaitGroup
			mu           sync.Mutex
			pulled, fail int
			sem          = make(chan struct{}, collectParallel)
		)
		for _, id := range due {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; wg.Done() }()
				err := s.collectOne(ctx, id, now)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					fail++
					s.logger.Warn("拉取流量快照失败", "server_id", id, "err", err)
					return
				}
				pulled++
			}()
		}
		wg.Wait()
		return fmt.Sprintf("拉了 %d 台、失败 %d 台", pulled, fail), nil
	}
}

// collectOne 向一台服务器要快照、收进累计并写内核状态。
func (s *Service) collectOne(ctx context.Context, id int64, now func() time.Time) error {
	raw, err := s.conns.Call(ctx, id, xrpc.MethodTrafficSnapshot, nil, callTimeout)
	if err != nil {
		return err
	}
	var snap xrpc.TrafficSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("快照解不开：%w", err)
	}
	// 先把快照收进累计，再读起点：起点读失败时这份快照也不能丢，否则下一份会被当成第一份、漏掉中间的重启。
	s.mu.Lock()
	a := s.accs[id]
	if a == nil {
		a = &accumulator{}
		s.accs[id] = a
	}
	a.restarts += int64(a.add(snap, now()))
	based := a.based
	s.mu.Unlock()
	if !based {
		// 以库里现有的 core_boot_count 为起点（主控重启以前记的次数接着往上加）；读失败就下一次拉取再读。
		srv, err := s.repo.ByID(ctx, id)
		if err != nil {
			return err
		}
		s.mu.Lock()
		a.bootBase, a.based = srv.Status.CoreBootCount, true
		s.mu.Unlock()
	}
	s.mu.Lock()
	bootCount := a.bootBase + a.restarts
	s.mu.Unlock()
	version := snap.KernelVersion
	if !snap.KernelRunning {
		version = ""
	}
	// 写的是绝对值：这次写库失败（不论写没写进去），下一次拉取写的是同一个或更大的值，重启既不漏记也不重复。
	return s.repo.MarkCore(ctx, id, snap.KernelRunning, version, snap.KernelStartedAt, bootCount)
}
