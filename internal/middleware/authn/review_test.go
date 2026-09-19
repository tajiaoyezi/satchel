package authn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-identity-and-authz「本机管理员的判定」：对端 uid 是 0 或本进程 uid 才是管理员，其它 uid 是 anonymous。
func TestPeerUIDRules(t *testing.T) {
	self := uint32(os.Getuid())
	cases := []struct {
		name string
		uid  uint32
		want v1.ActorKind
	}{
		{"本进程 uid", self, v1.ActorLocalAdmin},
		{"root", 0, v1.ActorLocalAdmin},
		{"别的 uid", self + 1, v1.ActorAnonymous},
		{"很大的 uid", 4000000000, v1.ActorAnonymous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/whoami", nil)
			req = req.WithContext(context.WithValue(req.Context(), peerKey{}, peer{uid: tc.uid}))
			rec := httptest.NewRecorder()
			whoamiHandler().ServeHTTP(rec, req)
			var id v1.Identity
			if err := json.NewDecoder(rec.Body).Decode(&id); err != nil {
				t.Fatal(err)
			}
			if id.ActorKind != tc.want {
				t.Fatalf("uid %d 应当是 %s，得到 %+v", tc.uid, tc.want, id)
			}
			if tc.want == v1.ActorAnonymous && (id.Actor != "" || len(id.Scopes) != 0 || len(id.Danger) != 0 || id.Role != "") {
				t.Fatalf("anonymous 不该带任何权限：%+v", id)
			}
		})
	}
	// 没有对端凭据（TCP）的请求，即使带上自称的头也是 anonymous。
	req := httptest.NewRequest("GET", "/whoami", nil)
	req.Header.Set("Authorization", "Bearer abc")
	rec := httptest.NewRecorder()
	whoamiHandler().ServeHTTP(rec, req)
	var id v1.Identity
	_ = json.NewDecoder(rec.Body).Decode(&id)
	if !id.IsAnonymous() {
		t.Fatalf("没有对端凭据应当 anonymous：%+v", id)
	}
	_ = http.StatusOK
}
