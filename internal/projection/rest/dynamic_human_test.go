package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/satchel/satchel/internal/command"
)

func TestDynamicHumanAcceptsVerifyIntoInvocationOnly(t *testing.T) {
	tbl := command.MustNew(
		&command.Command{
			Path:         []string{"demo", "cond"},
			Summary:      "条件人类专属测试命令",
			Class:        command.ClassAction,
			DynamicHuman: true,
			Flags:        []command.Flag{{Name: "target", Type: command.TypeString}},
		},
	)
	var gotInv *command.Invocation
	h := NewHandler(tbl, command.RunnerFunc(func(_ context.Context, inv *command.Invocation) (any, error) {
		gotInv = inv
		return map[string]any{"ok": true}, nil
	}), nil)

	// 1. 不带 verify-*：远程节点场景正常放行且 inv.Verify 为空。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/demo/cond", bytes.NewBufferString(`{"target":"hk-1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || gotInv == nil || gotInv.Verify != nil || gotInv.String("target", "") != "hk-1" {
		t.Fatalf("不带 verify-* 请求异常：code=%d inv=%+v body=%s", rec.Code, gotInv, rec.Body.String())
	}

	// 2. 携带 verify-*：仅进入 inv.Verify，绝不进入 inv.Flags（避免进审计摘要）。
	gotInv = nil
	body, _ := json.Marshal(map[string]any{
		"target":          "local-1",
		"verify-user":     "admin",
		"verify-password": "top-secret-password",
		"verify-code":     "123456",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/demo/cond", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || gotInv == nil || gotInv.Verify == nil {
		t.Fatalf("带 verify-* 请求异常：code=%d inv=%+v body=%s", rec.Code, gotInv, rec.Body.String())
	}
	if gotInv.Verify.User != "admin" || gotInv.Verify.Password != "top-secret-password" || gotInv.Verify.Code != "123456" {
		t.Fatalf("inv.Verify 解析不对：%+v", gotInv.Verify)
	}
	for _, k := range command.VerifyFlags {
		if _, leaked := gotInv.Flags[k]; leaked {
			t.Fatalf("verify 字段 %s 绝不能进入 inv.Flags：%v", k, gotInv.Flags)
		}
	}
}
