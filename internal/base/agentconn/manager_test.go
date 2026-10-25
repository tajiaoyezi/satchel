package agentconn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/satchel/satchel/pkg/xrpc"
)

// fakeDir 是假的服务器仓储：一台服务器（id 1，token tok），写入照 core/servers 的规则（离线只动 connected 的）；
// 两个钩子能在写入生效之前卡住它，用来复现交错；onlines、offlines 数已经生效的写入，测试靠它等被卡住的写入落地。
// closed 模拟关库：之后开始的写入都失败，已经开始的照样做完（与 sql.DB.Close 一样）。
type fakeDir struct {
	mu       sync.Mutex
	status   string
	ip       string
	boot     time.Time
	onlines  int
	offlines int
	closed   bool
	online   func(ctx context.Context, ip string, boot time.Time) error // 每次 Online 生效之前调用（在锁外）；返回错误时写入不生效
	offline  func()                                                     // 每次 Offline 生效之前调用（在锁外）
}

var errDBClosed = errors.New("数据库已关闭")

func (d *fakeDir) Lookup(_ context.Context, token string) (Node, bool, error) {
	if token == "tok2" { // 第二台，只有 TestConnectedIDs 用；在线状态只记第一台的
		return Node{ID: 2, Name: "hk-2"}, true, nil
	}
	return Node{ID: 1, Name: "hk-1"}, token == "tok", nil
}

func (d *fakeDir) Online(ctx context.Context, id int64, ip string, _ bool, boot, _ time.Time) error {
	if id != 1 {
		return nil
	}
	if d.isClosed() {
		return errDBClosed
	}
	if d.online != nil {
		if err := d.online(ctx, ip, boot); err != nil {
			return err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status, d.ip = "connected", ip
	if !boot.IsZero() {
		d.boot = boot
	}
	d.onlines++
	return nil
}

func (d *fakeDir) Offline(_ context.Context, id int64, _ time.Time) error {
	if id != 1 {
		return nil
	}
	if d.isClosed() {
		return errDBClosed
	}
	if d.offline != nil {
		d.offline()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.status == "connected" {
		d.status = "offline"
	}
	d.offlines++
	return nil
}

func (d *fakeDir) state() (status, ip string, boot time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status, d.ip, d.boot
}

func (d *fakeDir) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
}

func (d *fakeDir) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

func (d *fakeDir) writes() (onlines, offlines int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.onlines, d.offlines
}

// serveManager 起一个只挂节点通道的 httptest 服务；来源 IP 由请求头 X-Test-IP 给出。onAccept 拿到服务端这一侧的连接。
func serveManager(t *testing.T, cfg Config, onAccept ...func(*websocket.Conn)) (*Manager, string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg.Identity = priv
	m := New(cfg)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		for _, f := range onAccept {
			f(ws)
		}
		m.Serve(r.Context(), ws, r.Header.Get("X-Test-IP"))
	}))
	t.Cleanup(srv.Close)
	return m, "ws" + strings.TrimPrefix(srv.URL, "http"), pub
}

type testAgent struct {
	ws   *websocket.Conn
	conn *xrpc.Conn
}

// connect 以 token tok 握手并认证，返回认证结果；连接在认证前后被关掉时 err 非空。
func connect(t *testing.T, url, ip string, pub ed25519.PublicKey) (*testAgent, xrpc.AuthResult, error) {
	t.Helper()
	return connectAs(t, url, ip, pub, "tok")
}

func connectAs(t *testing.T, url, ip string, pub ed25519.PublicKey, token string) (*testAgent, xrpc.AuthResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-IP": {ip}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	a := &testAgent{ws: ws, conn: xrpc.NewConn(ws)}
	if err := xrpc.ClientHandshake(ctx, a.conn, pub); err != nil {
		return a, xrpc.AuthResult{}, err
	}
	if err := a.conn.Write(ctx, xrpc.TypeAuth, xrpc.Auth{Token: token, AgentVersion: "1.0.0", ControlProtocol: xrpc.ControlProtocol}); err != nil {
		return a, xrpc.AuthResult{}, err
	}
	m, err := a.conn.Read(ctx)
	if err != nil {
		return a, xrpc.AuthResult{}, err
	}
	var res xrpc.AuthResult
	return a, res, xrpc.Decode(m, &res)
}

