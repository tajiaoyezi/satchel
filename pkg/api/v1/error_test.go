package v1

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestExitCodeOf(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, ExitOK},
		{errors.New("plain"), ExitFailure},
		{New(CodeDatabase, "x"), ExitFailure},
		{New(CodeBadRequest, "x"), ExitUsage},
		{New(CodeUnauthenticated, "x"), ExitUnauthenticated},
		{New(CodeForbidden, "x"), ExitForbidden},
		{New(CodeNotFound, "x"), ExitNotFound},
		{New(CodeVersionConflict, "x"), ExitVersionConflict},
		{New(CodeConfirmRequired, "x"), ExitConfirmRequired},
		{New(CodePartialFailure, "x"), ExitPartialFailure},
		{New(CodeHumanRequired, "x"), ExitHumanRequired},
		{New(CodeConflict, "x"), ExitFailure},
		{New(CodeSchemaMismatch, "x"), ExitFailure},
		{New(CodeConfig, "x"), ExitFailure},
		{errors.Join(errors.New("outer"), New(CodeNotFound, "x")), ExitNotFound},
	}
	for _, tc := range cases {
		if got := ExitCodeOf(tc.err); got != tc.want {
			t.Errorf("ExitCodeOf(%v) = %d，想要 %d", tc.err, got, tc.want)
		}
	}
}

func TestWrapKeepsDriverTextOutOfReason(t *testing.T) {
	driver := errors.New("SQLITE_CONSTRAINT_UNIQUE: UNIQUE constraint failed: notify_channels.name")
	err := Wrap(CodeDatabase, "数据库操作失败", driver)
	if strings.Contains(err.Reason, "SQLITE") || strings.Contains(err.Error(), "SQLITE") {
		t.Fatalf("reason 里不该出现驱动文本：%q", err.Reason)
	}
	if !errors.Is(err, driver) {
		t.Fatal("原始错误应当在 Unwrap 链里")
	}
}

func TestErrorJSONHasFourFields(t *testing.T) {
	err := New(CodeVersionConflict, "对象已被别人改过").WithState("resourceVersion", int64(2)).WithNext("重新读取后再改")
	data, _ := json.Marshal(err)
	var got map[string]json.RawMessage
	if e := json.Unmarshal(data, &got); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"code", "reason", "state", "next"} {
		if _, ok := got[key]; !ok {
			t.Errorf("JSON 缺少 %s：%s", key, data)
		}
	}
	if len(got) != 4 {
		t.Errorf("JSON 应当恰好四个字段：%s", data)
	}
	bare, _ := json.Marshal(New(CodeNotFound, "没有这个对象"))
	if !strings.Contains(string(bare), `"state":{}`) || !strings.Contains(string(bare), `"next":""`) {
		t.Errorf("state 为空时应当是 {} 而不是 null，next 为空时也要出现：%s", bare)
	}
}
