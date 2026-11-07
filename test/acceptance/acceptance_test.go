package acceptance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	kt "github.com/satchel/satchel-agent/pkg/kerneltest"
)

// M2 证伪点的正式验收（m2-04 node-acceptance，技术方案第 13 章）：
// 四容器环境里只经主控的 REST 接口操作节点与读取结果（唯一例外是重启基线里经 Docker socket 向节点容器发 SIGKILL）。

const (
	serverName    = "hk-1"
	masterBaseURL = "https://master.test"
	nodeContainer = "satchel-acceptance-node"

	nodeHost   = "172.30.204.20"
	vlessPort  = 8443
	hy2Port    = 8444
	driverHost = "172.30.204.30"
	sink1Port  = 17001
	sink2Port  = 17002

	limitMbps   = 5
	bLoad       = 40_000_000 // B 的负载固定 40 Mbps：bridge 网络容量远大于所有并行流的目标之和
	tolerance   = 0.2
	applyWait   = 2 * time.Second
	firstWindow = 2 * time.Second
)

var (
	inbounds = []string{kt.InVLESS, kt.InHY2}
	target   = kt.Target{
		Host:      nodeHost,
		VLESSPort: vlessPort,
		HY2Port:   hy2Port,
		SinkHost:  driverHost,
		SinkPorts: [3]int{0, sink1Port, sink2Port},
	}
)

type profile struct {
	name   string
	warmup time.Duration
	window time.Duration
	mib    int64
}

func currentProfile() profile {
	if os.Getenv("ACCEPTANCE_QUICK") == "1" {
		return profile{
			name:   "快速版",
			warmup: time.Second,
			window: 3 * time.Second,
			mib:    1 << 20 / 10,
		}
	}
	return profile{
		name:   "完整版",
		warmup: 2 * time.Second,
		window: 30 * time.Second,
		mib:    1 << 20,
	}
}

var (
	sinksOnce sync.Once
	sink1     *kt.Sink
	sink2     *kt.Sink
)

func ensureSinks(t *testing.T) (*kt.Sink, *kt.Sink) {
	t.Helper()
	sinksOnce.Do(func() {
		sink1 = kt.StartSinkAt(&persistentTB{t}, "0.0.0.0", sink1Port)
		sink2 = kt.StartSinkAt(&persistentTB{t}, "0.0.0.0", sink2Port)
	})
	return sink1, sink2
}

// persistentTB 让收发流量服务在整个测试进程期间保持监听，不在单个顶层测试结束时关掉。
type persistentTB struct{ testing.TB }

func (p *persistentTB) Cleanup(func()) {}

type masterClient struct {
	http   *http.Client
	docker *http.Client
	token  string
}

func newMasterClient(t *testing.T) *masterClient {
	t.Helper()
	raw, err := os.ReadFile("/shared/token")
	if err != nil {
		t.Fatalf("读 /shared/token 失败：%v（验收测试由 test/acceptance/run.sh 在 driver 容器里运行）", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		t.Fatal("/shared/token 为空")
	}
	dockerTr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", "/var/run/docker.sock")
		},
	}
	return &masterClient{
		http:   &http.Client{Timeout: 70 * time.Second},
		docker: &http.Client{Transport: dockerTr, Timeout: 15 * time.Second},
		token:  tok,
	}
}

func (m *masterClient) get(t *testing.T, path string, query url.Values, out any) {
	t.Helper()
	u := masterBaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	m.do(t, req, out)
}

func (m *masterClient) post(t *testing.T, path string, body any, out any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, masterBaseURL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")
	m.do(t, req, out)
}

func (m *masterClient) do(t *testing.T, req *http.Request, out any) {
	t.Helper()
	resp, err := m.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败：%v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读 %s %s 响应失败：%v", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s 状态码 %d：%s", req.Method, req.URL.Path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("解码 %s %s 响应失败：%v（%s）", req.Method, req.URL.Path, err, data)
		}
	}
}

func (m *masterClient) limitSet(t *testing.T, inbound, credential string, mbpsVal int) {
	t.Helper()
	m.post(t, "/api/v1/server/limit/set/"+serverName, map[string]any{
		"inbound":    inbound,
		"credential": credential,
		"mbps":       mbpsVal,
	}, nil)
}

func (m *masterClient) limitClear(t *testing.T, inbound, credential string) {
	t.Helper()
	m.post(t, "/api/v1/server/limit/clear/"+serverName, map[string]any{
		"inbound":    inbound,
		"credential": credential,
	}, nil)
}

