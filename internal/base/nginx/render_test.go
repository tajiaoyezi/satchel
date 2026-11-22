package nginx

import (
	"strings"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

func TestRenderWebsiteStaticProxyWSSDecoy(t *testing.T) {
	// 1. Static 默认目录与默认 index.html。
	st, err := RenderWebsite(1, v1.WebsiteSpec{
		ServerID: 1, Domain: "Static.Example.COM", Type: TypeStatic,
	}, false)
	if err != nil {
		t.Fatalf("static 渲染失败：%v", err)
	}
	if st.Domain != "static.example.com" || st.ConfPath != "/etc/nginx/servers/satchel-site-static.example.com.conf" {
		t.Fatalf("static 路径不对：%+v", st)
	}
	if !strings.HasPrefix(st.Conf, xrpc.SatchelManagedHeader) || !strings.Contains(st.Conf, "root /etc/nginx/html/static.example.com;") || st.HTMLPath == "" {
		t.Fatalf("static 配置不对：\n%s", st.Conf)
	}
	if HasPort443Listen(st.Conf) {
		t.Fatal("未开 use_443 不应监听 443")
	}

	// 2. WSS 反代模板。
	wssTarget, err := NormalizeBuildTarget(TypeProxy, "http://127.0.0.1:10000", "/ray")
	if err != nil || wssTarget != "ws://127.0.0.1:10000/ray" {
		t.Fatalf("WSS 目标归一化不对：%q %v", wssTarget, err)
	}
	wss, err := RenderWebsite(2, v1.WebsiteSpec{
		ServerID: 1, Domain: "ws.example.com", Type: TypeProxy, Target: wssTarget, Use443: true,
	}, false)
	if err != nil {
		t.Fatalf("wss 渲染失败：%v", err)
	}
	for _, want := range []string{
		xrpc.SatchelManagedHeader,
		"listen 443 ssl;",
		"location /ray {",
		"proxy_pass http://127.0.0.1:10000;",
		"proxy_set_header Upgrade $http_upgrade;",
		`proxy_set_header Connection "upgrade";`,
		"proxy_buffering off;",
	} {
		if !strings.Contains(wss.Conf, want) {
			t.Errorf("WSS 配置缺少 %q：\n%s", want, wss.Conf)
		}
	}
	if !HasPort443Listen(wss.Conf) {
		t.Fatal("开启 use_443 应检出 443 监听")
	}

	// 3. 普通 HTTP 反代。
	px, err := RenderWebsite(3, v1.WebsiteSpec{
		ServerID: 1, Domain: "api.example.com", Type: TypeProxy, Target: "http://127.0.0.1:8080",
	}, false)
	if err != nil || !strings.Contains(px.Conf, "proxy_pass http://127.0.0.1:8080;") || strings.Contains(px.Conf, "Upgrade") {
		t.Fatalf("普通反代不对：err=%v conf=\n%s", err, px.Conf)
	}

	// 4. Decoy 内置模板与上游镜像。
	dc, err := RenderWebsite(4, v1.WebsiteSpec{
		ServerID: 1, Domain: "blog.example.com", Type: TypeDecoy, Target: "blog",
	}, false)
	if err != nil || dc.HTMLPath != "/etc/nginx/html/blog.example.com/index.html" || !strings.Contains(dc.HTMLContent, "Engineering Notes") {
		t.Fatalf("decoy 模板不对：err=%v %+v", err, dc)
	}
	dcMirror, err := RenderWebsite(5, v1.WebsiteSpec{
		ServerID: 1, Domain: "mirror.example.com", Type: TypeDecoy, Target: "https://example.org",
	}, false)
	if err != nil || !strings.Contains(dcMirror.Conf, "proxy_ssl_server_name on;") || dcMirror.HTMLPath != "" {
		t.Fatalf("decoy 镜像不对：err=%v %+v", err, dcMirror)
	}

	if len(Templates()) < 7 {
		t.Fatalf("模板目录偏少：%d", len(Templates()))
	}
}

func TestValidateWebsiteGuardsAndInjectionRejection(t *testing.T) {
	// 同机 443 抢占拒绝。
	err := ValidateWebsiteSpec(v1.WebsiteSpec{
		ServerID: 1, Domain: "same.example.com", Type: TypeStatic, Use443: true,
	}, true)
	if ve, ok := err.(*v1.Error); !ok || ve.Code != v1.CodeConflict {
		t.Fatalf("同机 use_443=true 应报 conflict，得到：%v", err)
	}
	// 同机 use_443=false 放行。
	if err := ValidateWebsiteSpec(v1.WebsiteSpec{
		ServerID: 1, Domain: "same.example.com", Type: TypeStatic, Use443: false,
	}, true); err != nil {
		t.Fatalf("同机 use_443=false 应放行：%v", err)
	}

	// 注入与路径穿越拦截。
	badCases := []v1.WebsiteSpec{
		{Domain: "bad;domain.com", Type: TypeStatic},
		{Domain: "../etc.com", Type: TypeStatic},
		{Domain: "singlelabel", Type: TypeStatic},
		{Domain: "ok.example.com", Type: "unknown"},
		{Domain: "ok.example.com", Type: TypeStatic, Target: "/etc/shadow"},
		{Domain: "ok.example.com", Type: TypeStatic, Target: "/etc/nginx/html/../cert"},
		{Domain: "ok.example.com", Type: TypeProxy, Target: "http://127.0.0.1:8080; return 200;"},
		{Domain: "ok.example.com", Type: TypeProxy, Target: "http://127.0.0.1:8080\nadd_header X-Pwn 1;"},
		{Domain: "ok.example.com", Type: TypeProxy, Target: "file:///etc/passwd"},
		{Domain: "ok.example.com", Type: TypeDecoy, Target: "not-a-preset"},
	}
	for i, tc := range badCases {
		if err := ValidateWebsiteSpec(tc, false); err == nil {
			t.Errorf("用例 %d 应当拒绝：%+v", i, tc)
		}
	}
}