func mustConnect(t *testing.T, url, ip string, pub ed25519.PublicKey) *testAgent {
	t.Helper()
	a, res, err := connect(t, url, ip, pub)
	if err != nil || !res.OK {
		t.Fatalf("应当认证成功：%+v %v", res, err)
	}
	return a
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 旧连接的离线写入被卡住时新连接认证：新连接要等旧连接写完离线才写上线，最后是 connected，连接表里有它。
func TestStaleOfflineDoesNotOverrideNewConnection(t *testing.T) {
	dir := &fakeDir{}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	dir.offline = func() {
		once.Do(func() { close(started); <-release })
	}
	m, url, pub := serveManager(t, Config{Directory: dir})
	a := mustConnect(t, url, "10.0.0.1", pub)
	a.ws.Close(websocket.StatusNormalClosure, "bye")
	<-started // 旧连接的离线写入卡在这里
	done := make(chan error, 1)
	go func() {
		_, res, err := connect(t, url, "10.0.0.2", pub)
		if err == nil && !res.OK {
			err = fmt.Errorf("认证失败：%+v", res)
		}
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // 让新连接走到写上线那一步
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitFor(t, "旧连接的离线写入落地", func() bool { _, n := dir.writes(); return n >= 1 })
	if status, ip, _ := dir.state(); status != "connected" || ip != "10.0.0.2" || !m.Connected(1) {
		t.Fatalf("新连接之后应当是 connected、IP 是新连接的：%s %s %v", status, ip, m.Connected(1))
	}
}

// 旧连接的心跳写入被卡住时被新连接取代：旧心跳先写完，新连接的上线与心跳后写，最后 IP 与启动时间都是新连接的。
func TestStaleHeartbeatDoesNotOverrideNewConnection(t *testing.T) {
	dir := &fakeDir{}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	dir.online = func(_ context.Context, ip string, boot time.Time) error {
		if ip == "10.0.0.1" && !boot.IsZero() {
			once.Do(func() { close(started); <-release })
		}
		return nil
	}
	m, url, pub := serveManager(t, Config{Directory: dir})
	a := mustConnect(t, url, "10.0.0.1", pub)
	oldBoot := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	if err := a.conn.Write(context.Background(), xrpc.TypeHeartbeat, xrpc.Heartbeat{BootTime: oldBoot}); err != nil {
		t.Fatal(err)
	}
	<-started // 旧连接的心跳写入卡在这里
	done := make(chan *testAgent, 1)
	go func() {
		b, res, err := connect(t, url, "10.0.0.2", pub)
		if err != nil || !res.OK {
			done <- nil
			return
		}
		done <- b
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)
	b := <-done
	if b == nil {
		t.Fatal("新连接应当认证成功")
	}
	// 三次上线写入：旧连接认证、旧心跳、新连接认证。等旧心跳落地后再看，IP 必须已经是新连接的。
	waitFor(t, "旧心跳的写入落地", func() bool { n, _ := dir.writes(); return n >= 3 })
	if _, ip, _ := dir.state(); ip != "10.0.0.2" {
		t.Fatalf("旧心跳不能盖掉新连接的上线：%s", ip)
	}
	newBoot := time.Date(2026, 10, 24, 2, 0, 0, 0, time.UTC)
	if err := b.conn.Write(context.Background(), xrpc.TypeHeartbeat, xrpc.Heartbeat{BootTime: newBoot}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "新连接的心跳写进去", func() bool { _, _, boot := dir.state(); return boot.Equal(newBoot) })
	if status, ip, _ := dir.state(); status != "connected" || ip != "10.0.0.2" || !m.Connected(1) {
		t.Fatalf("最后应当是新连接的状态：%s %s", status, ip)
	}
}

// 写了上线之后认证回复写不出去：要走断线的收尾（标离线、摘掉），不能留下「connected 但连接表里没有」。
// 在写上线的那一刻把服务端这一侧的连接断掉，随后的回复必定写不出去。
func TestAuthReplyFailureCleansUp(t *testing.T) {
	dir := &fakeDir{}
	var server atomic.Pointer[websocket.Conn]
	var once sync.Once
	dir.online = func(context.Context, string, time.Time) error {
		once.Do(func() { server.Load().CloseNow() })
		return nil
	}
	m, url, pub := serveManager(t, Config{Directory: dir}, func(ws *websocket.Conn) { server.Store(ws) })
	if _, res, err := connect(t, url, "10.0.0.1", pub); err == nil && res.OK {
		t.Fatal("回复写不出去时不应当认证成功")
	}
	waitFor(t, "服务端收尾标离线", func() bool { status, _, _ := dir.state(); return status == "offline" })
	if m.Connected(1) {
		t.Fatal("连接表里不应当有它")
	}
}

// 等服务器锁时认证期限照样生效：旧连接的离线写入卡住、一直占着锁，新连接认证完排队等锁，期限到了就收到
// unavailable 并被断开，不一直挂着。连着来 10 次，每次都要收到 unavailable（websocket 库在 ctx 过期后写帧时
// 随机地有时仍写得出去，只试一次守不住「回复用过期 ctx」这种回归）。
func TestAuthDeadlineCoversServerLock(t *testing.T) {
	dir := &fakeDir{}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	dir.offline = func() {
		once.Do(func() { close(started); <-release })
	}
	m, url, pub := serveManager(t, Config{Directory: dir, AuthTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { close(release) })
	a := mustConnect(t, url, "10.0.0.1", pub)
	a.ws.Close(websocket.StatusNormalClosure, "bye")
	<-started // 旧连接的离线写入卡在这里，锁一直被占着
	for i := 0; i < 10; i++ {
		began := time.Now()
		if _, res, err := connect(t, url, "10.0.0.2", pub); err != nil || res.OK || res.Reason != xrpc.AuthUnavailable {
			t.Fatalf("第 %d 次：等不到锁时应当收到 unavailable：%+v %v", i+1, res, err)
		}
		if d := time.Since(began); d > time.Second {
			t.Fatalf("第 %d 次：认证期限是 200 毫秒，新连接应当很快被断开，用了 %v", i+1, d)
		}
	}
	if m.Connected(1) {
		t.Fatal("连接表里不应当有新连接")
	}
}

// Shutdown 也断开还没认证的连接：握完手、还没发认证的，与连上了还没开始握手的，什么都不发也会被主控断开；
// Shutdown 之后来的新连接直接断开。
func TestShutdownCoversPendingConnections(t *testing.T) {
	dir := &fakeDir{}
	m, url, pub := serveManager(t, Config{Directory: dir})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func() *websocket.Conn {
		ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-IP": {"10.0.0.1"}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ws.CloseNow() })
		return ws
	}
	shook := dial()
	if err := xrpc.ClientHandshake(ctx, xrpc.NewConn(shook), pub); err != nil {
		t.Fatal(err)
	}
	fresh := dial() // 不握手
	sctx, scancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer scancel()
	m.Shutdown(sctx)
	// 只读、不发：读应当因为对端断开而返回，而不是等到超时。
	for name, ws := range map[string]*websocket.Conn{"握完手的": shook, "没握手的": fresh} {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
		_, _, err := ws.Read(rctx)
		closedByPeer := err != nil && rctx.Err() == nil
		rcancel()
		if !closedByPeer {
			t.Fatalf("Shutdown 应当断开%s连接：%v", name, err)
		}
	}
	if _, res, err := connect(t, url, "10.0.0.2", pub); err == nil && res.OK {
		t.Fatal("Shutdown 之后来的新连接不应当认证成功")
	}
	if status, _, _ := dir.state(); status == "connected" || m.Connected(1) {
		t.Fatalf("Shutdown 之后不应当有在线的连接：%s %v", status, m.Connected(1))
	}
}

// Shutdown 取消进行中的上线写入：认证连接的上线写入卡住时开始停止，写入被取消、连接不登记。随后照 serve 的顺序关库，
// 再放开写入（没被取消的写入这时才落地，关库后的离线清理会失败），服务器也不会留下 connected。
// 这个测试守的是「停止时取消写入」；activate 里登记前的复查与撤销离线防的是写入恰好在取消之前完成，这里守不到。
func TestShutdownCancelsActivation(t *testing.T) {
	dir := &fakeDir{}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	dir.online = func(ctx context.Context, _ string, _ time.Time) error {
		once.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			return ctx.Err() // 像数据库一样：ctx 取消时写入不生效
		case <-release:
			return nil
		}
	}
	m, url, pub := serveManager(t, Config{Directory: dir})
	go func() { _, _, _ = connect(t, url, "10.0.0.1", pub) }()
	<-started
	sctx, scancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer scancel()
	m.Shutdown(sctx)
	dir.close()
	close(release)
	finished := make(chan struct{})
	go func() { m.active.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("连接的收尾没有在 5 秒内结束")
	}
	if status, _, _ := dir.state(); status == "connected" || m.Connected(1) {
		t.Fatalf("Shutdown 之后不应当留下 connected：%s %v", status, m.Connected(1))
	}
}

// 下发给节点的心跳间隔最多是离线阈值的三分之一：阈值 30 秒时设置的 60 秒下发成 10 秒，设置的 5 秒照原样下发。
func TestHeartbeatIntervalCappedByOfflineThreshold(t *testing.T) {
	for setting, want := range map[time.Duration]int{60 * time.Second: 10, 5 * time.Second: 5} {
		interval := setting
		_, url, pub := serveManager(t, Config{Directory: &fakeDir{}, OfflineThreshold: 30 * time.Second,
			HeartbeatInterval: func(context.Context) time.Duration { return interval }})
		if _, res, err := connect(t, url, "10.0.0.1", pub); err != nil || !res.OK || res.HeartbeatInterval != want {
			t.Fatalf("设置 %v 时应当下发 %d 秒：%+v %v", setting, want, res, err)
		}
	}
}

// blockOn 是一个日志 handler：记到指定的那条日志时卡住，直到 release 关闭。
type blockOn struct {
	msg     string
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *blockOn) Enabled(context.Context, slog.Level) bool { return true }
func (h *blockOn) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *blockOn) WithGroup(string) slog.Handler            { return h }
func (h *blockOn) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.once.Do(func() { close(h.reached); <-h.release })
	}
	return nil
}

