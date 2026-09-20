package rest

import (
	"net/http"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/middleware/authz"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-rest-api「列表分页」：显式 limit=0 与越界都 400，不换成默认值。
func TestLimitZeroRejected(t *testing.T) {
	e := &echo{}
	h := NewHandler(testTable(t), e, nil)
	for _, q := range []string{"limit=0", "limit=-5", "limit=501"} {
		code, fields := do(t, h, "GET", "/api/v1/audit?"+q, "", nil)
		if code != 400 || errorOf(t, fields).Code != v1.CodeBadRequest || !strings.Contains(errorOf(t, fields).Reason, "limit") {
			t.Errorf("%s 应当 400 bad_request：%d %v", q, code, fields)
		}
	}
	if code, _ := do(t, h, "GET", "/api/v1/audit", "", nil); code != 200 || e.last.Page.Limit != 0 {
		t.Fatalf("没给 limit 时交给执行链取默认值：%d %+v", code, e.last.Page)
	}
}

// identity 把身份塞进请求 ctx，模拟 authn。
func identity(id v1.Identity, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(v1.WithIdentity(r.Context(), id)))
	})
}

// master-identity-and-authz「confirm 是字符串」经真实的 authz：请求体里 "confirm": true 是 428 confirm_required。
func TestBooleanConfirmThroughAuthz(t *testing.T) {
	table := testTable(t)
	e := &echo{}
	h := identity(v1.LocalAdmin("root"), NewHandler(table, authz.Wrap(table, nil, e), nil))
	js := map[string]string{"Content-Type": "application/json"}
	for _, body := range []string{`{"confirm":true}`, `{"confirm":1}`, `{}`, `{"confirm":"bob"}`} {
		code, fields := do(t, h, "POST", "/api/v1/demo/remove/alice", body, js)
		if code != 428 || errorOf(t, fields).Code != v1.CodeConfirmRequired {
			t.Errorf("%s 应当 428 confirm_required：%d %v", body, code, fields)
		}
	}
	if code, _ := do(t, h, "POST", "/api/v1/demo/remove/alice", `{"confirm":"alice"}`, js); code != 200 || e.last.Confirm != "alice" {
		t.Fatalf("字符串 confirm 填对应当执行：%d %+v", code, e.last)
	}
	_ = command.ConfirmObject
}
