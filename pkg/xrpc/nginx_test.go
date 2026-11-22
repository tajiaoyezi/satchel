package xrpc

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNginxAndServiceRPCJSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	methods := []string{
		MethodServiceStatus, MethodServiceControl,
		MethodNginxInstall, MethodNginxUninstall,
		MethodNginxServers, MethodNginxClearStream,
		MethodNginxWebsitesScan, MethodNginxWebsitesApply,
	}
	seen := map[string]bool{}
	for _, m := range methods {
		if m == "" || seen[m] {
			t.Fatalf("方法名重复或为空：%q", m)
		}
		seen[m] = true
	}

	st := ServiceStatusResult{
		Kernel:    KernelServiceStatus{Running: true, Version: "1.14.4", StartedAt: &now},
		Nginx:     NginxServiceStatus{Installed: true, Running: true, Version: "1.27.4", InDocker: true, InitSystem: "direct", ConfigOK: true},
		Agent:     AgentServiceStatus{Running: true, Version: "0.2.0", UptimeSeconds: 120, InDocker: true},
		CheckedAt: now,
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var gotSt ServiceStatusResult
	if err := json.Unmarshal(raw, &gotSt); err != nil {
		t.Fatal(err)
	}
	if !gotSt.Kernel.Running || !gotSt.Nginx.Installed || !gotSt.Nginx.InDocker || gotSt.Agent.UptimeSeconds != 120 {
		t.Fatalf("ServiceStatusResult 回环不一致：%+v", gotSt)
	}

	applyReq := NginxWebsitesApplyParams{
		Sites: []ManagedWebsiteFile{{
			Domain:      "app.example.com",
			Conf:        SatchelManagedHeader + " website_id=1 domain=app.example.com\nserver { listen 80; }\n",
			HTMLPath:    "/etc/nginx/html/app.example.com/index.html",
			HTMLContent: "<html>ok</html>",
		}},
		RemoveDomains:    []string{"old.example.com"},
		ProtectMaster443: true,
	}
	raw, err = json.Marshal(applyReq)
	if err != nil {
		t.Fatal(err)
	}
	var gotApply NginxWebsitesApplyParams
	if err := json.Unmarshal(raw, &gotApply); err != nil {
		t.Fatal(err)
	}
	if len(gotApply.Sites) != 1 || gotApply.Sites[0].Domain != "app.example.com" || !gotApply.ProtectMaster443 || len(gotApply.RemoveDomains) != 1 {
		t.Fatalf("NginxWebsitesApplyParams 回环不一致：%+v", gotApply)
	}

	clearReq := NginxClearStreamParams{Port: 8443, ProtectMaster443: true}
	raw, err = json.Marshal(clearReq)
	if err != nil {
		t.Fatal(err)
	}
	var gotClear NginxClearStreamParams
	if err := json.Unmarshal(raw, &gotClear); err != nil {
		t.Fatal(err)
	}
	if gotClear.Port != 8443 || !gotClear.ProtectMaster443 {
		t.Fatalf("NginxClearStreamParams 回环不一致：%+v", gotClear)
	}

	// 验证 steal_site / remove_steal 及回复字段 omitempty 向后兼容与完整往返。
	minimalRaw, err := json.Marshal(NginxWebsitesApplyParams{Sites: []ManagedWebsiteFile{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(minimalRaw) != `{"sites":[]}` {
		t.Fatalf("steal 可选字段零值时应省略，实际：%s", minimalRaw)
	}
	stealReq := NginxWebsitesApplyParams{
		Sites: []ManagedWebsiteFile{},
		StealSite: &ManagedWebsiteFile{
			Domain:      "decoy.example.com",
			Conf:        SatchelManagedHeader + " steal_self=true\nserver { listen 127.0.0.1:8001 ssl; }\n",
			HTMLPath:    "/etc/nginx/html/satchel-steal/index.html",
			HTMLContent: "<html>decoy</html>",
		},
	}
	raw, err = json.Marshal(stealReq)
	if err != nil {
		t.Fatal(err)
	}
	var gotSteal NginxWebsitesApplyParams
	if err := json.Unmarshal(raw, &gotSteal); err != nil {
		t.Fatal(err)
	}
	if gotSteal.StealSite == nil || gotSteal.StealSite.Domain != "decoy.example.com" || gotSteal.RemoveSteal {
		t.Fatalf("StealSite 回环不一致：%+v", gotSteal)
	}

	stealRes := NginxWebsitesApplyResult{
		Applied:       []AppliedWebsiteFile{},
		Removed:       []string{},
		Reloaded:      true,
		StealApplied:  true,
		StealConfPath: "/etc/nginx/servers/satchel-steal.conf",
	}
	raw, err = json.Marshal(stealRes)
	if err != nil {
		t.Fatal(err)
	}
	var gotRes NginxWebsitesApplyResult
	if err := json.Unmarshal(raw, &gotRes); err != nil {
		t.Fatal(err)
	}
	if !gotRes.StealApplied || gotRes.StealRemoved || gotRes.StealConfPath != "/etc/nginx/servers/satchel-steal.conf" {
		t.Fatalf("NginxWebsitesApplyResult steal 字段回环不一致：%+v", gotRes)
	}
}
