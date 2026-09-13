package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

// storage-schema「Settings key catalog」与 resource-model 的主控设置类 scenario：
// 键值表的 key 合进 SystemSettings 的 kind 清单，按分档进 spec / status 与拒收清单。
func TestSystemSettingsKeysInCatalog(t *testing.T) {
	info, _ := Lookup("SystemSettings")
	for _, f := range []string{"branding_site_title", "probe_disguise_enabled", "dashboard_refresh_interval_ms", "heartbeat_interval"} {
		if !contains(info.SpecFields, f) {
			t.Errorf("SystemSettings 的 spec 应当含 %s", f)
		}
	}
	want := map[string]string{
		"master_url": "human", "subscription_url": "human", "tgbot_token": "human", "turnstile_secret_key": "human", "tgbot_admin_ids": "human",
		"update_cdn_enabled": "master_self", "tgbot_url": "master_self",
		"require_encryption":            "readonly",
		"master_https_recovery_pending": "status", "primary_admin_username": "status", "master_force_public_http": "status",
	}
	for f, class := range want {
		if contains(info.SpecFields, f) || !contains(info.StatusFields, f) {
			t.Errorf("%s 应当在 status 字段集合里而不在 spec 里", f)
		}
		if got, _ := info.RejectReason(f); got != class {
			t.Errorf("%s 应当按 %s 拒收，得到 %q", f, class, got)
		}
	}
	// 列 42 + 日常运维 key 58 = 100；列 4 + 其余 key 34 = 38。
	if len(info.SpecFields) != 100 || len(info.StatusFields) != 38 {
		t.Errorf("SystemSettings 应当有 100 个 spec 字段、38 个 status 字段，得到 %d / %d", len(info.SpecFields), len(info.StatusFields))
	}

	// 四个非 spec 档各拒一次：人类专属、主控自身类、只读、运行态。
	var spec SystemSettingsSpec
	for input, label := range map[string]string{
		`{"master_url":"https://x"}`: "人类专属", `{"update_cdn_enabled":true}`: "主控自身类",
		`{"require_encryption":false}`: "只读", `{"master_https_recovery_pending":true}`: "status",
	} {
		e := codeOf(t, DecodeSpec("SystemSettings", []byte(input), &spec))
		if e.Code != CodeFieldNotApplyable || !strings.Contains(e.Reason, label) {
			t.Errorf("%s 应当按%s拒收并点名，得到 %v", input, label, e)
		}
	}
	if e := codeOf(t, DecodeSpec("SystemSettings", []byte(`{"master_url":"https://x"}`), &spec)); !strings.Contains(e.Reason, "master_url") {
		t.Errorf("reason 应当点名 master_url，得到 %v", e)
	}
	if got, _ := info.RejectReason("user_quota_override"); got != "" {
		t.Errorf("user_quota_override 是日常运维，不该在拒收清单里，得到 %q", got)
	}
	if err := DecodeSpec("SystemSettings", []byte(`{"branding_site_title":"百宝袋","dashboard_refresh_interval_ms":3000,"probe_disguise_server_ids":[1,2]}`), &spec); err != nil {
		t.Fatalf("日常运维的 key 应当能解码：%v", err)
	}
	if spec.BrandingSiteTitle != "百宝袋" || spec.DashboardRefreshIntervalMs != 3000 || string(spec.ProbeDisguiseServerIds) != "[1,2]" {
		t.Errorf("解码结果不对：%+v", spec)
	}
	spec.ProbeExternalTokenSha256 = "deadbeefdeadbeefdeadbeefdeadbeef"
	out, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "deadbeef") {
		t.Errorf("外置探针令牌哈希应当打码：%s", out)
	}
	var status SystemSettingsStatus
	status.TurnstileSecretKey = "0x-turnstile-secret"
	status.TgbotToken = "123:bot-secret"
	out, _ = json.Marshal(status)
	if strings.Contains(string(out), "turnstile-secret") || strings.Contains(string(out), "bot-secret") {
		t.Errorf("Turnstile 密钥与 TG 机器人 token 应当打码：%s", out)
	}
}
