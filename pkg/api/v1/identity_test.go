package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// resource-model「Structured error shape」：错误码到 HTTP 状态码的折算表，每个码都有状态码。
func TestHTTPStatusOf(t *testing.T) {
	cases := map[Code]int{
		CodeUsage:               http.StatusBadRequest,
		CodeBadRequest:          http.StatusBadRequest,
		CodeConfig:              http.StatusBadRequest,
		CodeUnauthenticated:     http.StatusUnauthorized,
		CodeForbidden:           http.StatusForbidden,
		CodeHumanRequired:       http.StatusForbidden,
		CodeNotFound:            http.StatusNotFound,
		CodeVersionConflict:     http.StatusConflict,
		CodeNameTaken:           http.StatusConflict,
		CodeConfirmRequired:     http.StatusPreconditionRequired,
		CodeUnavailable:         http.StatusServiceUnavailable,
		CodeInternal:            http.StatusInternalServerError,
		CodePartialFailure:      http.StatusInternalServerError,
		CodeUnsupportedPlatform: http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := HTTPStatusOf(code); got != want {
			t.Errorf("HTTPStatusOf(%s) = %d，想要 %d", code, got, want)
		}
	}
	for _, code := range Codes() {
		if status := HTTPStatusOf(code); status < 400 || status > 599 {
			t.Errorf("错误码 %s 折算出的状态码 %d 不是 4xx / 5xx", code, status)
		}
	}
	for code := range httpStatuses {
		if !containsCode(allCodes, code) {
			t.Errorf("折算表里的 %s 不在 allCodes 里", code)
		}
	}
	for code := range exitCodes {
		if !containsCode(allCodes, code) {
			t.Errorf("退出码表里的 %s 不在 allCodes 里", code)
		}
	}
	if HTTPStatusOf(Code("made_up")) != http.StatusInternalServerError {
		t.Error("没登记的错误码应当是 500")
	}
}

func containsCode(list []Code, c Code) bool {
	for _, v := range list {
		if v == c {
			return true
		}
	}
	return false
}

func TestNewCodesExitOne(t *testing.T) {
	for _, code := range []Code{CodeUnsupportedPlatform, CodeUnavailable} {
		if got := ExitCodeOf(New(code, "x")); got != ExitFailure {
			t.Errorf("%s 的退出码应当是 1，得到 %d", code, got)
		}
	}
}

func TestAsError(t *testing.T) {
	e := New(CodeNotFound, "没有")
	if AsError(e) != e {
		t.Fatal("四字段错误应当原样返回")
	}
	if AsError(errors.Join(errors.New("outer"), e)) != e {
		t.Fatal("包在 Join 里的四字段错误应当被找出来")
	}
	plain := errors.New("driver: boom")
	got := AsError(plain)
	if got.Code != CodeInternal || strings.Contains(got.Reason, "boom") || !errors.Is(got, plain) {
		t.Fatalf("普通错误应当包成 internal、reason 不含原文、原文在 Unwrap 链里：%+v", got)
	}
}

// master-identity-and-authz「身份对象」：形状与 JSON 字段名。
func TestIdentityShape(t *testing.T) {
	id := LocalAdmin("root")
	data, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"actor", "actor_kind", "role", "token_id", "scopes", "danger"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("身份对象缺少 %s：%s", k, data)
		}
	}
	if len(keys) != 6 || string(keys["token_id"]) != "null" {
		t.Fatalf("身份对象应当恰好六个键、token_id 为 null：%s", data)
	}
	if !id.HasScope(ScopeOperate) || !id.HasDanger(DangerExec) || id.IsAnonymous() || !id.IsAdmin() || len(id.Danger) != 6 {
		t.Fatal("本机管理员应当是 admin、有全部 scope 与六个危险类")
	}
	ctx := WithIdentity(context.Background(), id)
	if IdentityFrom(ctx).Actor != "root" || !IdentityFrom(context.Background()).IsAnonymous() {
		t.Fatal("ctx 里的身份取不回来，或没身份时不是 anonymous")
	}
	anon := Anonymous()
	if !anon.IsAnonymous() || anon.HasScope(ScopeRead) || (Identity{}).IsAnonymous() != true {
		t.Fatal("匿名与零值都应当是 anonymous")
	}
	data, _ = json.Marshal(anon)
	if !strings.Contains(string(data), `"scopes":[]`) || !strings.Contains(string(data), `"danger":[]`) {
		t.Fatalf("匿名身份的 scopes 与 danger 应当是空数组而不是 null：%s", data)
	}
}

func TestMarshalOutput(t *testing.T) {
	out, err := MarshalOutput(struct {
		Version string `json:"version"`
	}{"1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["apiVersion"]) != `"`+APIVersion+`"` || string(got["version"]) != `"1.2.3"` {
		t.Fatalf("输出应当带 apiVersion 与原字段：%s", out)
	}
	if _, err := MarshalOutput([]int{1}); err == nil {
		t.Fatal("数组不是对象，应当报错")
	}
	out, err = MarshalOutput(map[string]any(nil))
	if err != nil || !strings.Contains(string(out), "apiVersion") {
		t.Fatalf("nil map 应当编成只带 apiVersion 的对象：%s %v", out, err)
	}
}

// master-identity-and-authz「身份的判定顺序」：无效凭据是单独的一种来源，不是任何一种有效来源。
func TestCredentialSourceInvalid(t *testing.T) {
	ctx := WithCredentialSource(context.Background(), SourceInvalid)
	if CredentialSourceFrom(ctx) != SourceInvalid {
		t.Fatal("无效凭据的来源应当取得回来")
	}
	for _, s := range []CredentialSource{SourceNone, SourceSocket, SourceSession, SourceToken} {
		if s == SourceInvalid {
			t.Fatalf("SourceInvalid 不能与 %q 相同", s)
		}
	}
	if CredentialSourceFrom(context.Background()) != SourceNone {
		t.Fatal("没放来源时应当是 SourceNone")
	}
}
