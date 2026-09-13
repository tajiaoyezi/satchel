package v1

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestEveryKindHasClass(t *testing.T) {
	kinds := Kinds()
	if len(kinds) == 0 {
		t.Fatal("kind 清单是空的")
	}
	for _, k := range kinds {
		if k.Class == "" {
			t.Errorf("kind %s 没有类别", k.Name)
		}
	}
}

func TestLookupTask(t *testing.T) {
	task, ok := Lookup("Task")
	if !ok {
		t.Fatal("找不到 kind Task")
	}
	if task.Class != ClassAction {
		t.Errorf("Task 的类别应当是动作类，得到 %s", task.Class)
	}
	if !contains(task.StatusFields, "expires_at") {
		t.Errorf("Task 的 status 字段应当含 expires_at，得到 %v", task.StatusFields)
	}
	if !contains(task.SpecFields, "title") {
		t.Errorf("Task 的 spec 字段应当含 title，得到 %v", task.SpecFields)
	}
	if _, ok := Lookup("NoSuchKind"); ok {
		t.Error("不存在的 kind 不该查到")
	}
}

func TestKindsOf(t *testing.T) {
	var names []string
	for _, k := range KindsOf(ClassConfig) {
		names = append(names, string(k.Name))
	}
	if !contains(names, "AutomationRule") || !contains(names, "NotifyChannel") {
		t.Errorf("配置类应当含 AutomationRule 与 NotifyChannel，得到 %v", names)
	}
}

func TestRejectionListCoversEveryNonSpecField(t *testing.T) {
	for _, k := range Kinds() {
		for _, f := range k.StatusFields {
			if _, ok := k.RejectReason(f); !ok {
				t.Errorf("kind %s 的 status 字段 %s 不在拒收清单里", k.Name, f)
			}
		}
		for _, f := range k.SpecFields {
			if _, ok := k.RejectReason(f); ok {
				t.Errorf("kind %s 的 spec 字段 %s 不该在拒收清单里", k.Name, f)
			}
		}
	}
	rule, _ := Lookup("AutomationRule")
	for _, f := range []string{"id", "resource_version", "deleted_at", "created_at"} {
		if _, ok := rule.RejectReason(f); !ok {
			t.Errorf("元数据字段 %s 应当在拒收清单里", f)
		}
	}
}

func codeOf(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("想要 *v1.Error，得到 %v", err)
	}
	return e
}

func TestDecodeSpecRejectsStatusField(t *testing.T) {
	var spec AlertSpec
	err := DecodeSpec("Alert", []byte(`{"category":"server_offline","occurrence_count":3}`), &spec)
	e := codeOf(t, err)
	if e.Code != CodeFieldNotApplyable || !strings.Contains(e.Reason, "occurrence_count") {
		t.Fatalf("想要 field_not_applyable 并点名 occurrence_count，得到 %v", err)
	}
}

func TestDecodeSpecRejectsHumanField(t *testing.T) {
	var spec AutomationRuleSpec
	err := DecodeSpec("AutomationRule", []byte(`{"name":"r","approved_by":"alice"}`), &spec)
	if e := codeOf(t, err); e.Code != CodeFieldNotApplyable || !strings.Contains(e.Reason, "approved_by") {
		t.Fatalf("想要 field_not_applyable 并点名 approved_by，得到 %v", err)
	}
}

func TestDecodeSpecRejectsUnknownField(t *testing.T) {
	var spec TaskSpec
	err := DecodeSpec("Task", []byte(`{"title":"x","colour":"red"}`), &spec)
	if e := codeOf(t, err); e.Code != CodeUnknownField || !strings.Contains(e.Reason, "colour") {
		t.Fatalf("想要 unknown_field 并点名 colour，得到 %v", err)
	}
}

func TestDecodeSpecRejectsNonObject(t *testing.T) {
	var spec TaskSpec
	for _, in := range []string{`null`, `[]`, `"x"`, `1`, ``} {
		err := DecodeSpec("Task", []byte(in), &spec)
		if e := codeOf(t, err); e.Code != CodeBadRequest {
			t.Errorf("spec 为 %q 应当是 bad_request，得到 %v", in, err)
		}
	}
}

func TestMaskedFields(t *testing.T) {
	ch, ok := Lookup("NotifyChannel")
	if !ok || !contains(ch.MaskedFields, "secret") {
		t.Fatalf("NotifyChannel 的打码清单应当含 secret，得到 %v", ch.MaskedFields)
	}
	for _, k := range Kinds() {
		for _, f := range k.MaskedFields {
			if contains(k.SpecFields, f) {
				continue
			}
			if !contains(k.StatusFields, f) {
				t.Errorf("kind %s 的打码字段 %s 既不在 spec 也不在 status 里", k.Name, f)
			}
		}
	}
}

