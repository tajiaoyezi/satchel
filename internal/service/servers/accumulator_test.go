package servers

import (
	"testing"
	"time"

	"github.com/satchel/satchel/pkg/xrpc"
)

const mb = 1 << 20

func snap(boot string, gen uint64, counts ...xrpc.TrafficCount) xrpc.TrafficSnapshot {
	return xrpc.TrafficSnapshot{BootID: boot, Generation: gen, Counts: counts}
}

func count(in, out, cred string, up, down int64) xrpc.TrafficCount {
	return xrpc.TrafficCount{Inbound: in, Outbound: out, Credential: cred, Up: up, Down: down}
}

// total 取一个组合的累计；没有这个组合时是零。
func total(a *accumulator, in, out, cred string) (up, down int64) {
	for _, t := range a.totals() {
		if t.Inbound == in && t.Outbound == out && t.Credential == cred {
			return t.Uplink, t.Downlink
		}
	}
	return 0, 0
}

// node-traffic「主控侧的累计与重启基线」的各条规则，每一步都核对累计与这一步检测到的重启次数。
func TestAccumulator(t *testing.T) {
	type step struct {
		snap     xrpc.TrafficSnapshot
		restarts int
		down     int64 // vless→direct→a 的下行累计
	}
	cases := map[string][]step{
		"第一份快照照值计入、不算重启": {
			{snap("b1", 1, count("vless", "direct", "a", 1, 100*mb)), 0, 100 * mb},
		},
		"同进程内核重启": {
			{snap("b1", 1, count("vless", "direct", "a", 0, 100*mb)), 0, 100 * mb},
			{snap("b1", 2, count("vless", "direct", "a", 0, 150*mb)), 1, 150 * mb},
		},
		"换了进程": {
			{snap("b1", 1, count("vless", "direct", "a", 0, 100*mb)), 0, 100 * mb},
			{snap("b2", 1, count("vless", "direct", "a", 0, 50*mb)), 1, 150 * mb},
		},
		"同一份快照收两次": {
			{snap("b1", 1, count("vless", "direct", "a", 0, 100*mb)), 0, 100 * mb},
			{snap("b1", 1, count("vless", "direct", "a", 0, 100*mb)), 0, 100 * mb},
		},
		"内核代数一次跳两代": {
			{snap("b1", 1, count("vless", "direct", "a", 0, 10)), 0, 10},
			{snap("b1", 3, count("vless", "direct", "a", 0, 30)), 2, 30},
		},
		"换了进程且新进程里内核已启动两次": {
			{snap("b1", 1, count("vless", "direct", "a", 0, 100)), 0, 100},
			{snap("b2", 2, count("vless", "direct", "a", 0, 50)), 2, 150},
		},
		"换了进程但新进程里内核还没起来": {
			{snap("b1", 1, count("vless", "direct", "a", 0, 100)), 0, 100},
			{snap("b2", 0), 1, 100},
		},
	}
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			a := &accumulator{}
			for i, s := range steps {
				if got := a.add(s.snap, now.Add(time.Duration(i)*time.Minute)); got != s.restarts {
					t.Fatalf("第 %d 步检测到的重启应当是 %d：%d", i+1, s.restarts, got)
				}
				if _, down := total(a, "vless", "direct", "a"); down != s.down {
					t.Fatalf("第 %d 步的下行累计应当是 %d：%d", i+1, s.down, down)
				}
			}
			if !a.since.Equal(now) || !a.at.Equal(now.Add(time.Duration(len(steps)-1)*time.Minute)) {
				t.Fatalf("累计起点与最近一次收到快照的时间不对：%v %v", a.since, a.at)
			}
		})
	}
}

// 上下行分开计；同一份凭据分到两个出站各记各的；换了进程以后旧进程里有、新进程里没有的组合留在基线里。
func TestAccumulatorCombos(t *testing.T) {
	a, now := &accumulator{}, time.Now()
	a.add(snap("b1", 1, count("in1", "out1", "a", 3, 100), count("in1", "out2", "a", 4, 60), count("in2", "out2", "b", 5, 40)), now)
	a.add(snap("b2", 1, count("in1", "out1", "a", 1, 10)), now)
	want := map[[3]string][2]int64{
		{"in1", "out1", "a"}: {4, 110},
		{"in1", "out2", "a"}: {4, 60},
		{"in2", "out2", "b"}: {5, 40},
	}
	got := a.totals()
	if len(got) != len(want) {
		t.Fatalf("组合数应当是 %d：%+v", len(want), got)
	}
	for _, g := range got {
		if w := want[[3]string{g.Inbound, g.Outbound, g.Credential}]; w != [2]int64{g.Uplink, g.Downlink} {
			t.Fatalf("%+v 应当是 %v", g, w)
		}
	}
	if got[0].Inbound != "in1" || got[0].Outbound != "out1" || got[2].Inbound != "in2" {
		t.Fatalf("应当按入站、出站、凭据排序：%+v", got)
	}
}
