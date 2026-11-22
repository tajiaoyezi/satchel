package nginx

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

func TestRenderStealSiteDecoyStaticProxyAndWSSCoexist(t *testing.T) {
	wsLocs := ExtractWSSLocations([]v1.InboundSpec{
		{Tag: "ws-b", Port: 20002, Listen: "127.0.0.1", Enabled: true, Transport: json.RawMessage(`{"type":"ws","path":"/ray"}`)},
		{Tag: "ws-off", Port: 20003, Listen: "127.0.0.1", Enabled: false, Transport: json.RawMessage(`{"type":"ws","path":"/off"}`)},
		{Tag: "tcp-1", Port: 20004, Listen: "", Enabled: true, Transport: json.RawMessage(`{}`)},
		{Tag: "ws-a", Port: 20001, Listen: "", Enabled: true, Transport: json.RawMessage(`{"type":"ws"}`)},
	})
	if len(wsLocs) != 2 || wsLocs[0].Path != "/ray" || wsLocs[1].Path != "/ws" || wsLocs[1].Listen != "127.0.0.1" {
		t.Fatalf("ExtractWSSLocations 结果异常：%+v", wsLocs)
	}

	// 1. 默认 decoy + WSS 合并 + IPv6 开启
	r, err := RenderStealSite(StealRenderInput{
		ServerID:     7,
		Domain:       "Decoy.Example.COM",
		StealMode:    "tunnel",
		Ipv6Enabled:  true,
		WSSLocations: wsLocs,
	})
	if err != nil {
		t.Fatalf("RenderStealSite 失败：%v", err)
	}
	if r.ConfPath != "/etc/nginx/servers/satchel-steal.conf" || r.Domain != "decoy.example.com" {
		t.Fatalf("路径或域名异常：%+v", r)
	}
	if !strings.HasPrefix(r.Conf, xrpc.SatchelManagedHeader+" steal_self=true server_id=7 domain=decoy.example.com mode=tunnel") {
		t.Fatalf("缺少 satchel-managed 首行：\n%s", r.Conf)
	}
	if strings.Contains(r.Conf, "proxy_protocol") {
		t.Fatalf("严禁生成 proxy_protocol：\n%s", r.Conf)
	}
	if HasPort443Listen(r.Conf) {
		t.Fatalf("偷自己配置严禁直接监听 443：\n%s", r.Conf)
	}
	for _, want := range []string{
		"listen 127.0.0.1:8001 ssl default_server;",
		"ssl_reject_handshake on;",
		"listen 127.0.0.1:8001 ssl;",
		"http2 on;",
		"ssl_certificate /etc/nginx/cert/decoy.example.com.crt;",
		"location /ray {",
		"proxy_pass http://127.0.0.1:20002;",
		"location /ws {",
		"proxy_pass http://127.0.0.1:20001;",
		"listen [::]:80;",
		"return 301 https://$host$request_uri;",
	} {
		if !strings.Contains(r.Conf, want) {
			t.Errorf("配置缺少 %q：\n%s", want, r.Conf)
		}
	}
	if r.HTMLPath != "/etc/nginx/html/decoy.example.com/index.html" || !strings.Contains(r.HTMLContent, "Service Portal") {
		t.Fatalf("默认 decoy HTML 异常：%+v", r)
	}

	// 2. IPv6 关闭 + proxy 类型
	px, err := RenderStealSite(StealRenderInput{
		ServerID:    8,
		Domain:      "app.example.com",
		StealMode:   "fallback",
		SiteType:    TypeProxy,
		SiteValue:   "http://127.0.0.1:8080",
		Ipv6Enabled: false,
	})
	if err != nil {
		t.Fatalf("proxy steal 渲染失败：%v", err)
	}
	if strings.Contains(px.Conf, "listen [::]:80;") || !strings.Contains(px.Conf, "proxy_pass http://127.0.0.1:8080;") {
		t.Fatalf("IPv6=false 或 proxy 渲染异常：\n%s", px.Conf)
	}

	// 3. static 类型
	st, err := RenderStealSite(StealRenderInput{
		ServerID:  9,
		Domain:    "static.example.com",
		StealMode: "tunnel",
		SiteType:  TypeStatic,
		SiteValue: "/var/www/site",
	})
	if err != nil || !strings.Contains(st.Conf, "root /var/www/site;") || st.HTMLPath != "" {
		t.Fatalf("static steal 渲染异常：err=%v %+v", err, st)
	}
}

func TestRenderStealSiteGuards(t *testing.T) {
	// 同机 443 禁止
	_, err := RenderStealSite(StealRenderInput{
		ServerID: 1, Domain: "same.example.com", SameHost: true,
	})
	if err == nil || v1.AsError(err).Code != v1.CodeConflict {
		t.Fatalf("同机应报 conflict，实际：%v", err)
	}
	// 非法输入拦截
	bad := []StealRenderInput{
		{Domain: "", StealMode: "tunnel"},
		{Domain: "a.example.com", StealMode: "invalid"},
		{Domain: "a.example.com", SiteType: TypeProxy, SiteValue: "http://127.0.0.1:8080; return 200;"},
		{Domain: "a.example.com", SiteType: TypeStatic, SiteValue: "/etc/passwd"},
		{Domain: "a.example.com", WSSLocations: []WSSLocation{{Path: "/ws; evil", Port: 10000}}},
		{Domain: "a.example.com", CertPath: "/etc/nginx/cert/../../etc/shadow"},
	}
	for i, tc := range bad {
		if _, err := RenderStealSite(tc); err == nil {
			t.Errorf("用例 %d 应报错：%+v", i, tc)
		}
	}

	// IsSameHost443Protected 判定覆盖
	if !IsSameHost443Protected(true, "connected", "other.example.com", "https://master.example.com") {
		t.Error("same_host_as_master=true 必须受保护")
	}
	if !IsSameHost443Protected(false, "pending", "Master.Example.COM", "https://master.example.com:8443") {
		t.Error("pending 且非回环域名撞 https master_url 必须受保护")
	}
	if IsSameHost443Protected(false, "connected", "master.example.com", "https://master.example.com") {
		t.Error("已连接且 same_host_as_master=false 不应误判同机")
	}
	if IsSameHost443Protected(false, "pending", "127.0.0.1", "https://127.0.0.1:8443") {
		t.Error("回环地址不应触发同机域名碰撞")
	}
	if IsSameHost443Protected(false, "pending", "master.example.com", "http://master.example.com") {
		t.Error("非 https master_url 不应触发同机域名碰撞")
	}
}