// 先回认证成功、再登记：新连接取代旧连接时，登记那一刻会记一条「取代」的日志，测试让它卡住——这时新节点必须已经
// 收到认证结果（登记之后 Call 才看得到这条连接，请求不会排到认证结果前面）。
func TestAuthResultBeforeRegistration(t *testing.T) {
	h := &blockOn{msg: "同一台服务器的新连接取代旧连接", reached: make(chan struct{}), release: make(chan struct{})}
	m, url, pub := serveManager(t, Config{Directory: &fakeDir{}, Logger: slog.New(h)})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(h.release) }) }
	t.Cleanup(release)
	mustConnect(t, url, "10.0.0.1", pub)
	done := make(chan error, 1)
	go func() {
		_, res, err := connect(t, url, "10.0.0.2", pub)
		if err == nil && !res.OK {
			err = fmt.Errorf("认证失败：%+v", res)
		}
		done <- err
	}()
	<-h.reached // 正在登记新连接
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("登记新连接时，新节点应当已经收到认证结果")
	}
	release()
	waitFor(t, "新连接登记好", func() bool { return m.Connected(1) })
}

// ping 没有回应就断开：节点认证后不再读数据（不回 pong），主控在 ping 超时之后断开连接、把服务器标为离线。
func TestPingTimeoutClosesConnection(t *testing.T) {
	dir := &fakeDir{}
	m, url, pub := serveManager(t, Config{Directory: dir, PingInterval: 50 * time.Millisecond, PingTimeout: 50 * time.Millisecond})
	mustConnect(t, url, "10.0.0.1", pub) // 之后不再读
	waitFor(t, "ping 超时后标离线", func() bool {
		status, _, _ := dir.state()
		return status == "offline" && !m.Connected(1)
	})
}