func (m *masterClient) kick(t *testing.T, credential, inbound string) int {
	t.Helper()
	body := map[string]any{"credential": credential}
	if inbound != "" {
		body["inbound"] = inbound
	}
	var res struct {
		Kicked int `json:"kicked"`
	}
	m.post(t, "/api/v1/server/kick/"+serverName, body, &res)
	return res.Kicked
}

type connItem struct {
	ID       uint64    `json:"id"`
	Network  string    `json:"network"`
	Peer     string    `json:"peer"`
	Outbound string    `json:"outbound"`
	Start    time.Time `json:"start"`
}

func (m *masterClient) connections(t *testing.T, inbound, credential string) []connItem {
	t.Helper()
	var all []connItem
	cursor := ""
	for {
		q := url.Values{
			"inbound":    {inbound},
			"credential": {credential},
			"limit":      {"100"},
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Items      []connItem `json:"items"`
			NextCursor string     `json:"next_cursor"`
		}
		m.get(t, "/api/v1/server/connections/"+serverName, q, &page)
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			return all
		}
		cursor = page.NextCursor
	}
}

func (m *masterClient) waitConns(t *testing.T, inbound, credential string, want int, timeout time.Duration) []connItem {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []connItem
	for {
		last = m.connections(t, inbound, credential)
		if len(last) == want || time.Now().After(deadline) {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type trafficTotal struct {
	Inbound    string `json:"inbound"`
	Outbound   string `json:"outbound"`
	Credential string `json:"credential"`
	Uplink     int64  `json:"uplink"`
	Downlink   int64  `json:"downlink"`
}

type trafficView struct {
	CollectedAt   *time.Time     `json:"collected_at"`
	CoreRunning   bool           `json:"core_running"`
	CoreVersion   string         `json:"core_version"`
	CoreBootCount int64          `json:"core_boot_count"`
	Traffic       []trafficTotal `json:"traffic"`
}

func (m *masterClient) traffic(t *testing.T) trafficView {
	t.Helper()
	var v trafficView
	m.get(t, "/api/v1/server/traffic/"+serverName, nil, &v)
	return v
}

// waitSnapshot 等主控收到一份 collected_at 严格晚于 after 的流量快照（最多等 80 秒：主控启动后首次调度要等 60 秒）。
func (m *masterClient) waitSnapshot(t *testing.T, after time.Time) trafficView {
	t.Helper()
	prev := m.traffic(t).CollectedAt
	deadline := time.Now().Add(80 * time.Second)
	for time.Now().Before(deadline) {
		v := m.traffic(t)
		if v.CollectedAt != nil && v.CollectedAt.After(after) && (prev == nil || !v.CollectedAt.Equal(*prev)) {
			return v
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("80 秒内主控没有收到晚于 %s 的新流量快照", after.UTC().Format(time.RFC3339Nano))
	return trafficView{}
}

func (m *masterClient) restartKernel(t *testing.T) {
	t.Helper()
	var res struct {
		Running bool `json:"running"`
	}
	m.post(t, "/api/v1/server/restart-kernel/"+serverName, map[string]any{"confirm": serverName}, &res)
	if !res.Running {
		t.Fatal("server restart-kernel 返回 running=false")
	}
}

type serverStatus struct {
	Status    string     `json:"status"`
	BootTime  *time.Time `json:"boot_time"`
	UpdatedAt *time.Time `json:"last_heartbeat"`
}

func (m *masterClient) getServerStatus(t *testing.T) serverStatus {
	t.Helper()
	var srv struct {
		Status serverStatus `json:"status"`
	}
	m.get(t, "/api/v1/server/get/"+serverName, nil, &srv)
	return srv.Status
}

// killAndStartNode 经 Docker socket 向节点容器发 SIGKILL 再启动，并等节点重新连回主控且入站端口就绪（design 第 7、8 条）。
func (m *masterClient) killAndStartNode(t *testing.T) {
	t.Helper()
	beforeBoot := m.getServerStatus(t).BootTime
	for _, action := range []string{"kill?signal=KILL", "start"} {
		req, err := http.NewRequest(http.MethodPost, "http://unix/containers/"+nodeContainer+"/"+action, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := m.docker.Do(req)
		if err != nil {
			t.Fatalf("Docker %s 失败：%v", action, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
			t.Fatalf("Docker %s 状态码 %d：%s", action, resp.StatusCode, body)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st := m.getServerStatus(t)
		if st.Status == "connected" && st.BootTime != nil && (beforeBoot == nil || !st.BootTime.Equal(*beforeBoot)) {
			if conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", nodeHost, vlessPort), time.Second); err == nil {
				conn.Close()
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("SIGKILL 并重启节点容器后 30 秒内没有重新连回主控")
}

func settle(t *testing.T, m *masterClient) {
	t.Helper()
	for _, user := range []string{kt.UserA, kt.UserB} {
		m.kick(t, user, "")
	}
	for _, in := range inbounds {
		for _, user := range []string{kt.UserA, kt.UserB} {
			if left := m.waitConns(t, in, user, 0, 5*time.Second); len(left) != 0 {
				t.Fatalf("%s@%s 还剩 %d 条连接", user, in, len(left))
			}
		}
	}
}

func mbps(n float64) int64 { return int64(n * 1e6) }

func near(got, want float64) bool { return got >= want*(1-tolerance) && got <= want*(1+tolerance) }

func measure(window time.Duration, groups ...[]*kt.Meter) []float64 {
	start := make([][]int64, len(groups))
	for i, g := range groups {
		for _, m := range g {
			start[i] = append(start[i], m.Bytes())
		}
	}
	t0 := time.Now()
	time.Sleep(window)
	elapsed := time.Since(t0).Seconds()
	out := make([]float64, len(groups))
	for i, g := range groups {
		var n int64
		for j, m := range g {
			n += m.Bytes() - start[i][j]
		}
		out[i] = float64(n) * 8 / elapsed / 1e6
	}
	return out
}

func meters(flows ...*kt.Flow) []*kt.Meter {
	out := make([]*kt.Meter, len(flows))
	for i, f := range flows {
		out[i] = f.Meter
	}
	return out
}

func stopAll(flows ...*kt.Flow) {
	for _, f := range flows {
		f.Stop()
	}
}

func downloads(t *testing.T, c *kt.Client, n int) []*kt.Flow {
	var out []*kt.Flow
	for i := 0; i < n; i++ {
		out = append(out, kt.Download(t, c, 1, 0, 0))
	}
	return out
}

func connIDs(m *masterClient, t *testing.T, inbound, user string) string {
	t.Helper()
	var ids []string
	for _, c := range m.connections(t, inbound, user) {
		ids = append(ids, fmt.Sprint(c.ID))
	}
	return strings.Join(ids, ",")
}

func waitDone(t *testing.T, d time.Duration, flows ...*kt.Flow) {
	t.Helper()
	deadline := time.After(d)
	for _, f := range flows {
		select {
		case <-f.Done():
		case <-deadline:
			t.Fatalf("%v 内流没有跑完", d)
		}
	}
}

type clients struct {
	t *testing.T
	m map[string]*kt.Client
}

func (c *clients) get(user, inbound string) *kt.Client {
	if c.m == nil {
		c.m = map[string]*kt.Client{}
	}
	key := user + "@" + inbound
	if c.m[key] == nil {
		c.m[key] = kt.StartClient(c.t, target, user, inbound)
	}
	return c.m[key]
}

// 第 1 项：验收里的按凭据限速（node-acceptance「验收里的按凭据限速」）。
func TestAcceptanceLimit(t *testing.T) {
	prof := currentProfile()
	s1, _ := ensureSinks(t)
	m := newMasterClient(t)
	cl := &clients{t: t}
	t.Cleanup(func() {
		for _, in := range inbounds {
			for _, user := range []string{kt.UserA, kt.UserB} {
				m.limitClear(t, in, user)
			}
		}
		settle(t, m)
	})

	for _, in := range inbounds {
		for _, user := range []string{kt.UserA, kt.UserB} {
			m.limitClear(t, in, user)
		}
	}
	settle(t, m)

	// 基线：B 不限速、单独跑一个时间窗，两个入站各量一次。
	baseline := map[string]float64{}
	for _, in := range inbounds {
		f := kt.Download(t, cl.get(kt.UserB, in), 1, bLoad, 0)
		time.Sleep(prof.warmup)
		baseline[in] = f.Meter.Mbps(prof.window)
		f.Stop()
		t.Logf("结果（%s）基线 %s：B 不限速 %.2f Mbps", prof.name, in, baseline[in])
		if baseline[in] < 30 {
			t.Fatalf("%s 上 B 的基线只有 %.2f Mbps，负载是 40 Mbps，链路容量不够", in, baseline[in])
		}
	}
	for _, in := range inbounds {
		m.limitSet(t, in, kt.UserA, limitMbps)
	}
	checkB := func(t *testing.T, in string, got float64) {
		t.Helper()
		if got < 0.9*baseline[in] {
			t.Errorf("%s 上 B 只有 %.2f Mbps，低于基线 %.2f 的 90%%", in, got, baseline[in])
		}
	}

	for _, in := range inbounds {
		name := map[string]string{kt.InVLESS: "1. VLESS 上的 TCP", kt.InHY2: "2. Hysteria2 上"}[in]
		t.Run(name, func(t *testing.T) {
			settle(t, m)
			a := downloads(t, cl.get(kt.UserA, in), 4)
			b := kt.Download(t, cl.get(kt.UserB, in), 1, bLoad, 0)
			defer stopAll(append(a, b)...)
			time.Sleep(prof.warmup)
			got := measure(prof.window, meters(a...), meters(b))
			t.Logf("结果（%s）%s：A 合计 %.2f Mbps（目标 5），B %.2f Mbps", prof.name, name, got[0], got[1])
			if !near(got[0], limitMbps) {
				t.Errorf("A 合计 %.2f Mbps，不在 5 Mbps 的 ±20%%", got[0])
			}
			checkB(t, in, got[1])
		})
	}

	t.Run("3. 两个入站一起", func(t *testing.T) {
		settle(t, m)
		a1 := downloads(t, cl.get(kt.UserA, kt.InVLESS), 4)
		a2 := downloads(t, cl.get(kt.UserA, kt.InHY2), 4)
		b1 := kt.Download(t, cl.get(kt.UserB, kt.InVLESS), 1, bLoad, 0)
		b2 := kt.Download(t, cl.get(kt.UserB, kt.InHY2), 1, bLoad, 0)
		defer stopAll(append(append(a1, a2...), b1, b2)...)
		time.Sleep(prof.warmup)
		got := measure(prof.window, meters(a1...), meters(a2...), meters(b1), meters(b2))
		t.Logf("结果（%s）3. 两个入站一起：A@VLESS %.2f、A@Hysteria2 %.2f Mbps（各自目标 5），B %.2f / %.2f Mbps", prof.name, got[0], got[1], got[2], got[3])
		if !near(got[0], limitMbps) || !near(got[1], limitMbps) {
			t.Errorf("两个入站应当各自约 5 Mbps：%.2f / %.2f", got[0], got[1])
		}
		checkB(t, kt.InVLESS, got[2])
		checkB(t, kt.InHY2, got[3])
	})

	t.Run("4. 上下行合计", func(t *testing.T) {
		settle(t, m)
		c := cl.get(kt.UserA, kt.InVLESS)
		down := downloads(t, c, 2)
		up := []*kt.Flow{kt.Upload(t, s1, c, 1, "case4-1", 0, 0), kt.Upload(t, s1, c, 1, "case4-2", 0, 0)}
		b := kt.Download(t, cl.get(kt.UserB, kt.InVLESS), 1, bLoad, 0)
		defer stopAll(append(append(down, up...), b)...)
		time.Sleep(prof.warmup)
		got := measure(prof.window, meters(down...), meters(up...), meters(b))
		t.Logf("结果（%s）4. 上下行合计：A 下行 %.2f + 上行 %.2f = %.2f Mbps（目标 5），B %.2f Mbps", prof.name, got[0], got[1], got[0]+got[1], got[2])
		if !near(got[0]+got[1], limitMbps) {
			t.Errorf("上下行合计 %.2f Mbps，不在 5 Mbps 的 ±20%%", got[0]+got[1])
		}
		checkB(t, kt.InVLESS, got[2])
	})

	t.Run("UDP 也受同一个桶限制", func(t *testing.T) {
		settle(t, m)
		f := kt.UDPDownload(t, cl.get(kt.UserA, kt.InVLESS), 1, mbps(20), prof.warmup+prof.window+5*time.Second)
		defer f.Stop()
		time.Sleep(prof.warmup)
		got := f.Meter.Mbps(prof.window)
		t.Logf("结果（%s）UDP：A 在 VLESS 上 %.2f Mbps（服务端按 20 Mbps 发，目标 5）", prof.name, got)
		if !near(got, limitMbps) {
			t.Errorf("UDP %.2f Mbps，不在 5 Mbps 的 ±20%%", got)
		}
	})

	t.Run("TCP 与 UDP 共用一个桶", func(t *testing.T) {
		settle(t, m)
		c := cl.get(kt.UserA, kt.InVLESS)
		tcp := downloads(t, c, 4)
		udp := kt.UDPDownload(t, c, 1, mbps(20), prof.warmup+prof.window+5*time.Second)
		defer stopAll(append(tcp, udp)...)
		time.Sleep(prof.warmup)
		got := measure(prof.window, meters(tcp...), meters(udp))
		t.Logf("结果（%s）TCP 与 UDP 一起跑：A 在 VLESS 上 TCP %.2f + UDP %.2f = %.2f Mbps（目标 5）", prof.name, got[0], got[1], got[0]+got[1])
		if !near(got[0]+got[1], limitMbps) {
			t.Errorf("TCP 与 UDP 合计 %.2f Mbps，不在 5 Mbps 的 ±20%%", got[0]+got[1])
		}
		if got[0] == 0 || got[1] == 0 {
			t.Errorf("时间窗里 TCP 与 UDP 都应当有数据：TCP %.2f、UDP %.2f Mbps", got[0], got[1])
		}
	})

	t.Run("5-7. 改值、加限速、撤掉，都作用在已有连接上", func(t *testing.T) {
		settle(t, m)
		a := downloads(t, cl.get(kt.UserA, kt.InVLESS), 4)
		b := kt.Download(t, cl.get(kt.UserB, kt.InVLESS), 1, bLoad, 0)
		defer stopAll(append(a, b)...)
		time.Sleep(prof.warmup)
		aIDs, bIDs := connIDs(m, t, kt.InVLESS, kt.UserA), connIDs(m, t, kt.InVLESS, kt.UserB)
		sameConns := func(step string) {
			t.Helper()
			if connIDs(m, t, kt.InVLESS, kt.UserA) != aIDs || connIDs(m, t, kt.InVLESS, kt.UserB) != bIDs {
				t.Fatalf("%s：连接变了，说明发生了重连", step)
			}
		}

		m.limitSet(t, kt.InVLESS, kt.UserA, 10)
		time.Sleep(applyWait)
		first := measure(firstWindow, meters(a...))[0]
		got := measure(prof.window, meters(a...))[0]
		t.Logf("结果（%s）5. A 改成 10 Mbps，%v 后紧接着的 %v：%.2f Mbps，随后 %v：%.2f Mbps", prof.name, applyWait, firstWindow, first, prof.window, got)
		if !near(first, 10) || !near(got, 10) {
			t.Errorf("改成 10 Mbps 后 %.2f / %.2f Mbps，不在 ±20%%", first, got)
		}
		sameConns("5")

		m.limitSet(t, kt.InVLESS, kt.UserB, limitMbps)
		time.Sleep(applyWait)
		first = measure(firstWindow, meters(b))[0]
		got = measure(prof.window, meters(b))[0]
		t.Logf("结果（%s）6. B 中途加主控临时 5 Mbps，%v 后紧接着的 %v：%.2f Mbps，随后 %v：%.2f Mbps", prof.name, applyWait, firstWindow, first, prof.window, got)
		if !near(first, limitMbps) || !near(got, limitMbps) {
			t.Errorf("加限速后 B %.2f / %.2f Mbps，不在 5 Mbps 的 ±20%%", first, got)
		}
		sameConns("6")

		m.limitClear(t, kt.InVLESS, kt.UserB)
		time.Sleep(applyWait)
		first = measure(firstWindow, meters(b))[0]
		got = measure(prof.window, meters(b))[0]
		t.Logf("结果（%s）7. 撤掉 B 的主控临时，%v 后紧接着的 %v：%.2f Mbps，随后 %v：%.2f Mbps（基线 %.2f）", prof.name, applyWait, firstWindow, first, prof.window, got, baseline[kt.InVLESS])
		checkB(t, kt.InVLESS, first)
		checkB(t, kt.InVLESS, got)
		sameConns("7")
	})
}

// 第 2 项：验收里的按凭据识别连接（node-acceptance「验收里的按凭据识别连接」）。
func TestAcceptanceConnections(t *testing.T) {
	prof := currentProfile()
	ensureSinks(t)
	m := newMasterClient(t)
	settle(t, m)
	t.Cleanup(func() { settle(t, m) })

	sinkOf := map[string][2]int{kt.UserA: {1, 2}, kt.UserB: {2, 1}} // {TCP 去的, UDP 去的}
	outOf := map[int]string{1: kt.Out1, 2: kt.Out2}
	relays := map[string]*kt.Relay{}
	for _, user := range []string{kt.UserA, kt.UserB} {
		for _, in := range inbounds {
			network, port := "tcp", vlessPort
			if in == kt.InHY2 {
				network, port = "udp", hy2Port
			}
			r := kt.StartRelay(t, network, fmt.Sprintf("%s:%d", nodeHost, port))
			relays[user+"@"+in] = r
			c := kt.StartClientVia(t, target, user, in, r.Port)
			for i := 0; i < 3; i++ {
				kt.Echo(t, c, sinkOf[user][0])
			}
			for i := 0; i < 2; i++ {
				kt.EchoUDP(t, c, sinkOf[user][1])
			}
		}
	}
	for _, user := range []string{kt.UserA, kt.UserB} {
		for _, in := range inbounds {
			got := m.waitConns(t, in, user, 5, 5*time.Second)
			counts := map[string]int{}
			var sources []string
			for _, c := range got {
				sources = append(sources, c.Peer)
				if in == kt.InVLESS && relays[user+"@"+in].Network(c.Peer) != c.Network {
					t.Errorf("%s@%s 的一条 %s 连接的对端 %s，在 Relay 那边装的是 %q", user, in, c.Network, c.Peer, relays[user+"@"+in].Network(c.Peer))
				}
				counts[c.Network]++
				wantOut := outOf[sinkOf[user][0]]
				if c.Network == "udp" {
					wantOut = outOf[sinkOf[user][1]]
				}
				if c.Outbound != wantOut {
					t.Errorf("%s@%s 的一条 %s 连接走的是 %s，应当是 %s：记错了凭据", user, in, c.Network, c.Outbound, wantOut)
				}
			}
			peers := relays[user+"@"+in].Peers()
			sort.Strings(sources)
			if in == kt.InVLESS && strings.Join(sources, " ") != strings.Join(peers, " ") {
				t.Errorf("%s@%s 的对端 %v 与 Relay 记下的 %v 对不上", user, in, sources, peers)
			}
			if in == kt.InHY2 {
				same := len(peers) == 1
				for _, src := range sources {
					same = same && src == peers[0]
				}
				if !same {
					t.Errorf("%s@%s 的对端 %v 应当都是 Relay 记下的唯一地址 %v", user, in, sources, peers)
				}
			}
			t.Logf("结果（%s）识别连接 %s@%s：TCP %d 条、UDP %d 条", prof.name, user, in, counts["tcp"], counts["udp"])
			if counts["tcp"] != 3 || counts["udp"] != 2 {
				t.Errorf("%s@%s 应当是 3 条 TCP、2 条 UDP：%+v", user, in, got)
			}
		}
	}
}

// 第 3 项：验收里的按凭据踢线（node-acceptance「验收里的按凭据踢线」）。
func TestAcceptanceKick(t *testing.T) {
	prof := currentProfile()
	ensureSinks(t)
	m := newMasterClient(t)
	settle(t, m)
	t.Cleanup(func() { settle(t, m) })

	cl := &clients{t: t}
	long := 5 * time.Minute
	type pair struct{ tcp, udp *kt.Flow }
	start := func(user, in string) pair {
		c := cl.get(user, in)
		return pair{kt.Download(t, c, 1, mbps(8), 0), kt.UDPDownload(t, c, 2, mbps(8), long)}
	}
	aV, aH, bV := start(kt.UserA, kt.InVLESS), start(kt.UserA, kt.InHY2), start(kt.UserB, kt.InVLESS)
	defer stopAll(aV.tcp, aV.udp, aH.tcp, aH.udp, bV.tcp, bV.udp)
	time.Sleep(prof.warmup)
	flowing := func(f *kt.Flow) bool {
		before := f.Meter.Bytes()
		time.Sleep(time.Second)
		return f.Meter.Bytes() > before
	}
	stopped := func(f *kt.Flow) bool { return !flowing(f) }
	for name, f := range map[string]*kt.Flow{"A@VLESS TCP": aV.tcp, "A@VLESS UDP": aV.udp, "A@Hysteria2 TCP": aH.tcp, "A@Hysteria2 UDP": aH.udp, "B TCP": bV.tcp, "B UDP": bV.udp} {
		if !flowing(f) {
			t.Fatalf("踢线之前 %s 应当在跑", name)
		}
	}

	t0 := time.Now()
	n := m.kick(t, kt.UserA, kt.InVLESS)
	select {
	case <-aV.tcp.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("A@VLESS 的 TCP 3 秒内没断")
	}
	tcpGone := time.Since(t0)
	time.Sleep(3*time.Second - time.Since(t0))
	if !stopped(aV.udp) {
		t.Error("A@VLESS 的 UDP 3 秒后还在收数据")
	}
	t.Logf("结果（%s）踢 A@VLESS：断开 %d 条，TCP %v 内断开，UDP 3 秒后不再收到数据", prof.name, n, tcpGone.Round(time.Microsecond))
	for name, f := range map[string]*kt.Flow{"A@Hysteria2 TCP": aH.tcp, "A@Hysteria2 UDP": aH.udp, "B TCP": bV.tcp, "B UDP": bV.udp} {
		if !flowing(f) {
			t.Errorf("%s 不该受影响", name)
		}
	}
	kt.Echo(t, cl.get(kt.UserA, kt.InVLESS), 1)

	n = m.kick(t, kt.UserA, "")
	time.Sleep(3 * time.Second)
	if !stopped(aH.tcp) || !stopped(aH.udp) {
		t.Error("踢 A 的全部凭据后，A@Hysteria2 应当断开")
	}
	if !flowing(bV.tcp) || !flowing(bV.udp) {
		t.Error("踢 A 的全部凭据不该影响 B")
	}
	t.Logf("结果（%s）踢 A 的全部凭据：断开 %d 条，B 不受影响", prof.name, n)
}

type countKey struct {
	Inbound, Outbound, User string
}

type countVal struct {
	up, down int64
}

func trafficMap(v trafficView) map[countKey]countVal {
	out := make(map[countKey]countVal, len(v.Traffic))
	for _, item := range v.Traffic {
		out[countKey{Inbound: item.Inbound, Outbound: item.Outbound, User: item.Credential}] = countVal{up: item.Uplink, down: item.Downlink}
	}
	return out
}

func within1(got, want int64) bool {
	if want == 0 {
		return got == 0
	}
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return float64(diff) <= 0.01*float64(want)
}

// 第 4 项：验收里的三维流量计数（node-acceptance「验收里的三维流量计数」）。
func TestAcceptanceCounting(t *testing.T) {
	prof := currentProfile()
	s1, s2 := ensureSinks(t)
	m := newMasterClient(t)
	settle(t, m)
	t.Cleanup(func() { settle(t, m) })

	before := trafficMap(m.waitSnapshot(t, time.Now()))
	cl := &clients{t: t}
	mb := prof.mib
	header := func(f string, args ...any) int64 { return int64(len(fmt.Sprintf(f, args...))) }
	expect := map[countKey]countVal{
		{Inbound: kt.InVLESS, Outbound: kt.Out1, User: kt.UserA}: {header("D 0 %d\n", 100*mb), 100 * mb},
		{Inbound: kt.InVLESS, Outbound: kt.Out2, User: kt.UserA}: {header("U a-up\n") + 60*mb, 0},
		{Inbound: kt.InHY2, Outbound: kt.Out2, User: kt.UserA}:   {header("D 0 %d\n", 40*mb), 40 * mb},
		{Inbound: kt.InVLESS, Outbound: kt.Out2, User: kt.UserB}: {header("D 0 %d\n", 30*mb), 30 * mb},
		{Inbound: kt.InHY2, Outbound: kt.Out1, User: kt.UserB}:   {header("U b-up\n") + 20*mb, 0},
	}
	for _, user := range []string{kt.UserA, kt.UserB} {
		for _, in := range inbounds {
			cl.get(user, in)
		}
	}
	t0 := time.Now()
	flows := []*kt.Flow{
		kt.Download(t, cl.get(kt.UserA, kt.InVLESS), 1, 0, 100*mb),
		kt.Upload(t, s2, cl.get(kt.UserA, kt.InVLESS), 2, "a-up", 0, 60*mb),
		kt.Download(t, cl.get(kt.UserA, kt.InHY2), 2, 0, 40*mb),
		kt.Download(t, cl.get(kt.UserB, kt.InVLESS), 2, 0, 30*mb),
		kt.Upload(t, s1, cl.get(kt.UserB, kt.InHY2), 1, "b-up", 0, 20*mb),
	}
	waitDone(t, 5*time.Minute, flows...)
	elapsed := time.Since(t0)
	t.Logf("结果（%s）不限速吞吐：五条流合计 %d 字节，用时 %v，合计不低于 %.0f Mbps（容器 bridge 网络）", prof.name, 250*mb, elapsed.Round(time.Millisecond), float64(250*mb)*8/elapsed.Seconds()/1e6)
	time.Sleep(500 * time.Millisecond)
	after := trafficMap(m.waitSnapshot(t, time.Now()))

	deltas := map[countKey]countVal{}
	for k, a := range after {
		b := before[k]
		deltas[k] = countVal{up: a.up - b.up, down: a.down - b.down}
	}
	for k, w := range expect {
		g := deltas[k]
		t.Logf("结果（%s）计数 %s→%s %s：上行增量 %d（实际 %d）、下行增量 %d（实际 %d）", prof.name, k.Inbound, k.Outbound, k.User, g.up, w.up, g.down, w.down)
		if !within1(g.up, w.up) || !within1(g.down, w.down) {
			t.Errorf("%+v：增量 %+v，实际 %+v，误差超过 1%%", k, g, w)
		}
	}
	for k, g := range deltas {
		if _, ok := expect[k]; !ok && (g.up != 0 || g.down != 0) {
			t.Errorf("没跑过的组合 %+v 增量应当为零：%+v", k, g)
		}
	}
}

// 第 5 项：验收里的重启基线（node-acceptance「验收里的重启基线」）。
func TestAcceptanceRestart(t *testing.T) {
	prof := currentProfile()
	ensureSinks(t)
	m := newMasterClient(t)
	settle(t, m)
	t.Cleanup(func() { settle(t, m) })

	key := countKey{Inbound: kt.InVLESS, Outbound: kt.Out1, User: kt.UserA}
	mb := prof.mib

	for _, mode := range []string{"同进程内核重启", "杀掉整个进程"} {
		t.Run(mode, func(t *testing.T) {
			settle(t, m)
			baseSnap := m.waitSnapshot(t, time.Now())
			baseDown := trafficMap(baseSnap)[key].down
			baseBoots := baseSnap.CoreBootCount

			cl := &clients{t: t}
			waitDone(t, 5*time.Minute, kt.Download(t, cl.get(kt.UserA, kt.InVLESS), 1, 0, 100*mb))
			time.Sleep(300 * time.Millisecond)
			midSnap := m.waitSnapshot(t, time.Now())
			midDown := trafficMap(midSnap)[key].down
			if !within1(midDown-baseDown, 100*mb) {
				t.Fatalf("%s 重启前增量应当约为 %d：%d", mode, 100*mb, midDown-baseDown)
			}

			if mode == "同进程内核重启" {
				m.restartKernel(t)
				if st := m.getServerStatus(t); st.Status != "connected" {
					t.Fatalf("同进程重启内核后节点应当一直是 connected：%s", st.Status)
				}
			} else {
				m.killAndStartNode(t)
			}

			waitDone(t, 5*time.Minute, kt.Download(t, cl.get(kt.UserA, kt.InVLESS), 1, 0, 50*mb))
			time.Sleep(300 * time.Millisecond)
			endSnap := m.waitSnapshot(t, time.Now())
			endDown := trafficMap(endSnap)[key].down
			if endDown < midDown {
				t.Fatalf("%s 重启后累计倒退：重启前 %d，重启后 %d", mode, midDown, endDown)
			}

			downDelta := endDown - baseDown
			bootDelta := endSnap.CoreBootCount - baseBoots
			t.Logf("结果（%s）重启基线（%s）：累计下行增量 %d（实际 %d），core_boot_count 增量 %d", prof.name, mode, downDelta, 150*mb, bootDelta)
			if !within1(downDelta, 150*mb) || bootDelta != 1 {
				t.Errorf("累计增量应当是 %d、core_boot_count 恰好多 1：%d，%d 次", 150*mb, downDelta, bootDelta)
			}
			if st := m.getServerStatus(t); st.Status != "connected" {
				t.Errorf("%s 结束时节点应当是 connected：%s", mode, st.Status)
			}
		})
	}
}
