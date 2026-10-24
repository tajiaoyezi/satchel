package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// hostRewriter 模拟同机反代：连接仍进到回环，Host 换成主控域名。
type hostRewriter struct {
	host string
	next http.RoundTripper
}

func (h hostRewriter) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = h.host
	return h.next.RoundTrip(r)
}

// master-mcp「服务与两个工具」：经同机反代（进到回环、Host 是主控域名）的请求与直连一样处理，MCP 层不另按 Host 拒绝。
func TestBehindLoopbackProxy(t *testing.T) {
	srv := httptest.NewServer(identity(admin(), NewHandler(testOptions(t))))
	t.Cleanup(srv.Close)
	httpClient := &http.Client{Transport: hostRewriter{host: "panel.example.com", next: srv.Client().Transport}}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: srv.URL + Path, HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatalf("经反代的 initialize 应当成功：%v", err)
	}
	t.Cleanup(func() { cs.Close() })
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("经反代的 tools/list 应当成功：%v", err)
	}
	if len(tools.Tools) != 2 {
		t.Fatalf("工具应当恰好两个：%d", len(tools.Tools))
	}
	s := &session{t: t, cs: cs}
	if text, isErr := s.run([]string{"whoami"}, ""); isErr {
		t.Fatalf("经反代跑命令应当成功：%s", text)
	}
}