// ConnectedIDs 只列出已登记的连接：两台连上后列出两台，一台断开后只剩另一台。
func TestConnectedIDs(t *testing.T) {
	m, url, pub := serveManager(t, Config{Directory: &fakeDir{}})
	mustConnect(t, url, "10.0.0.1", pub)
	b, res, err := connectAs(t, url, "10.0.0.2", pub, "tok2")
	if err != nil || !res.OK {
		t.Fatalf("第二台应当认证成功：%+v %v", res, err)
	}
	if ids := m.ConnectedIDs(); !slices.Equal(ids, []int64{1, 2}) {
		t.Fatalf("应当列出两台：%v", ids)
	}
	b.ws.Close(websocket.StatusNormalClosure, "bye")
	waitFor(t, "断开的那台不再列出", func() bool { return slices.Equal(m.ConnectedIDs(), []int64{1}) })
}

// 认证失败记录会过期：一万个来源各失败一次后过了 24 小时，再来一个来源时旧记录全部清掉。
func TestFailureRecordsExpire(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	m := New(Config{Now: func() time.Time { return now }})
	for i := 0; i < 10000; i++ {
		m.recordFailure(fmt.Sprintf("2001:db8::%x", i))
	}
	now = now.Add(24 * time.Hour)
	m.recordFailure("2001:db8:1::1")
	m.mu.Lock()
	n := len(m.fails)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("过期的失败记录应当清掉，只剩新来源的一条：%d", n)
	}
	if m.Blocked("2001:db8:1::1") {
		t.Fatal("失败一次不应当被封锁")
	}
}

// Shutdown 遵守期限：节点不读数据时，关闭握手等不到回应，20 毫秒的期限到了就断开返回。
func TestShutdownHonorsDeadline(t *testing.T) {
	dir := &fakeDir{}
	m, url, pub := serveManager(t, Config{Directory: dir})
	for i := 0; i < 3; i++ {
		mustConnect(t, url, fmt.Sprintf("10.0.0.%d", i+1), pub) // 三条都不再读
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	began := time.Now()
	m.Shutdown(ctx)
	if d := time.Since(began); d > time.Second {
		t.Fatalf("Shutdown 应当在期限到了之后很快返回，用了 %v", d)
	}
}
