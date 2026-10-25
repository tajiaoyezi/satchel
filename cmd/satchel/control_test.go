package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	svcservers "github.com/satchel/satchel/internal/service/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// fakeNode 是测试里节点一侧的连接管控：按 node-limiter 的规则记三份来源（入站只有 vless-in），回固定的 5 条连接，
// 回测试设好的快照，并记下每次收到的参数。
type fakeNode struct {
	mu      sync.Mutex
	sources map[string]xrpc.LimitSource // 凭据 → 来源 → 值；入站只有 vless-in
	last    map[string]json.RawMessage  // 方法 → 最近一次的参数
	snap    xrpc.TrafficSnapshot
	pulls   int
}

func newFakeNode() *fakeNode {
	return &fakeNode{sources: map[string]xrpc.LimitSource{}, last: map[string]json.RawMessage{}}
}

// params 把某个方法最近一次的参数解到 v 里（v 应当是新的零值：省略的字段不会被清掉）。
func (n *fakeNode) params(method string, v any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	_ = json.Unmarshal(n.last[method], v)
}

func (n *fakeNode) setSnap(s xrpc.TrafficSnapshot) {
	n.mu.Lock()
	n.snap = s
	n.mu.Unlock()
}

func (n *fakeNode) pullCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pulls
}

// state 按「已设且大于 0 的来源里取最小」算出这份凭据的回复（调用方持锁）。
func (n *fakeNode) state(inbound, credential string) xrpc.LimiterState {
	st := xrpc.LimiterState{Inbound: inbound, Credential: credential, Sources: []xrpc.LimitSource{}}
	for _, src := range xrpc.Sources {
		v, ok := n.sources[credential+"/"+src]
		if !ok {
			continue
		}
		st.Sources = append(st.Sources, v)
		if v.BPS > 0 && (!st.Limited || v.BPS < st.EffectiveBPS) {
			st.Limited, st.EffectiveBPS = true, v.BPS
		}
	}
	return st
}

func (n *fakeNode) methods() xrpc.Methods {
	handle := func(method string, fn func(raw json.RawMessage) (any, error)) xrpc.Handler {
		return func(_ context.Context, raw json.RawMessage) (any, error) {
			n.mu.Lock()
			defer n.mu.Unlock()
			n.last[method] = raw
			return fn(raw)
		}
	}
	noInbound := func(inbound string) error {
		if inbound != "vless-in" {
			return &xrpc.Error{Code: xrpc.CodeBadParams, Message: "节点上没有入站 " + inbound}
		}
		return nil
	}
	return xrpc.Methods{
		xrpc.MethodLimiterSet: handle(xrpc.MethodLimiterSet, func(raw json.RawMessage) (any, error) {
			var p xrpc.LimiterSetParams
			_ = json.Unmarshal(raw, &p)
			if err := noInbound(p.Inbound); err != nil {
				return nil, err
			}
			n.sources[p.Credential+"/"+p.Source] = xrpc.LimitSource{Source: p.Source, BPS: p.BPS, Deadline: p.Deadline}
			return n.state(p.Inbound, p.Credential), nil
		}),
		xrpc.MethodLimiterUnset: handle(xrpc.MethodLimiterUnset, func(raw json.RawMessage) (any, error) {
			var p xrpc.LimiterUnsetParams
			_ = json.Unmarshal(raw, &p)
			if err := noInbound(p.Inbound); err != nil {
				return nil, err
			}
			delete(n.sources, p.Credential+"/"+p.Source)
			return n.state(p.Inbound, p.Credential), nil
		}),
		xrpc.MethodConnectionsList: handle(xrpc.MethodConnectionsList, func(json.RawMessage) (any, error) {
			start := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
			var list xrpc.ConnectionsList
			for i, network := range []string{"udp", "tcp", "tcp", "udp", "tcp"} { // 故意乱序，主控按 id 排
				id := uint64(5 - i)
				list.Connections = append(list.Connections, xrpc.Connection{ID: id, Network: network, Peer: "198.51.100.7:4000" + string(rune('0'+id)), Outbound: "direct", Start: start})
			}
			return list, nil
		}),
		xrpc.MethodConnectionsKick: handle(xrpc.MethodConnectionsKick, func(raw json.RawMessage) (any, error) {
			var p xrpc.ConnectionsKickParams
			_ = json.Unmarshal(raw, &p)
			if p.Credential == "boom" {
				return nil, errors.New("tracker 出错了") // 节点执行失败：Serve 回 failed
			}
			if p.Inbound == "" {
				return xrpc.ConnectionsKicked{Kicked: 4}, nil
			}
			return xrpc.ConnectionsKicked{Kicked: 2}, nil
		}),
		xrpc.MethodTrafficSnapshot: handle(xrpc.MethodTrafficSnapshot, func(json.RawMessage) (any, error) {
			n.pulls++
			return n.snap, nil
		}),
	}
}

