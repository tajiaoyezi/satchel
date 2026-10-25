package servers

import (
	"cmp"
	"slices"
	"time"

	"github.com/satchel/satchel/pkg/xrpc"
)

// trafficKey 是累计的键：一个（入站，出站，凭据）组合。
type trafficKey struct {
	Inbound, Outbound, Credential string
}

// TrafficTotal 是一个组合的累计字节数（应用层，上下行分开）。
type TrafficTotal struct {
	Inbound    string `json:"inbound"`
	Outbound   string `json:"outbound"`
	Credential string `json:"credential"`
	Uplink     int64  `json:"uplink"`
	Downlink   int64  `json:"downlink"`
}

// accumulator 是一台服务器的累计（m2-03 design 第 6 条，node-traffic「主控侧的累计与重启基线」），规则照 satchel-agent 的
// kerneltest.Accumulator：
//   - 第一份快照照值计入、不算重启（主控内存里的累计从零开始，照值计入不会重复）；
//   - 同一个启动标识：累计 = 基线 + 这次的值；
//   - 启动标识变了（换了进程，计数从零开始）：把上一份快照的值并进基线，这次从零开始加；
//   - 内核代数变大或启动标识变了：重启次数相应增加，换了进程时新进程里的每次内核启动都算一次。
type accumulator struct {
	since      time.Time // 收到第一份快照的时间：本次累计的起点
	at         time.Time // 最近一次收到快照的时间
	bootID     string
	generation uint64
	base, last map[trafficKey][2]int64
	bootBase   int64 // 开始累计时库里的 core_boot_count
	based      bool  // bootBase 读到了没有：没读到时下一次拉取再读（在那之前主控没写过这一列，晚读读到的仍是原值）
	restarts   int64 // 从开始累计起检测到的重启总数；写库时写 bootBase + restarts 这个绝对值
}

// add 收一份快照，回这次检测到的内核重启次数。
func (a *accumulator) add(s xrpc.TrafficSnapshot, now time.Time) int {
	restarts := 0
	switch {
	case a.base == nil:
		a.base, a.since = map[trafficKey][2]int64{}, now
	case s.BootID != a.bootID:
		for k, v := range a.last {
			b := a.base[k]
			a.base[k] = [2]int64{b[0] + v[0], b[1] + v[1]}
		}
		restarts = int(max(s.Generation, 1))
	case s.Generation > a.generation:
		restarts = int(s.Generation - a.generation)
	}
	a.bootID, a.generation, a.at = s.BootID, s.Generation, now
	a.last = make(map[trafficKey][2]int64, len(s.Counts))
	for _, c := range s.Counts {
		a.last[trafficKey{c.Inbound, c.Outbound, c.Credential}] = [2]int64{c.Up, c.Down}
	}
	return restarts
}

// totals 返回每个组合的累计，按入站、出站、凭据排序。
func (a *accumulator) totals() []TrafficTotal {
	sum := make(map[trafficKey][2]int64, len(a.base)+len(a.last))
	for _, m := range []map[trafficKey][2]int64{a.base, a.last} {
		for k, v := range m {
			s := sum[k]
			sum[k] = [2]int64{s[0] + v[0], s[1] + v[1]}
		}
	}
	out := make([]TrafficTotal, 0, len(sum))
	for k, v := range sum {
		out = append(out, TrafficTotal{Inbound: k.Inbound, Outbound: k.Outbound, Credential: k.Credential, Uplink: v[0], Downlink: v[1]})
	}
	slices.SortFunc(out, func(x, y TrafficTotal) int {
		return cmp.Or(cmp.Compare(x.Inbound, y.Inbound), cmp.Compare(x.Outbound, y.Outbound), cmp.Compare(x.Credential, y.Credential))
	})
	return out
}