// 动作类 kind 可以没有 spec：ApiToken 的可写列全是人类专属，任何字段都不能经 apply 写。
func TestApiTokenHasNoSpec(t *testing.T) {
	tok, ok := Lookup("ApiToken")
	if !ok || tok.Class != ClassAction || len(tok.SpecFields) != 0 {
		t.Fatalf("ApiToken 应当是动作类且没有 spec 字段，得到 %+v", tok)
	}
	for _, f := range []string{"scopes", "token_hash", "last_used_at"} {
		if _, rejected := tok.RejectReason(f); !rejected {
			t.Errorf("ApiToken 的 %s 应当在拒收清单里", f)
		}
	}
	if !contains(tok.MaskedFields, "token_hash") {
		t.Errorf("ApiToken 的打码清单应当含 token_hash，得到 %v", tok.MaskedFields)
	}
	var spec ApiTokenSpec
	if e := codeOf(t, DecodeSpec("ApiToken", []byte(`{"name":"x"}`), &spec)); e.Code != CodeFieldNotApplyable {
		t.Fatalf("ApiToken 的 name 应当报 field_not_applyable，得到 %v", e)
	}
	if err := DecodeSpec("ApiToken", []byte(`{}`), &spec); err != nil {
		t.Fatalf("空 spec 应当能解：%v", err)
	}
}

func TestImmutableFields(t *testing.T) {
	user, _ := Lookup("User")
	if strings.Join(user.ImmutableFields, ",") != "username" {
		t.Fatalf("User 的不可改字段应当只有 username，得到 %v", user.ImmutableFields)
	}
	for _, k := range Kinds() {
		for _, f := range k.ImmutableFields {
			if !contains(k.SpecFields, f) || !k.Immutable(f) {
				t.Errorf("kind %s 的不可改字段 %s 必须是 spec 字段且 Immutable 返回 true", k.Name, f)
			}
		}
	}
	if user.Immutable("email") {
		t.Error("email 不该是不可改字段")
	}
}

// 凭据类的列不在任何 kind 的 spec 里。
func TestCredentialsNeverInSpec(t *testing.T) {
	credentials := map[Kind][]string{
		"Server":         {"token", "agent_token", "pull_token"},
		"User":           {"password_hash", "totp_secret", "recovery_codes"},
		"SystemSettings": {"telegram_bot_token"},
	}
	for kind, cols := range credentials {
		info, ok := Lookup(kind)
		if !ok {
			t.Fatalf("没有 kind %s", kind)
		}
		for _, c := range cols {
			if contains(info.SpecFields, c) {
				t.Errorf("%s 的 spec 不该含 %s", kind, c)
			}
			if !contains(info.MaskedFields, c) {
				t.Errorf("%s 的打码清单应当含 %s", kind, c)
			}
		}
	}
	if kinds := Kinds(); len(kinds) != 32 {
		t.Fatalf("kind 应当恰好 32 个，得到 %d", len(kinds))
	}
}

// 主控设置类 kind：日常运维档是 spec，七组与主控自身类的设置项在 status 字段集合里并按分档拒收。
func TestSystemSettingsTiers(t *testing.T) {
	info, ok := Lookup("SystemSettings")
	if !ok || info.Class != ClassMasterSettings {
		t.Fatalf("SystemSettings 应当是主控设置类，得到 %+v", info)
	}
	for _, f := range []string{"heartbeat_interval", "notify_login", "enable_short_link"} {
		if !contains(info.SpecFields, f) {
			t.Errorf("SystemSettings 的 spec 应当含 %s", f)
		}
	}
	for _, f := range []string{"telegram_bot_token", "telegram_chat_id", "silent_mode", "silent_mode_timeout"} {
		if contains(info.SpecFields, f) || !contains(info.StatusFields, f) {
			t.Errorf("SystemSettings 的 %s 应当在 status 字段集合里而不在 spec 里", f)
		}
	}
	if got, _ := info.RejectReason("silent_mode"); got != "human" {
		t.Errorf("静默模式是第 05 章七组「门」里的设置，应当按人类专属拒收，得到 %q", got)
	}
	// 列上一把（通知推送的 bot token）加键值表三把（Turnstile 密钥、TG 管理机器人 token、外置探针令牌哈希）。
	if strings.Join(info.MaskedFields, ",") != "telegram_bot_token,turnstile_secret_key,tgbot_token,probe_external_token_sha256" {
		t.Errorf("SystemSettings 的打码清单不对，得到 %v", info.MaskedFields)
	}
	if got, _ := info.RejectReason("telegram_bot_token"); got != "human" {
		t.Errorf("telegram_bot_token 应当按人类专属拒收，得到 %q", got)
	}
	if got, _ := info.RejectReason("telegram_chat_id"); got != "master_self" {
		t.Errorf("telegram_chat_id 应当按主控自身类拒收，得到 %q", got)
	}
	var spec SystemSettingsSpec
	if e := codeOf(t, DecodeSpec("SystemSettings", []byte(`{"telegram_bot_token":"x"}`), &spec)); e.Code != CodeFieldNotApplyable {
		t.Fatalf("设置写接口带七组字段应当报 field_not_applyable，得到 %v", e)
	}
	var status SystemSettingsStatus
	status.TelegramBotToken = "123456:secret-bot-token"
	out, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "secret-bot-token") || !strings.Contains(string(out), Redacted) {
		t.Errorf("序列化的 SystemSettings status 应当把 bot token 打码，得到 %s", out)
	}
}

func TestDecodeSpecAcceptsValidSpec(t *testing.T) {
	var spec TaskSpec
	if err := DecodeSpec("Task", []byte(`{"title":"看看东京那台","source":"admin","dedup_key":""}`), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Title != "看看东京那台" || spec.Source != "admin" {
		t.Fatalf("解码结果不对：%+v", spec)
	}
	if err := DecodeSpec("NoSuchKind", []byte(`{}`), &spec); codeOf(t, err).Code != CodeBadRequest {
		t.Fatal("未知 kind 应当是 bad_request")
	}
}
