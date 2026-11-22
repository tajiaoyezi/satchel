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
}