// node-limiter 的命令、错误与 node-traffic 的拉取和累计：主控是真的，节点是 fakeNode（双库）。
func TestServerControlCommands(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		base, pub := h.tcpURL, masterPub(t, h)
		admin := setupAdmin(t, base)
		ro := issueToken(t, admin, base, "readonly")
		ops := issueToken(t, admin, base, "ops")
		node := newFakeNode()
		a := mustDial(t, h)
		if !a.authenticate(pub, h.addServer("hk-1"), xrpc.ControlProtocol).OK {
			t.Fatal("hk-1 应当认证成功")
		}
		a.serveMethods(node.methods())
		h.addServer("hk-2") // 不在线
		old := mustDial(t, h)
		if !old.authenticate(pub, h.addServer("hk-3"), xrpc.ControlProtocol).OK {
			t.Fatal("hk-3 应当认证成功")
		}
		old.serveCalls() // 旧版 agent：只认 agent.info

		limit := func(args ...string) svcservers.LimitResult {
			t.Helper()
			stdout, stderr, code := h.cli(append([]string{"server", "limit"}, append(args, "--json")...)...)
			if code != 0 {
				t.Fatalf("server limit %v：%s", args, stderr)
			}
			var r svcservers.LimitResult
			if err := json.Unmarshal([]byte(stdout), &r); err != nil {
				t.Fatalf("结果解不开：%v %s", err, stdout)
			}
			return r
		}
		target := []string{"hk-1", "--inbound", "vless-in", "--credential", "a"}

		// 设临时限速：节点收到 source=master、5,000,000 比特每秒、没有截止时间。
		r := limit(append([]string{"set"}, append(target, "--mbps", "5")...)...)
		var set xrpc.LimiterSetParams
		node.params(xrpc.MethodLimiterSet, &set)
		if set.Source != xrpc.SourceMaster || set.BPS != 5_000_000 || set.Deadline != nil || set.Inbound != "vless-in" || set.Credential != "a" {
			t.Fatalf("节点收到的参数不对：%+v", set)
		}
		if r.Server != "hk-1" || !r.Limited || r.EffectiveMbps != 5 || len(r.Sources) != 1 || r.Sources[0].Source != "master" || r.Sources[0].Mbps != 5 {
			t.Fatalf("结果里应当有主控临时 5 Mbps 与生效值：%+v", r)
		}
		// 带截止时间。
		limit(append([]string{"set"}, append(target, "--mbps", "5", "--expires-in", "2m")...)...)
		set = xrpc.LimiterSetParams{}
		node.params(xrpc.MethodLimiterSet, &set)
		if set.Deadline == nil || set.Deadline.Sub(time.Now()) < 110*time.Second || set.Deadline.Sub(time.Now()) > 130*time.Second {
			t.Fatalf("截止时间应当约在两分钟之后：%v", set.Deadline)
		}
		// 设成 0 与撤掉回的状态不同。
		if r := limit(append([]string{"set"}, append(target, "--mbps", "0")...)...); r.Limited || len(r.Sources) != 1 || r.Sources[0].BPS != 0 {
			t.Fatalf("设成 0 应当是「设了、值为 0」、不限速：%+v", r)
		}
		if r := limit(append([]string{"clear"}, target...)...); r.Limited || len(r.Sources) != 0 {
			t.Fatalf("撤掉之后主控临时应当是没设：%+v", r)
		}
		var unset xrpc.LimiterUnsetParams
		node.params(xrpc.MethodLimiterUnset, &unset)
		if unset != (xrpc.LimiterUnsetParams{Inbound: "vless-in", Credential: "a", Source: xrpc.SourceMaster}) {
			t.Fatalf("撤的应当是 vless-in 上 a 的主控临时：%+v", unset)
		}
		// 参数检查与权限。
		if e := h.cliErr(append([]string{"server", "limit", "set"}, target...)...); e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "--mbps") {
			t.Fatalf("没给 --mbps 应当 bad_request：%+v", e)
		}
		if e := h.cliErr(append([]string{"server", "limit", "set"}, append(target, "--mbps=-1")...)...); e.Code != v1.CodeBadRequest {
			t.Fatalf("负数应当 bad_request：%+v", e)
		}
		if e := h.cliErr(append([]string{"server", "limit", "set"}, append(target, "--mbps", "9300000000000")...)...); e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "太大") {
			t.Fatalf("乘上 1,000,000 会溢出的值应当 bad_request：%+v", e)
		}
		if e := h.cliErr("server", "limit", "set", "hk-1", "--credential", "a", "--mbps", "5"); e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "--inbound") {
			t.Fatalf("没给 --inbound 应当 bad_request：%+v", e)
		}
		body := `{"inbound":"vless-in","credential":"a","mbps":5}`
		if r := send(t, nil, "POST", base+"/api/v1/server/limit/set/hk-1", map[string]string{"Authorization": "Bearer " + ro}, body); r.status != 403 || r.code() != "forbidden" {
			t.Fatalf("只读令牌做不了：%d %s", r.status, r.body)
		}
		if r := send(t, nil, "POST", base+"/api/v1/server/limit/set/hk-1", map[string]string{"Authorization": "Bearer " + ops}, body); r.status != 200 || !strings.Contains(r.body, `"effective_mbps":5`) {
			t.Fatalf("日常运维令牌经 REST 设临时限速：%d %s", r.status, r.body)
		}

		// 踢线：给与不给入站。
		stdout, stderr, code := h.cli("server", "kick", "hk-1", "--credential", "a", "--inbound", "vless-in", "--json")
		var kick xrpc.ConnectionsKickParams
		node.params(xrpc.MethodConnectionsKick, &kick)
		if code != 0 || !strings.Contains(stdout, `"kicked":2`) || kick.Inbound != "vless-in" || kick.Credential != "a" {
			t.Fatalf("给入站踢线：%s %s %+v", stdout, stderr, kick)
		}
		stdout, _, _ = h.cli("server", "kick", "hk-1", "--credential", "a", "--json")
		kick = xrpc.ConnectionsKickParams{}
		node.params(xrpc.MethodConnectionsKick, &kick)
		if !strings.Contains(stdout, `"kicked":4`) || kick.Inbound != "" {
			t.Fatalf("不给入站踢全部：%s %+v", stdout, kick)
		}

		// 列连接：5 条，按 id 排，3 条 tcp、2 条 udp；分页。
		rows, total := h.cliPage("server", "connections", "hk-1", "--inbound", "vless-in", "--credential", "a")
		var networks []string
		for i, row := range rows {
			if row["id"] != float64(i+1) {
				t.Fatalf("应当按 id 排：%v", rows)
			}
			networks = append(networks, row["network"].(string))
		}
		if total != 5 || len(rows) != 5 || slices.Index(networks, "udp") < 0 || strings.Count(strings.Join(networks, ","), "tcp") != 3 {
			t.Fatalf("应当列出 5 条，3 条 tcp：%d %v", total, rows)
		}
		var list xrpc.ConnectionsListParams
		node.params(xrpc.MethodConnectionsList, &list)
		if list != (xrpc.ConnectionsListParams{Inbound: "vless-in", Credential: "a"}) {
			t.Fatalf("节点收到的列连接参数不对：%+v", list)
		}
		if rows, _ := h.cliPage("server", "connections", "hk-1", "--inbound", "vless-in", "--credential", "a", "--limit", "2"); len(rows) != 2 {
			t.Fatalf("--limit 2 应当只给 2 条：%v", rows)
		}

		// 错误：不在线立即 unavailable、节点拒绝 bad_request、旧版 agent conflict、服务器不存在 not_found。
		began := time.Now()
		if e := h.cliErr("server", "limit", "set", "hk-2", "--inbound", "vless-in", "--credential", "a", "--mbps", "5"); e.Code != v1.CodeUnavailable ||
			!strings.Contains(e.Next, "server get") || time.Since(began) > 2*time.Second {
			t.Fatalf("不在线应当立即 unavailable：%+v %v", e, time.Since(began))
		}
		if e := h.cliErr("server", "limit", "set", "hk-1", "--inbound", "nope", "--credential", "a", "--mbps", "5"); e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "nope") {
			t.Fatalf("入站不存在应当 bad_request 并带节点的原因：%+v", e)
		}
		if e := h.cliErr("server", "kick", "hk-3", "--credential", "a"); e.Code != v1.CodeConflict || !strings.Contains(e.Reason, "satchel-agent") {
			t.Fatalf("旧版 agent 应当 conflict：%+v", e)
		}
		if e := h.cliErr("server", "kick", "hk-1", "--credential", "boom"); e.Code != v1.CodeInternal || !strings.Contains(e.Reason, "tracker 出错了") {
			t.Fatalf("节点执行失败应当 internal 并带节点的原因：%+v", e)
		}
		if e := h.cliErr("server", "traffic", "nope"); e.Code != v1.CodeNotFound {
			t.Fatalf("服务器不存在应当 not_found：%+v", e)
		}

		// 拉取与累计：还没收到快照时组合为空。
		traffic := func(name string) svcservers.TrafficView {
			t.Helper()
			stdout, stderr, code := h.cli("server", "traffic", name, "--json")
			if code != 0 {
				t.Fatalf("server traffic：%s", stderr)
			}
			var v svcservers.TrafficView
			if err := json.Unmarshal([]byte(stdout), &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		if v := traffic("hk-1"); len(v.Traffic) != 0 || v.CollectedAt != nil {
			t.Fatalf("还没收到快照时应当为空：%+v", v)
		}
		now := time.Now()
		collect := func(d time.Duration) string {
			t.Helper()
			detail, err := h.app.servers.CollectTraffic(func() time.Time { return now.Add(d) })(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			return detail
		}
		setInterval := func(n string) {
			env, _, _ := h.settingsCLI("settings", "show")
			if _, e, _ := h.settingsCLI("settings", "set", "--set", "traffic_collect_interval="+n, "--resource-version", jsonInt(env.Metadata.ResourceVersion)); e != nil {
				t.Fatalf("settings set：%v", e)
			}
		}
		mbVless := func(boot string, gen uint64, down int64) xrpc.TrafficSnapshot {
			started := now.Add(-time.Hour).UTC()
			return xrpc.TrafficSnapshot{BootID: boot, Generation: gen, KernelRunning: true, KernelVersion: "1.14.4", KernelStartedAt: &started,
				Counts: []xrpc.TrafficCount{{Inbound: "vless-in", Outbound: "direct", Credential: "a", Down: down << 20}}}
		}
		down := func() int64 {
			for _, row := range traffic("hk-1").Traffic {
				if row.Inbound == "vless-in" && row.Outbound == "direct" && row.Credential == "a" {
					return row.Downlink >> 20
				}
			}
			return 0
		}
		bootCount := func() int64 { return h.server("hk-1").Status.CoreBootCount }

		// 按间隔拉：间隔 10 秒时，0、5、10、25 秒这四次里拉了三次（hk-3 每次都失败）；改回 60 秒后，35 秒不拉、70 秒拉。
		setInterval("10")
		node.setSnap(mbVless("b1", 1, 100))
		for _, step := range []struct {
			at    time.Duration
			pulls int
		}{{0, 1}, {5 * time.Second, 1}, {10 * time.Second, 2}, {25 * time.Second, 3}} {
			detail := collect(step.at)
			if node.pullCount() != step.pulls {
				t.Fatalf("第 %v 秒之后应当拉了 %d 次：%d（%s）", step.at, step.pulls, node.pullCount(), detail)
			}
			if step.at == 0 && detail != "拉了 1 台、失败 1 台" {
				t.Fatalf("hk-3 不认识方法应当算失败、不影响 hk-1：%s", detail)
			}
		}
		setInterval("60")
		collect(35 * time.Second)
		if node.pullCount() != 3 {
			t.Fatalf("间隔 60 秒时距上次 10 秒不应当拉：%d", node.pullCount())
		}
		collect(85 * time.Second)
		if node.pullCount() != 4 {
			t.Fatalf("间隔 60 秒时距上次 60 秒应当拉：%d", node.pullCount())
		}
		if got, s := down(), h.server("hk-1").Status; got != 100 || !s.CoreRunning || s.CoreVersion != "1.14.4" || s.CoreBootCount != 0 || s.CoreBootTime == nil {
			t.Fatalf("第一份快照照值计入、不算重启：%d %+v", got, s)
		}

		// 同一份快照收两次不变；同进程内核重启与换了进程各记一次重启，累计 150 MB。
		collect(150 * time.Second)
		if down() != 100 || bootCount() != 0 {
			t.Fatalf("同一份快照收两次累计不应当变：%d %d", down(), bootCount())
		}
		node.setSnap(mbVless("b1", 2, 150))
		collect(220 * time.Second)
		if down() != 150 || bootCount() != 1 {
			t.Fatalf("同进程内核重启：累计 150、重启 1 次：%d %d", down(), bootCount())
		}
		node.setSnap(mbVless("b2", 1, 50))
		collect(290 * time.Second)
		if down() != 200 || bootCount() != 2 {
			t.Fatalf("换了进程：累计 200（150 + 50）、重启再加 1：%d %d", down(), bootCount())
		}

		// 内核停了（同一个进程，计数还在 tracker 里）：core_running 为 false，累计不变。
		stopped := mbVless("b2", 1, 50)
		stopped.KernelRunning, stopped.KernelVersion, stopped.KernelStartedAt = false, "", nil
		node.setSnap(stopped)
		collect(360 * time.Second)
		if s := h.server("hk-1").Status; s.CoreRunning || s.CoreVersion != "" || down() != 200 {
			t.Fatalf("内核没在跑时 core_running 应当为 false、累计不变：%+v %d", s, down())
		}

		// 断开以后还能看。
		a.ws.CloseNow()
		h.waitStatus("hk-1", "offline")
		if v := traffic("hk-1"); v.CollectedAt == nil || len(v.Traffic) != 1 || v.Traffic[0].Downlink>>20 != 200 {
			t.Fatalf("断开以后仍应当看得到累计：%+v", v)
		}
		if r := send(t, nil, "GET", base+"/api/v1/server/traffic/hk-1", map[string]string{"Authorization": "Bearer " + ro}, ""); r.status != 200 || !strings.Contains(r.body, `"core_boot_count":2`) {
			t.Fatalf("只读令牌经 REST 看流量：%d %s", r.status, r.body)
		}
	})
}
