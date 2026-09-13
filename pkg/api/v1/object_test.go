package v1

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func sampleRule() Object[AutomationRuleSpec, AutomationRuleStatus] {
	ts := time.Date(2026, 9, 11, 8, 30, 0, 0, time.UTC)
	return Object[AutomationRuleSpec, AutomationRuleStatus]{
		APIVersion: APIVersion,
		Kind:       "AutomationRule",
		Metadata:   Metadata{ID: 7, Name: "throttle-abusers", ResourceVersion: 3, CreatedAt: ts, UpdatedAt: ts},
		Spec: AutomationRuleSpec{
			Name: "throttle-abusers", Trigger: "device_limit_exceeded",
			Condition: json.RawMessage(`{"level":"critical"}`), Action: "throttle",
			MaxTargets: ptr[int64](10), DeadlineMinutes: ptr[int64](30), Enabled: true,
		},
		Status: AutomationRuleStatus{ProposedBy: "ai", Status: "pending"},
	}
}

func TestObjectRoundTrip(t *testing.T) {
	obj := sampleRule()
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	if len(top) != 5 {
		t.Fatalf("顶层应当恰好五个键，得到 %d 个：%s", len(top), data)
	}
	for _, key := range []string{"apiVersion", "kind", "metadata", "spec", "status"} {
		if _, ok := top[key]; !ok {
			t.Fatalf("顶层缺少 %s：%s", key, data)
		}
	}
	if string(top["apiVersion"]) != `"satchel/v1"` {
		t.Fatalf("apiVersion = %s", top["apiVersion"])
	}
	got, err := Decode[AutomationRuleSpec, AutomationRuleStatus](data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got, obj) {
		t.Fatalf("往返后对象变了：\n得到 %+v\n想要 %+v", *got, obj)
	}
}

func TestDecodeRejectsOtherAPIVersion(t *testing.T) {
	_, err := Decode[AutomationRuleSpec, AutomationRuleStatus]([]byte(`{"apiVersion":"satchel/v2","kind":"AutomationRule"}`))
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeUnsupportedAPIVersion {
		t.Fatalf("想要 unsupported_api_version，得到 %v", err)
	}
	// 对象内容的错误不是「命令敲错了」，退出码 1。
	if ExitCodeOf(err) != ExitFailure {
		t.Fatalf("退出码应当是 %d，得到 %d", ExitFailure, ExitCodeOf(err))
	}
}

func TestDecodeRejectsExtraTopLevelKey(t *testing.T) {
	_, err := Decode[json.RawMessage, json.RawMessage]([]byte(`{"apiVersion":"satchel/v1","kind":"Task","extra":1}`))
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeBadRequest || !strings.Contains(e.Reason, "extra") {
		t.Fatalf("想要 bad_request 并点名 extra，得到 %v", err)
	}
}

func TestDecodeRequiresEnvelopeKeys(t *testing.T) {
	cases := map[string]string{
		`{"kind":"Task","metadata":{"name":"t"},"spec":{}}`:                                                "apiVersion",
		`{"apiVersion":"satchel/v1","metadata":{"name":"t"},"spec":{}}`:                                    "kind",
		`{"apiVersion":"satchel/v1","kind":"Task","spec":{}}`:                                              "metadata",
		`{"apiVersion":"satchel/v1","kind":"Task","metadata":{"name":"t"}}`:                                "spec",
		`{"apiVersion":"satchel/v1","kind":"Task","metadata":{"name":"t","resource_version":3},"spec":{}}`: "resource_version",
	}
	for in, want := range cases {
		_, err := Decode[json.RawMessage, json.RawMessage]([]byte(in))
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeBadRequest || !strings.Contains(e.Reason, want) {
			t.Errorf("%s 应当是 bad_request 并点名 %s，得到 %v", in, want, err)
		}
	}
	for _, in := range []string{`[1]`, `null`, `"x"`, ``,
		`{"apiVersion":"satchel/v1","kind":"Task","metadata":null,"spec":{}}`,
		`{"apiVersion":"satchel/v1","kind":"Task","metadata":{"name":"t"},"spec":null}`,
		`{"apiVersion":"satchel/v1","kind":"Task","metadata":{"name":"t"},"spec":{},"status":[]}`,
	} {
		if _, err := Decode[json.RawMessage, json.RawMessage]([]byte(in)); err == nil {
			t.Errorf("%q 应当被拒绝", in)
		}
	}
}

func TestDecodeAllowsMissingStatus(t *testing.T) {
	obj, err := Decode[TaskSpec, TaskStatus]([]byte(`{"apiVersion":"satchel/v1","kind":"Task","metadata":{"name":"t","resourceVersion":1},"spec":{"title":"x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if obj.Spec.Title != "x" || obj.Metadata.ResourceVersion != 1 || obj.Status != (TaskStatus{}) {
		t.Fatalf("解码结果不对：%+v", obj)
	}
}
