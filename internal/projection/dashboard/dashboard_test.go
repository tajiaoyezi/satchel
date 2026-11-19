package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	svcservers "github.com/satchel/satchel/internal/service/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func withAuth(id v1.Identity, src v1.CredentialSource, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := v1.WithIdentity(r.Context(), id)
		ctx = v1.WithCredentialSource(ctx, src)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func TestDashboardWSAuthCSWSHAndLazyTicker(t *testing.T) {
	var snapCalls atomic.Int32
	hub := New(Config{
		RefreshInterval: time.Second,
		Snapshot: func(context.Context) ([]svcservers.DashboardServer, svcservers.DashboardSummary, error) {
			snapCalls.Add(1)
			return []svcservers.DashboardServer{
					{ID: 1, Name: "hk-1", Status: "connected", CurrentUploadSpeed: 1024, CurrentDownloadSpeed: 2048},
				}, svcservers.DashboardSummary{
					TotalServers: 1, ConnectedServers: 1, TotalUploadSpeed: 1024, TotalDownloadSpeed: 2048,
				}, nil
		},
	})
	defer hub.Close(context.Background())

	if hub.TickerRunning() {
		t.Fatal("无客户端时惰性 ticker 不应当启动")
	}

	// 1. 拒绝 ?token= 查询参数、未登录 401、普通用户 403、跨站 Cookie 403。
	adminID := v1.Identity{Actor: "alice", ActorKind: v1.ActorUser, Role: v1.RoleAdmin}
	userID := v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, Path+"?token=sat_leak", nil)
	withAuth(adminID, v1.SourceSession, hub).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("带 ?token= 应当 400，得到 %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, Path, nil)
	withAuth(v1.Anonymous(), v1.SourceNone, hub).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应当 401，得到 %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, Path, nil)
	withAuth(userID, v1.SourceSession, hub).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户应当 403，得到 %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://panel.example.com"+Path, nil)
	req.Header.Set("Origin", "https://evil.example.com")
	withAuth(adminID, v1.SourceSession, hub).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站 Origin Cookie 应当 403，得到 %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://panel.example.com"+Path, nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	withAuth(adminID, v1.SourceSession, hub).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站 Sec-Fetch-Site Cookie 应当 403，得到 %d %s", rec.Code, rec.Body.String())
	}

	// 2. 同源管理员建连立即收到首帧、惰性 ticker 启动，断开后停止。
	srv := httptest.NewServer(withAuth(adminID, v1.SourceSession, hub))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + Path
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{"Origin": {srv.URL}}
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("同源管理员建连应当成功：%v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读首帧失败：%v", err)
	}
	if strings.Contains(string(data), "agent_token") || strings.Contains(string(data), "pull_token") {
		t.Fatalf("广播帧不应包含任何密钥字段：%s", data)
	}
	var frame Frame
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("解析首帧失败：%v", err)
	}
	if frame.Type != "realtime" || frame.RefreshIntervalMS != 1000 || len(frame.Servers) != 1 || frame.Summary.TotalUploadSpeed != 1024 {
		t.Fatalf("首帧内容不对：%+v", frame)
	}
	if !hub.TickerRunning() || hub.ClientCount() != 1 {
		t.Fatalf("在线时应当有 1 个客户端且 ticker 运行中：count=%d running=%v", hub.ClientCount(), hub.TickerRunning())
	}

	// 3. 热更新刷新间隔不会断开客户端，并在下一帧体现新 refresh_interval_ms。
	hub.SetRefreshInterval(1200 * time.Millisecond)
	_, data2, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读周期广播帧失败：%v", err)
	}
	var frame2 Frame
	if err := json.Unmarshal(data2, &frame2); err != nil || frame2.RefreshIntervalMS != 1200 {
		t.Fatalf("热更新后帧间隔应当为 1200ms：err=%v frame=%+v", err, frame2)
	}

	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.ClientCount() == 0 && !hub.TickerRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hub.ClientCount() != 0 || hub.TickerRunning() {
		t.Fatalf("全部断开后惰性 ticker 应当停止：count=%d running=%v", hub.ClientCount(), hub.TickerRunning())
	}
}
