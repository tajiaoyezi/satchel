package v1

import (
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
