package authn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/satchel/satchel/internal/service/auth"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// fakeResolver 认一个固定令牌。
type fakeResolver struct {
	token string
	id    v1.Identity
	err   error
}

func (f fakeResolver) Resolve(_ context.Context, token string) (v1.Identity, string, bool, error) {
	if f.err != nil {
		return v1.Identity{}, "", false, f.err
	}
	if token == f.token {
		return f.id, "hash-of-" + token, true, nil
	}
	return v1.Identity{}, "", false, nil
}

func echoHandler(resolver SessionResolver) http.Handler {
	return Middleware(resolver, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"identity": v1.IdentityFrom(r.Context()),
			"source":   v1.CredentialSourceFrom(r.Context()),
			"hash":     auth.SessionHashFrom(r.Context()),
		})
	}))
}

func call(t *testing.T, h http.Handler, cookie string, peerUID *uint32) (v1.Identity, v1.CredentialSource, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/whoami", nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	}
	if peerUID != nil {
		req = req.WithContext(context.WithValue(req.Context(), peerKey{}, peer{uid: *peerUID}))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		Identity v1.Identity         `json:"identity"`
		Source   v1.CredentialSource `json:"source"`
		Hash     string              `json:"hash"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Identity, out.Source, out.Hash
}

// master-web-session「会话到身份」：cookie 认出用户；坏 cookie 与解析失败都 anonymous；socket 优先于 cookie。
func TestSessionCookie(t *testing.T) {
	user := v1.Identity{Actor: "alice", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}, Danger: []v1.Danger{}}
	h := echoHandler(fakeResolver{token: "good", id: user})
	id, src, hash := call(t, h, "good", nil)
	if id.ActorKind != v1.ActorUser || id.Actor != "alice" || src != v1.SourceSession || hash != "hash-of-good" {
		t.Fatalf("有效 cookie 应当认出用户并记来源与哈希：%+v %s %s", id, src, hash)
	}
	if id, src, _ := call(t, h, "bad", nil); !id.IsAnonymous() || src != v1.SourceNone {
		t.Fatalf("坏 cookie 应当 anonymous：%+v %s", id, src)
	}
	if id, _, _ := call(t, h, "", nil); !id.IsAnonymous() {
		t.Fatalf("没 cookie 应当 anonymous：%+v", id)
	}
	failing := echoHandler(fakeResolver{token: "good", id: user, err: errors.New("db down")})
	if id, _, _ := call(t, failing, "good", nil); !id.IsAnonymous() {
		t.Fatalf("解析出错应当 anonymous：%+v", id)
	}
	root := uint32(0)
	if id, src, hash := call(t, h, "good", &root); id.ActorKind != v1.ActorLocalAdmin || src != v1.SourceSocket || hash != "" {
		t.Fatalf("socket 对端凭据应当优先于 cookie：%+v %s %q", id, src, hash)
	}
	if id, _, _ := call(t, echoHandler(nil), "good", nil); !id.IsAnonymous() {
		t.Fatal("没有 resolver 时不认会话")
	}
}
