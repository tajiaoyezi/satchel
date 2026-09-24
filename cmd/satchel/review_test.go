package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/projection/mcp"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-mcp「身份只来自 HTTP 认证」走真实的 authn：TCP 上的 MCP 不带凭据没有身份，带自称的头也没用；
// 带无效令牌是无效凭据（m1-04），同样 unauthenticated。
func TestMCPOverTCPHasNoIdentity(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	h := start(t, bdb)
	for _, header := range []http.Header{
		{"X-Satchel-Actor": {"root"}},
		{"Authorization": {"Bearer abc"}, "X-Satchel-Actor": {"root"}},
	} {
		client := &http.Client{Transport: headerTransport{header: header}}
		mc := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
		cs, err := mc.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: h.tcpURL + mcp.Path, HTTPClient: client}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range []*sdk.CallToolParams{
			{Name: "satchel_run", Arguments: map[string]any{"args": []string{"whoami"}}},
			{Name: "satchel_run", Arguments: map[string]any{"args": []string{"explain", "Task"}}},
			{Name: "satchel_explain", Arguments: map[string]any{"target": "Task"}},
		} {
			res, err := cs.CallTool(context.Background(), call)
			if err != nil {
				t.Fatal(err)
			}
			text := res.Content[0].(*sdk.TextContent).Text
			var e v1.Error
			if !res.IsError || json.Unmarshal([]byte(text), &e) != nil || e.Code != v1.CodeUnauthenticated {
				t.Errorf("%v：%s %v 经 TCP 应当 unauthenticated：%v %s", header, call.Name, call.Arguments, res.IsError, text)
			}
		}
		cs.Close()
	}
	if n := h.auditCount(); n != 0 {
		t.Fatalf("无身份与无效凭据的调用都不该记审计，得到 %d 条", n)
	}
}

type headerTransport struct{ header http.Header }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	for k, v := range h.header {
		r.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(r)
}

// master-serve「TCP 与 unix socket 双监听」：同一个数据目录已有主控在跑时，第二个实例拒绝启动、不碰第一个的 socket。
func TestSecondInstanceRefused(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	h := start(t, bdb)
	second, err := newApp(h.dataDir, bdb, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = second.listen("127.0.0.1:0")
	if e := v1.AsError(err); err == nil || e.Code != v1.CodeConflict || !strings.Contains(e.Reason, "已有一个主控在运行") {
		t.Fatalf("第二个实例应当以 conflict 拒绝：%v", err)
	}
	if status, _ := get(t, h.unixClient(), "http://satchel/api/v1/healthz"); status != 200 {
		t.Fatalf("第一个实例的 socket 应当还在工作：%d", status)
	}
}

// master-serve「优雅停止」：停止时进行中的慢请求拿到完整响应，新连接不再接受。
func TestGracefulStopWaitsForSlowRequest(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	dataDir := shortTempDir(t)
	if err := db.EnsureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(dataDir, bdb, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	inner := a.handler
	started := make(chan struct{}, 1)
	a.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slow") == "1" {
			started <- struct{}{}
			time.Sleep(700 * time.Millisecond)
		}
		inner.ServeHTTP(w, r)
	})
	tcp, unix, err := a.listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.serve(ctx, tcp, unix) }()
	url := listenAddrOf(tcp) + "/api/v1/healthz"
	var wg sync.WaitGroup
	wg.Add(1)
	var slowStatus int
	go func() {
		defer wg.Done()
		resp, err := http.Get(url + "?slow=1")
		if err != nil {
			slowStatus = -1
			return
		}
		resp.Body.Close()
		slowStatus = resp.StatusCode
	}()
	<-started
	cancel()
	wg.Wait()
	if slowStatus != 200 {
		t.Fatalf("停止期间进行中的慢请求应当拿到 200，得到 %d", slowStatus)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve 应当干净退出：%v", err)
	}
	if _, err := http.Get(url); err == nil {
		t.Fatal("停止后不该再接受连接")
	}
}
